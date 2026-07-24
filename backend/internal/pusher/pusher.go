// Package pusher is a thin WebSocket proxy between the browser and NATS Core.
// It handles WebSocket I/O, dummy token validation, session management, and
// NATS forwarding. No game logic.
package pusher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	otelinternal "github.com/lstep/pixeleruv/backend/internal/otel"
	pb "github.com/lstep/pixeleruv/backend/internal/pb"
	"github.com/lstep/pixeleruv/backend/internal/version"
	"google.golang.org/protobuf/proto"
)

// PingInterval is how often the pusher sends a WebSocket protocol-level ping
// to keep idle connections alive. coder/websocket does not auto-ping, so
// without this an idle player (no input, no replication traffic) has zero
// bytes flowing in either direction and the TCP connection silently dies
// after long idle periods / sleep / network changes. The browser auto-
// responds to protocol pings with a pong, so no client-side keepalive code is
// needed. Overridable (mainly by tests) to keep the regression test fast.
var PingInterval = 30 * time.Second

type Server struct {
	wsAddr     string
	nc         *nats.Conn
	logger     *slog.Logger
	tracer     trace.Tracer
	auth       *AuthValidator
	sessions   sync.Map // client_id -> *session
	startTime  time.Time
	healthMu   sync.Mutex
	healthMap  map[string]*HealthEntry // service name -> latest health
}

// HealthEntry is the health status of one backend service, collected from the
// "healthz" NATS subject. The pusher aggregates these and serves them via the
// HTTP /healthz endpoint.
type HealthEntry struct {
	Service  string          `json:"service"`
	Status   string          `json:"status"`
	Version  string          `json:"version"`
	Uptime   string          `json:"uptime"`
	Extras   json.RawMessage `json:"extras,omitempty"`
	LastSeen time.Time       `json:"last_seen"`
}

type session struct {
	clientID  string
	conn      *websocket.Conn
	sub       *nats.Subscription
	avSub     *nats.Subscription
	chatSub   *nats.Subscription
	adminSub  *nats.Subscription
	recSub    *nats.Subscription
	recActSub *nats.Subscription
	pingSub   *nats.Subscription
	closeOnce sync.Once
}

func New(wsAddr, natsURL, pbAPIURL string, logger *slog.Logger) (*Server, error) {
	nc, err := nats.Connect(natsURL,
		nats.Name("pusher"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	var auth *AuthValidator
	if pbAPIURL != "" {
		auth = NewAuthValidator(pbAPIURL)
	}
	srv := &Server{
		wsAddr:    wsAddr,
		nc:        nc,
		logger:    logger,
		tracer:    otel.Tracer("pusher"),
		auth:      auth,
		startTime: time.Now(),
		healthMap: make(map[string]*HealthEntry),
	}

	// healthz — every backend service (pusher, worldsim, extensions)
	// publishes a health JSON to this subject every 10 seconds. The pusher
	// aggregates them into healthMap and serves the result via HTTP /healthz.
	if _, err := nc.Subscribe("healthz", func(m *nats.Msg) {
		var entry HealthEntry
		if err := json.Unmarshal(m.Data, &entry); err != nil {
			srv.logger.Warn("healthz unmarshal", "err", err)
			return
		}
		entry.LastSeen = time.Now()
		srv.healthMu.Lock()
		srv.healthMap[entry.Service] = &entry
		srv.healthMu.Unlock()
	}); err != nil {
		return nil, fmt.Errorf("subscribe healthz: %w", err)
	}

	// chat.broadcast — worldsim publishes a fully-marshaled ServerFrame
	// (ChatMessageFrame) here for global chat. Fan out the raw bytes to
	// every active session. See documentation/plans/2026-07-07-chat-design.md.
	if _, err := nc.Subscribe("chat.broadcast", func(m *nats.Msg) {
		srv.sessions.Range(func(_, v any) bool {
			sess := v.(*session)
			writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := sess.conn.Write(writeCtx, websocket.MessageBinary, m.Data); err != nil {
				srv.logger.Warn("ws write chat broadcast", "client", sess.clientID, "err", err)
			}
			return true
		})
	}); err != nil {
		return nil, fmt.Errorf("subscribe chat.broadcast: %w", err)
	}

	// client.*.force_close — worldsim publishes a marshaled ServerFrame
	// (AuthResult with kicked=true) here when a player is kicked (admin
	// kick or dual-connect displacement). The pusher forwards the raw
	// bytes to the target session's WebSocket, then closes the connection
	// so the browser shows the "kicked" overlay and stops reconnecting.
	if _, err := nc.Subscribe("client.*.force_close", func(m *nats.Msg) {
		clientID := subjectClientID(m.Subject, "force_close")
		val, ok := srv.sessions.Load(clientID)
		if !ok {
			srv.logger.Warn("force_close: session not found", "client", clientID)
			return
		}
		sess := val.(*session)
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sess.conn.Write(writeCtx, websocket.MessageBinary, m.Data); err != nil {
			srv.logger.Warn("force_close ws write", "client", clientID, "err", err)
		}
		sess.closeOnce.Do(func() {
			sess.conn.Close(websocket.StatusPolicyViolation, "kicked")
		})
		srv.logger.Info("force_close: closed session", "client", clientID)
	}); err != nil {
		return nil, fmt.Errorf("subscribe client.*.force_close: %w", err)
	}

	return srv, nil
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/healthz", s.handleHealthz)

	srv := &http.Server{Addr: s.wsAddr, Handler: mux}

	// Publish the pusher's own health to "healthz" every 10 seconds.
	go s.startHealthPublisher(ctx)

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
		s.nc.Close()
	}()

	return srv.ListenAndServe()
}

// handleHealthz serves the aggregated health of all backend services as JSON.
// Each service publishes to the "healthz" NATS subject every 10 seconds; the
// pusher collects them into healthMap. Entries older than 30 seconds are
// marked "stale" so consumers can detect dead services.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.healthMu.Lock()
	entries := make([]*HealthEntry, 0, len(s.healthMap))
	now := time.Now()
	for _, e := range s.healthMap {
		if now.Sub(e.LastSeen) > 30*time.Second {
			e.Status = "stale"
		}
		entries = append(entries, e)
	}
	s.healthMu.Unlock()

	// Sort by service name for stable output.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Service < entries[j].Service
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"services": entries,
	})
}

// startHealthPublisher publishes the pusher's own health JSON to the "healthz"
// NATS subject every 10 seconds so other pusher instances (and itself) can
// aggregate it.
func (s *Server) startHealthPublisher(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	s.publishHealth() // publish immediately on startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.publishHealth()
		}
	}
}

func (s *Server) publishHealth() {
	sessionCount := 0
	s.sessions.Range(func(_, _ any) bool {
		sessionCount++
		return true
	})
	health := map[string]any{
		"service": "pusher",
		"status":  "OK",
		"version": version.Version,
		"uptime":  time.Since(s.startTime).Round(time.Second).String(),
		"extras": map[string]any{
			"nats_connected":   s.nc.Status() == nats.CONNECTED,
			"active_sessions":  sessionCount,
		},
	}
	data, err := json.Marshal(health)
	if err != nil {
		s.logger.Warn("healthz marshal", "err", err)
		return
	}
	if err := s.nc.Publish("healthz", data); err != nil {
		s.logger.Warn("healthz publish", "err", err)
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // lite MVP — no origin check
	})
	if err != nil {
		s.logger.Warn("ws accept", "err", err)
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx, span := s.tracer.Start(r.Context(), "pusher.ws.handle")
	defer span.End()

	// Read the first frame — must be AuthFrame.
	typ, data, err := c.Read(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "ws read auth")
		s.logger.Warn("ws read auth", "err", err)
		return
	}
	if typ != websocket.MessageBinary {
		c.Close(websocket.StatusPolicyViolation, "expected binary protobuf frame")
		span.SetStatus(codes.Error, "expected binary frame")
		return
	}

	var cf pb.ClientFrame
	if err := proto.Unmarshal(data, &cf); err != nil {
		c.Close(websocket.StatusPolicyViolation, "bad protobuf")
		span.SetStatus(codes.Error, "bad protobuf")
		return
	}
	auth := cf.GetAuth()
	if auth == nil {
		c.Close(websocket.StatusPolicyViolation, "first frame must be AuthFrame")
		span.SetStatus(codes.Error, "missing auth frame")
		return
	}

	// Parent a server-side auth span to the browser's ws.send_auth span via
	// the traceparent carried in the AuthFrame. This is what links the
	// client-side trace to the backend trace tree.
	authCtx, authSpan := s.tracer.Start(
		otelinternal.ContextFromTraceparent(ctx, auth.GetTraceparent()),
		"pusher.ws.auth",
	)
	defer authSpan.End()

	// Validate the id_token via PocketBase API. If PB_API_URL is not set,
	// fall back to dummy auth for local dev without PocketBase. An empty
	// id_token is an intentional guest connection (no Login performed) and
	// is let through with sub == "" — worldsim treats that as a non-
	// persistent session. A non-empty but invalid/expired token is rejected.
	var sub string
	if s.auth != nil {
		if idToken := auth.GetIdToken(); idToken != "" {
			sub, err = s.auth.ValidateToken(idToken)
			if err != nil {
				authSpan.RecordError(err)
				authSpan.SetStatus(codes.Error, "token validation")
				s.logger.Warn("token validation failed", "err", err)
				audit.Emit(s.nc, "auth.failed", audit.SeverityWarn,
					audit.Actor{IP: clientIP(r)},
					audit.Details{"error": err.Error()},
					"")
				errResult := &pb.ServerFrame{
					Payload: &pb.ServerFrame_AuthResult{
						AuthResult: &pb.AuthResultFrame{Ok: false},
					},
				}
				errBytes, _ := proto.Marshal(errResult)
				c.Write(authCtx, websocket.MessageBinary, errBytes)
				c.Close(websocket.StatusPolicyViolation, "auth failed")
				return
			}
		}
		authSpan.SetAttributes(attribute.String("user.sub", sub))
	} else {
		sub = "dev"
	}

	clientID := generateClientID()
	deviceID := auth.GetDeviceId()
	span.SetAttributes(attribute.String("client.id", clientID), attribute.String("user.sub", sub))
	authSpan.SetAttributes(attribute.String("client.id", clientID), attribute.String("user.sub", sub))
	sess := &session{clientID: clientID, conn: c}

	// Subscribe to replication + control subjects for this client.
	repSub, err := s.nc.Subscribe(fmt.Sprintf("client.%s.replication", clientID), func(m *nats.Msg) {
		// Continue the trace started by worldsim's replication publish.
		rctx, rspan := s.tracer.Start(otelinternal.Extract(authCtx, m), "pusher.ws.write_replication")
		defer rspan.End()
		rspan.SetAttributes(attribute.String("client.id", clientID), attribute.Int("bytes", len(m.Data)))

		writeCtx, cancel := context.WithTimeout(rctx, 5*time.Second)
		defer cancel()
		if err := c.Write(writeCtx, websocket.MessageBinary, m.Data); err != nil {
			rspan.RecordError(err)
			rspan.SetStatus(codes.Error, "ws write replication")
			s.logger.Warn("ws write replication", "client", clientID, "err", err)
		}
	})
	if err != nil {
		s.logger.Warn("nats sub replication", "client", clientID, "err", err)
		return
	}
	sess.sub = repSub

	// Subscribe to A/V token messages from ext-av. These are JSON payloads
	// that we wrap in an AvTokenFrame protobuf and forward to the browser.
	avSub, err := s.nc.Subscribe(fmt.Sprintf("client.%s.av_token", clientID), func(m *nats.Msg) {
		// Parse the JSON payload from ext-av.
		var avMsg struct {
			Action  string   `json:"action"`
			Room    string   `json:"room"`
			Token   string   `json:"token"`
			URL     string   `json:"url"`
			Members []string `json:"members"`
		}
		if err := json.Unmarshal(m.Data, &avMsg); err != nil {
			s.logger.Warn("av_token unmarshal", "client", clientID, "err", err)
			return
		}
		frame := &pb.ServerFrame{
			Payload: &pb.ServerFrame_AvToken{
				AvToken: &pb.AvTokenFrame{
					Room:    avMsg.Room,
					Token:   avMsg.Token,
					Url:     avMsg.URL,
					Action:  avMsg.Action,
					Members: avMsg.Members,
				},
			},
		}
		frameBytes, err := proto.Marshal(frame)
		if err != nil {
			s.logger.Warn("av_token marshal", "client", clientID, "err", err)
			return
		}
		writeCtx, cancel := context.WithTimeout(authCtx, 5*time.Second)
		defer cancel()
		if err := c.Write(writeCtx, websocket.MessageBinary, frameBytes); err != nil {
			s.logger.Warn("ws write av_token", "client", clientID, "err", err)
		}
	})
	if err != nil {
		s.logger.Warn("nats sub av_token", "client", clientID, "err", err)
	} else {
		sess.avSub = avSub
	}

	// Subscribe to per-session chat inbox (proximity chat). Worldsim
	// publishes a fully-marshaled ServerFrame (ChatMessageFrame) here for
	// each recipient of a proximity message. Raw bytes pass through
	// unchanged — same pattern as the replication subscription.
	chatSub, err := s.nc.Subscribe(fmt.Sprintf("client.%s.chat_inbox", clientID), func(m *nats.Msg) {
		writeCtx, cancel := context.WithTimeout(authCtx, 5*time.Second)
		defer cancel()
		if err := c.Write(writeCtx, websocket.MessageBinary, m.Data); err != nil {
			s.logger.Warn("ws write chat_inbox", "client", clientID, "err", err)
		}
	})
	if err != nil {
		s.logger.Warn("nats sub chat_inbox", "client", clientID, "err", err)
	} else {
		sess.chatSub = chatSub
	}

	// Subscribe to per-session ping inbox. Worldsim publishes a
	// fully-marshaled ServerFrame (PlayerPingFrame) here when another player
	// pings this client. Raw bytes pass through unchanged — same pattern as
	// chat_inbox.
	pingSub, err := s.nc.Subscribe(fmt.Sprintf("client.%s.ping_inbox", clientID), func(m *nats.Msg) {
		writeCtx, cancel := context.WithTimeout(authCtx, 5*time.Second)
		defer cancel()
		if err := c.Write(writeCtx, websocket.MessageBinary, m.Data); err != nil {
			s.logger.Warn("ws write ping_inbox", "client", clientID, "err", err)
		}
	})
	if err != nil {
		s.logger.Warn("nats sub ping_inbox", "client", clientID, "err", err)
	} else {
		sess.pingSub = pingSub
	}

	s.sessions.Store(clientID, sess)
	defer func() {
		sess.sub.Unsubscribe()
		if sess.avSub != nil {
			sess.avSub.Unsubscribe()
		}
		if sess.chatSub != nil {
			sess.chatSub.Unsubscribe()
		}
		if sess.adminSub != nil {
			sess.adminSub.Unsubscribe()
		}
		if sess.recSub != nil {
			sess.recSub.Unsubscribe()
		}
		if sess.recActSub != nil {
			sess.recActSub.Unsubscribe()
		}
		if sess.pingSub != nil {
			sess.pingSub.Unsubscribe()
		}
		s.sessions.Delete(clientID)
		s.publishLifecycle(authCtx, "client.disconnected", clientID, sub)
		audit.Emit(s.nc, "client.disconnected", audit.SeverityInfo,
			audit.Actor{Sub: sub, ClientID: clientID},
			nil,
			"")
	}()

	// Publish client.connected so World Sim provisions the entity. Use
	// request-reply to get the actual entity ID, which may differ from
	// "e_"+clientID[2:] when a PocketBase-stored identity exists. The
	// client needs the real entity ID to identify its own avatar. The
	// device_id is threaded so worldsim can check the ban list.
	ip := clientIP(r)
	entityID := ""
	isAdmin := false
	mapID := ""
	mapOptions := ""
	playerOptions := ""
	var mapWarnings []*pb.MapWarning
	mapError := ""
	if reply, err := s.publishLifecycleRequest(authCtx, "client.connected", clientID, sub, ip, deviceID, false); err != nil {
		s.logger.Warn("client.connected request-reply, using default entity ID", "client", clientID, "err", err)
	} else {
		var ar pb.AuthResultFrame
		if err := proto.Unmarshal(reply, &ar); err != nil {
			s.logger.Warn("client.connected reply unmarshal", "client", clientID, "err", err)
		} else {
			// If worldsim detected an active ban, reject the connection.
			if ar.Banned {
				banResult := &pb.ServerFrame{
					Payload: &pb.ServerFrame_AuthResult{
						AuthResult: &pb.AuthResultFrame{
							Ok:        false,
							BanReason: ar.BanReason,
							BanUntil:  ar.BanUntil,
						},
					},
				}
				banBytes, _ := proto.Marshal(banResult)
				c.Write(authCtx, websocket.MessageBinary, banBytes)
				c.Close(websocket.StatusPolicyViolation, "banned")
				s.logger.Info("rejected banned client", "client", clientID, "sub", sub, "ip", ip, "device", deviceID)
				audit.Emit(s.nc, "auth.banned", audit.SeverityWarn,
					audit.Actor{Sub: sub, ClientID: clientID, IP: ip, DeviceID: deviceID},
					audit.Details{"reason": ar.BanReason, "until": ar.BanUntil},
					"")
				return
			}
			// If worldsim detected a dual connection (same logged-in user
			// already active), forward already_connected to the browser so
			// it can ask the user to confirm. If the user agrees, the
			// browser sends a second AuthFrame with force=true; we re-publish
			// client.connected with force=true and worldsim despawns the old
			// session. If the user declines (WS closed or no force), return.
			if ar.AlreadyConnected {
				alreadyResult := &pb.ServerFrame{
					Payload: &pb.ServerFrame_AuthResult{
						AuthResult: &pb.AuthResultFrame{
							Ok:              false,
							AlreadyConnected: true,
						},
					},
				}
				alreadyBytes, _ := proto.Marshal(alreadyResult)
				if err := c.Write(authCtx, websocket.MessageBinary, alreadyBytes); err != nil {
					s.logger.Warn("already_connected ws write", "client", clientID, "err", err)
					return
				}
				// Read the second AuthFrame — the browser either sends
				// force=true or closes the WS.
				typ2, data2, err := c.Read(authCtx)
				if err != nil {
					s.logger.Info("already_connected: user declined (ws closed)", "client", clientID, "err", err)
					return
				}
				if typ2 != websocket.MessageBinary {
					c.Close(websocket.StatusPolicyViolation, "expected binary")
					return
				}
				var cf2 pb.ClientFrame
				if err := proto.Unmarshal(data2, &cf2); err != nil {
					c.Close(websocket.StatusPolicyViolation, "bad protobuf")
					return
				}
				auth2 := cf2.GetAuth()
				if auth2 == nil || !auth2.GetForce() {
					s.logger.Info("already_connected: user declined (no force)", "client", clientID)
					return
				}
				// Re-publish client.connected with force=true.
				reply2, err := s.publishLifecycleRequest(authCtx, "client.connected", clientID, sub, ip, deviceID, true)
				if err != nil {
					s.logger.Warn("client.connected force request-reply failed", "client", clientID, "err", err)
					return
				}
				var ar2 pb.AuthResultFrame
				if err := proto.Unmarshal(reply2, &ar2); err != nil {
					s.logger.Warn("client.connected force reply unmarshal", "client", clientID, "err", err)
					return
				}
				if ar2.Banned {
					banResult := &pb.ServerFrame{
						Payload: &pb.ServerFrame_AuthResult{
							AuthResult: &pb.AuthResultFrame{
								Ok:        false,
								BanReason: ar2.BanReason,
								BanUntil:  ar2.BanUntil,
							},
						},
					}
					banBytes, _ := proto.Marshal(banResult)
					c.Write(authCtx, websocket.MessageBinary, banBytes)
					c.Close(websocket.StatusPolicyViolation, "banned")
					return
				}
				entityID = ar2.EntityId
				isAdmin = ar2.IsAdmin
				mapID = ar2.MapId
				mapOptions = ar2.MapOptions
				playerOptions = ar2.PlayerOptions
				mapWarnings = ar2.MapWarnings
				mapError = ar2.MapError
			} else {
				entityID = ar.EntityId
				isAdmin = ar.IsAdmin
				mapID = ar.MapId
				mapOptions = ar.MapOptions
				playerOptions = ar.PlayerOptions
				mapWarnings = ar.MapWarnings
				mapError = ar.MapError
			}
		}
	}

	// If the client is an admin, subscribe to the admin-only NATS channel.
	// Worldsim publishes AdminInfoFrame data (guest IPs, etc.) here. The
	// pusher forwards raw bytes to the browser. Non-admin sessions never
	// get this subscription, so admin data never reaches their WebSocket.
	if isAdmin {
		adminSub, err := s.nc.Subscribe(fmt.Sprintf("client.%s.admin", clientID), func(m *nats.Msg) {
			writeCtx, cancel := context.WithTimeout(authCtx, 5*time.Second)
			defer cancel()
			if err := c.Write(writeCtx, websocket.MessageBinary, m.Data); err != nil {
				s.logger.Warn("ws write admin info", "client", clientID, "err", err)
			}
		})
		if err != nil {
			s.logger.Warn("nats sub admin", "client", clientID, "err", err)
		} else {
			sess.adminSub = adminSub
		}
	}

	// Subscribe to recording state messages from ext-rec. ext-rec publishes
	// JSON on client.<id>.recording_state (to the host that requested
	// start/stop) and client.<id>.recording_active (to each participant in a
	// recorded room). The pusher wraps each in the matching ServerFrame proto
	// and forwards to the browser.
	recSub, err := s.nc.Subscribe(fmt.Sprintf("client.%s.recording_state", clientID), func(m *nats.Msg) {
		var msg struct {
			Room     string `json:"room"`
			Status   string `json:"status"`
			Target   string `json:"target"`
			EgressID string `json:"egress_id"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(m.Data, &msg); err != nil {
			s.logger.Warn("recording_state unmarshal", "client", clientID, "err", err)
			return
		}
		frame := &pb.ServerFrame{
			Payload: &pb.ServerFrame_RecordingState{
				RecordingState: &pb.RecordingStateFrame{
					Room:     msg.Room,
					Status:   msg.Status,
					Target:   msg.Target,
					EgressId: msg.EgressID,
					Error:    msg.Error,
				},
			},
		}
		frameBytes, err := proto.Marshal(frame)
		if err != nil {
			s.logger.Warn("recording_state marshal", "client", clientID, "err", err)
			return
		}
		writeCtx, cancel := context.WithTimeout(authCtx, 5*time.Second)
		defer cancel()
		if err := c.Write(writeCtx, websocket.MessageBinary, frameBytes); err != nil {
			s.logger.Warn("ws write recording_state", "client", clientID, "err", err)
		}
	})
	if err != nil {
		s.logger.Warn("nats sub recording_state", "client", clientID, "err", err)
	} else {
		sess.recSub = recSub
	}

	recActSub, err := s.nc.Subscribe(fmt.Sprintf("client.%s.recording_active", clientID), func(m *nats.Msg) {
		var msg struct {
			Room   string `json:"room"`
			Active bool   `json:"active"`
			Target string `json:"target"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(m.Data, &msg); err != nil {
			s.logger.Warn("recording_active unmarshal", "client", clientID, "err", err)
			return
		}
		frame := &pb.ServerFrame{
			Payload: &pb.ServerFrame_RecordingActive{
				RecordingActive: &pb.RecordingActiveFrame{
					Room:   msg.Room,
					Active: msg.Active,
					Target: msg.Target,
					Reason: msg.Reason,
				},
			},
		}
		frameBytes, err := proto.Marshal(frame)
		if err != nil {
			s.logger.Warn("recording_active marshal", "client", clientID, "err", err)
			return
		}
		writeCtx, cancel := context.WithTimeout(authCtx, 5*time.Second)
		defer cancel()
		if err := c.Write(writeCtx, websocket.MessageBinary, frameBytes); err != nil {
			s.logger.Warn("ws write recording_active", "client", clientID, "err", err)
		}
	})
	if err != nil {
		s.logger.Warn("nats sub recording_active", "client", clientID, "err", err)
	} else {
		sess.recActSub = recActSub
	}

	// Send AuthResultFrame with the entity ID, map_id, admin flag, and options
	// from worldsim. The client needs the map_id to load the correct map, and
	// the map/player options to apply feature toggles (e.g. day/night, name tag).
	result := &pb.ServerFrame{
		Payload: &pb.ServerFrame_AuthResult{
			AuthResult: &pb.AuthResultFrame{
				Ok:            true,
				ClientId:      clientID,
				EntityId:      entityID,
				MapId:         mapID,
				IsAdmin:       isAdmin,
				MapOptions:    mapOptions,
				PlayerOptions: playerOptions,
				MapWarnings:   mapWarnings,
				MapError:      mapError,
			},
		},
	}
	resultBytes, _ := proto.Marshal(result)
	if err := c.Write(authCtx, websocket.MessageBinary, resultBytes); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "ws write auth result")
		s.logger.Warn("ws write auth result", "client", clientID, "err", err)
		return
	}

	s.logger.Info("client connected", "client", clientID, "entity", entityID)
	audit.Emit(s.nc, "client.connected", audit.SeverityInfo,
		audit.Actor{Sub: sub, EntityID: entityID, ClientID: clientID, IP: ip, DeviceID: deviceID},
		audit.Details{"map": mapID, "is_admin": isAdmin},
		"")

	// Keepalive: send a WebSocket ping every PingInterval. The browser
	// auto-responds with a pong, which keeps idle connections alive and lets
	// us detect dead peers (ping fails → we close → the read loop exits and
	// the session is cleaned up). Runs concurrently with the read loop, as
	// coder/websocket requires for Ping. Exits when ctx is canceled (read
	// loop returned) or a ping fails.
	go s.keepalive(ctx, c, clientID)

	// Main read loop — forward InputFrames to NATS.
	for {
		msgType, msgData, err := c.Read(ctx)
		if err != nil {
			s.logger.Info("client disconnected", "client", clientID, "err", err)
			return
		}
		if msgType != websocket.MessageBinary {
			continue
		}

		var frame pb.ClientFrame
		if err := proto.Unmarshal(msgData, &frame); err != nil {
			continue
		}

		switch p := frame.Payload.(type) {
		case *pb.ClientFrame_Input:
			// Parent to the browser's ws.send_input span via the InputFrame's
			// traceparent, so the full chain is:
			// ws.send_input -> pusher.nats.publish.input -> worldsim.apply_input.
			pctx, pspan := s.tracer.Start(
				otelinternal.ContextFromTraceparent(ctx, p.Input.GetTraceparent()),
				"pusher.nats.publish.input",
			)
			pspan.SetAttributes(attribute.String("client.id", clientID), attribute.Int("input.seq", int(p.Input.GetSeq())))
			inputBytes, _ := proto.Marshal(p.Input)
			subject := fmt.Sprintf("client.%s.input", clientID)
			msg := &nats.Msg{Subject: subject, Data: inputBytes}
			otelinternal.Inject(pctx, msg) // worldsim's apply span will parent to this
			if err := s.nc.PublishMsg(msg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish input")
				s.logger.Warn("nats publish input", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_Ping:
			pong := &pb.ServerFrame{Payload: &pb.ServerFrame_Pong{Pong: &pb.PongFrame{}}}
			pongBytes, _ := proto.Marshal(pong)
			c.Write(ctx, websocket.MessageBinary, pongBytes)
		case *pb.ClientFrame_Chat:
			// Forward chat to worldsim on client.<id>.chat. Worldsim stamps
			// display_name + timestamp and routes to chat.broadcast (global)
			// or per-recipient client.<id>.chat_inbox (proximity). The
			// response comes back on chat.broadcast or this session's
			// chat_inbox subscription — no separate reply subject.
			pctx, pspan := s.tracer.Start(
				otelinternal.ContextFromTraceparent(ctx, p.Chat.GetTraceparent()),
				"pusher.nats.publish.chat",
			)
			pspan.SetAttributes(attribute.String("client.id", clientID), attribute.String("chat.channel", p.Chat.GetChannel()))
			chatBytes, _ := proto.Marshal(p.Chat)
			subject := fmt.Sprintf("client.%s.chat", clientID)
			msg := &nats.Msg{Subject: subject, Data: chatBytes}
			otelinternal.Inject(pctx, msg)
			if err := s.nc.PublishMsg(msg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish chat")
				s.logger.Warn("nats publish chat", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_SetName:
			// Forward name change to worldsim on client.<id>.set_name.
			// Worldsim sanitizes (ASCII printable, max 20 runes), updates
			// Entity.DisplayName, marks dirty for replication, and persists
			// to PocketBase for logged-in users. See
			// documentation/plans/2026-07-07-avatar-name-tags-design.md.
			pctx, pspan := s.tracer.Start(
				otelinternal.ContextFromTraceparent(ctx, p.SetName.GetTraceparent()),
				"pusher.nats.publish.set_name",
			)
			pspan.SetAttributes(attribute.String("client.id", clientID))
			nameBytes, _ := proto.Marshal(p.SetName)
			subject := fmt.Sprintf("client.%s.set_name", clientID)
			msg := &nats.Msg{Subject: subject, Data: nameBytes}
			otelinternal.Inject(pctx, msg)
			if err := s.nc.PublishMsg(msg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish set_name")
				s.logger.Warn("nats publish set_name", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_SetSpriteBase:
			// Forward sprite sheet change to worldsim on
			// client.<id>.set_sprite_base. Worldsim validates the ID exists,
			// updates Entity.SpriteBase, marks dirty for replication, and
			// persists to PocketBase for logged-in users. See
			// documentation/plans/2026-07-07-sprite-selection-design.md.
			pctx, pspan := s.tracer.Start(
				otelinternal.ContextFromTraceparent(ctx, p.SetSpriteBase.GetTraceparent()),
				"pusher.nats.publish.set_sprite_base",
			)
			pspan.SetAttributes(attribute.String("client.id", clientID))
			spriteBytes, _ := proto.Marshal(p.SetSpriteBase)
			subject := fmt.Sprintf("client.%s.set_sprite_base", clientID)
			msg := &nats.Msg{Subject: subject, Data: spriteBytes}
			otelinternal.Inject(pctx, msg)
			if err := s.nc.PublishMsg(msg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish set_sprite_base")
				s.logger.Warn("nats publish set_sprite_base", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_Action:
			// Player-initiated input trigger (key/click) — see
			// 14-zones-and-interactions.md §3a. Forwarded to worldsim like
			// InputFrame; the result comes back on the replication subject
			// this session already subscribes to (ActionResultFrame).
			pctx, pspan := s.tracer.Start(
				otelinternal.ContextFromTraceparent(ctx, p.Action.GetTraceparent()),
				"pusher.nats.publish.action",
			)
			pspan.SetAttributes(attribute.String("client.id", clientID), attribute.String("action.input", p.Action.GetInput()))
			actionBytes, _ := proto.Marshal(p.Action)
			subject := fmt.Sprintf("client.%s.action", clientID)
			msg := &nats.Msg{Subject: subject, Data: actionBytes}
			otelinternal.Inject(pctx, msg)
			if err := s.nc.PublishMsg(msg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish action")
				s.logger.Warn("nats publish action", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_SetPlayerOptions:
			// Forward player options update to worldsim on
			// client.<id>.set_player_options. Worldsim updates the entity
			// in memory and persists to PocketBase for logged-in users.
			pctx, pspan := s.tracer.Start(
				otelinternal.ContextFromTraceparent(ctx, p.SetPlayerOptions.GetTraceparent()),
				"pusher.nats.publish.set_player_options",
			)
			pspan.SetAttributes(attribute.String("client.id", clientID))
			optsBytes, _ := proto.Marshal(p.SetPlayerOptions)
			subject := fmt.Sprintf("client.%s.set_player_options", clientID)
			msg := &nats.Msg{Subject: subject, Data: optsBytes}
			otelinternal.Inject(pctx, msg)
			if err := s.nc.PublishMsg(msg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish set_player_options")
				s.logger.Warn("nats publish set_player_options", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_SetStatus:
			// Forward status change to worldsim on client.<id>.set_status.
			// Worldsim validates the enum range (0-2), updates Entity.Status,
			// marks dirty for replication, persists to PocketBase (players.status),
			// and broadcasts on worldsim.player_status so ext-av can enforce
			// DND A/V exclusion.
			pctx, pspan := s.tracer.Start(
				otelinternal.ContextFromTraceparent(ctx, p.SetStatus.GetTraceparent()),
				"pusher.nats.publish.set_status",
			)
			pspan.SetAttributes(attribute.String("client.id", clientID), attribute.Int("status", int(p.SetStatus.GetStatus())))
			statusBytes, _ := proto.Marshal(p.SetStatus)
			subject := fmt.Sprintf("client.%s.set_status", clientID)
			msg := &nats.Msg{Subject: subject, Data: statusBytes}
			otelinternal.Inject(pctx, msg)
			if err := s.nc.PublishMsg(msg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish set_status")
				s.logger.Warn("nats publish set_status", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_SetAfk:
			// Forward AFK toggle to worldsim on client.<id>.set_afk.
			// Worldsim sets Entity.AFK and marks dirtyName so the DisplayName
			// component (which carries afk) is re-replicated. NOT persisted to
			// PocketBase and NOT broadcast on worldsim.player_status (ext-av
			// only cares about DND). See
			// documentation/plans/2026-07-22-afk-state-design.md.
			pctx, pspan := s.tracer.Start(
				otelinternal.ContextFromTraceparent(ctx, p.SetAfk.GetTraceparent()),
				"pusher.nats.publish.set_afk",
			)
			pspan.SetAttributes(attribute.String("client.id", clientID), attribute.Bool("afk", p.SetAfk.GetAfk()))
			afkBytes, _ := proto.Marshal(p.SetAfk)
			subject := fmt.Sprintf("client.%s.set_afk", clientID)
			msg := &nats.Msg{Subject: subject, Data: afkBytes}
			otelinternal.Inject(pctx, msg)
			if err := s.nc.PublishMsg(msg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish set_afk")
				s.logger.Warn("nats publish set_afk", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_Recording:
			// Forward recording start/stop to ext-rec on recording.<action>.
			// ext-rec authorizes via worldsim.entity_info (admin only) and
			// starts/stops a LiveKit Egress. The pusher adds client_id and
			// entity_id from the session; room and target come from the frame.
			action := p.Recording.GetAction()
			if action != "start" && action != "stop" {
				s.logger.Warn("recording frame invalid action", "client", clientID, "action", action)
				continue
			}
			pctx, pspan := s.tracer.Start(
				otelinternal.ContextFromTraceparent(ctx, p.Recording.GetTraceparent()),
				"pusher.nats.publish.recording",
			)
			pspan.SetAttributes(
				attribute.String("client.id", clientID),
				attribute.String("recording.action", action),
				attribute.String("recording.room", p.Recording.GetRoom()),
				attribute.String("recording.target", p.Recording.GetTarget()),
			)
			payload := map[string]string{
				"client_id":         clientID,
				"entity_id":         entityID,
				"room":              p.Recording.GetRoom(),
				"target":            p.Recording.GetTarget(),
				"youtube_rtmp_url":  p.Recording.GetYoutubeRtmpUrl(),
				"youtube_stream_key": p.Recording.GetYoutubeStreamKey(),
			}
			payloadBytes, _ := json.Marshal(payload)
			subject := "recording." + action
			msg := &nats.Msg{Subject: subject, Data: payloadBytes}
			otelinternal.Inject(pctx, msg)
			if err := s.nc.PublishMsg(msg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish recording")
				s.logger.Warn("nats publish recording", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_Kick:
			// Admin kick: forward to worldsim.client.kick.
			// Worldsim resolves entity_id → client_id, despawns the entity,
			// and publishes force_close so the pusher closes the target's
			// WebSocket. Fire-and-forget — the admin sees the target
			// disappear via replication. We do NOT send an ack frame
			// because reusing AuthResult would make the WsClient think
			// it's a new auth result and overwrite the admin's own
			// clientId/entityId.
			pctx, pspan := s.tracer.Start(ctx, "pusher.nats.publish.kick")
			pspan.SetAttributes(attribute.String("client.id", clientID), attribute.String("kick.entity_id", p.Kick.GetEntityId()))
			kickPayload := map[string]string{
				"entity_id": p.Kick.GetEntityId(),
				"reason":    p.Kick.GetReason(),
			}
			kickBytes, _ := json.Marshal(kickPayload)
			kickMsg := &nats.Msg{Subject: "worldsim.client.kick", Data: kickBytes}
			otelinternal.Inject(pctx, kickMsg)
			if err := s.nc.PublishMsg(kickMsg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish kick")
				s.logger.Warn("kick publish failed", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_PingPlayer:
			// Player ping: forward to worldsim.client.ping. Worldsim resolves
			// entity_id → client_id, drops the ping if the target is in DND
			// mode, and otherwise publishes a PlayerPingFrame to the target's
			// ping_inbox. Fire-and-forget — the target's client plays a sound
			// on receipt.
			pctx, pspan := s.tracer.Start(ctx, "pusher.nats.publish.ping")
			pspan.SetAttributes(attribute.String("client.id", clientID), attribute.String("ping.entity_id", p.PingPlayer.GetEntityId()))
			pingPayload := map[string]string{
				"entity_id":       p.PingPlayer.GetEntityId(),
				"sender_client_id": clientID,
			}
			pingBytes, _ := json.Marshal(pingPayload)
			pingMsg := &nats.Msg{Subject: "worldsim.client.ping", Data: pingBytes}
			otelinternal.Inject(pctx, pingMsg)
			if err := s.nc.PublishMsg(pingMsg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish ping")
				s.logger.Warn("ping publish failed", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_TeleportTo:
			// Teleport-to-player: forward to worldsim.entity.teleport_to_entity.
			// Worldsim enforces authorization server-side (admin always;
			// registered non-guest only when allow_player_teleport is on;
			// guests never) and moves the sender to the target's exact
			// position on the same map. Fire-and-forget — the sender's client
			// sees its own position update via replication.
			pctx, pspan := s.tracer.Start(ctx, "pusher.nats.publish.teleport_to")
			pspan.SetAttributes(attribute.String("client.id", clientID), attribute.String("teleport_to.entity_id", p.TeleportTo.GetEntityId()))
			teleportPayload := map[string]string{
				"sender_client_id": clientID,
				"target_entity_id": p.TeleportTo.GetEntityId(),
			}
			teleportBytes, _ := json.Marshal(teleportPayload)
			teleportMsg := &nats.Msg{Subject: "worldsim.entity.teleport_to_entity", Data: teleportBytes}
			otelinternal.Inject(pctx, teleportMsg)
			if err := s.nc.PublishMsg(teleportMsg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish teleport_to")
				s.logger.Warn("teleport_to publish failed", "client", clientID, "err", err)
			}
			pspan.End()
		case *pb.ClientFrame_AdminTeleport:
			// Admin teleport of ANOTHER player to a target map (optionally to
			// an exact x/y, e.g. "teleport to me"). Forward to
			// worldsim.entity.teleport with sender_client_id set so worldsim
			// can authorize the sender as an admin. Fire-and-forget — the
			// target's client sees a MapTransitionFrame via replication.
			pctx, pspan := s.tracer.Start(ctx, "pusher.nats.publish.admin_teleport")
			pspan.SetAttributes(
				attribute.String("client.id", clientID),
				attribute.String("admin_teleport.entity_id", p.AdminTeleport.GetEntityId()),
				attribute.String("admin_teleport.map_id", p.AdminTeleport.GetMapId()),
				attribute.Bool("admin_teleport.exact_position", p.AdminTeleport.GetExactPosition()),
			)
			adminTeleportPayload := map[string]any{
				"sender_client_id": clientID,
				"entity_id":        p.AdminTeleport.GetEntityId(),
				"map_id":           p.AdminTeleport.GetMapId(),
				"x":                p.AdminTeleport.GetX(),
				"y":                p.AdminTeleport.GetY(),
				"exact_position":   p.AdminTeleport.GetExactPosition(),
			}
			adminTeleportBytes, _ := json.Marshal(adminTeleportPayload)
			adminTeleportMsg := &nats.Msg{Subject: "worldsim.entity.teleport", Data: adminTeleportBytes}
			otelinternal.Inject(pctx, adminTeleportMsg)
			if err := s.nc.PublishMsg(adminTeleportMsg); err != nil {
				pspan.RecordError(err)
				pspan.SetStatus(codes.Error, "nats publish admin_teleport")
				s.logger.Warn("admin_teleport publish failed", "client", clientID, "err", err)
			}
			pspan.End()
		}
		}
	}

// keepalive sends a WebSocket protocol-level ping every PingInterval. The
// browser auto-responds with a pong, keeping idle connections alive. On ping
// failure (dead peer / timeout) it closes the connection so the read loop in
// handleWS exits and the session is torn down. Must run concurrently with the
// read loop, as coder/websocket requires for Ping.
func (s *Server) keepalive(ctx context.Context, c *websocket.Conn, clientID string) {
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := c.Ping(pingCtx); err != nil {
				cancel()
				s.logger.Info("ws keepalive ping failed", "client", clientID, "err", err)
				audit.Emit(s.nc, "ws.keepalive_timeout", audit.SeverityWarn,
					audit.Actor{ClientID: clientID},
					audit.Details{"error": err.Error()},
					"")
				c.Close(websocket.StatusPolicyViolation, "keepalive timeout")
				return
			}
			cancel()
			// Tell worldsim the WebSocket is alive so its client reaper
			// doesn't despawn this entity if a client.disconnected is lost
			// (e.g. pusher crash/restart). Fire-and-forget; loss is tolerated
			// by the reaper's 3x timeout window.
			if err := s.nc.Publish("client."+clientID+".heartbeat", nil); err != nil {
				s.logger.Warn("nats publish heartbeat", "client", clientID, "err", err)
			}
		}
	}
}

// publishLifecycle publishes a client.connected/disconnected event carrying
// the current span context so worldsim's provision/despawn spans parent here.
func (s *Server) publishLifecycle(ctx context.Context, subject, clientID, sub string) {
	msg, _ := proto.Marshal(&pb.AuthResultFrame{ClientId: clientID, Sub: sub})
	m := &nats.Msg{Subject: subject, Data: msg}
	otelinternal.Inject(ctx, m)
	if err := s.nc.PublishMsg(m); err != nil {
		s.logger.Warn("nats publish lifecycle", "subject", subject, "err", err)
	}
}

// publishLifecycleRequest is like publishLifecycle but uses NATS request-reply
// and returns the reply data. Used for client.connected so worldsim can return
// the provisioned entity ID. The client IP and device_id are carried so
// worldsim can persist the IP and check the ban list.
func (s *Server) publishLifecycleRequest(ctx context.Context, subject, clientID, sub, ip, deviceID string, force bool) ([]byte, error) {
	msg, _ := proto.Marshal(&pb.AuthResultFrame{ClientId: clientID, Sub: sub, Ip: ip, DeviceId: deviceID, Force: force})
	m := &nats.Msg{Subject: subject, Data: msg}
	otelinternal.Inject(ctx, m)
	reply, err := s.nc.RequestMsg(m, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func generateClientID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "c_" + hex.EncodeToString(b)
}

// subjectClientID extracts the client ID from a "client.<id>.<suffix>" NATS
// subject. e.g. "client.c_abc.force_close" → "c_abc".
func subjectClientID(subject, suffix string) string {
	prefix := "client."
	s := subject[len(prefix):]
	end := len(s) - len("."+suffix)
	if end < 0 {
		return ""
	}
	return s[:end]
}

// clientIP extracts the client's IP address from the request. It prefers the
// X-Forwarded-For header (set by nginx; first entry is the original client),
// then X-Real-IP, then falls back to the TCP remote address. The port is
// stripped — only the IP is stored.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.IndexByte(xff, ','); idx >= 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
