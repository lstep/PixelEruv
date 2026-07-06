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
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	otelinternal "github.com/lstep/pixeleruv/backend/internal/otel"
	pb "github.com/lstep/pixeleruv/backend/internal/pb"
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
	wsAddr   string
	nc       *nats.Conn
	logger   *slog.Logger
	tracer   trace.Tracer
	auth     *AuthValidator
	sessions sync.Map // client_id -> *session
}

type session struct {
	clientID  string
	conn      *websocket.Conn
	sub       *nats.Subscription
	avSub     *nats.Subscription
	closeOnce sync.Once
}

func New(wsAddr, natsURL, dexIssuer, dexJwksURL, dexClientID string, logger *slog.Logger) (*Server, error) {
	nc, err := nats.Connect(natsURL,
		nats.Name("pusher"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	var auth *AuthValidator
	if dexIssuer != "" {
		if dexJwksURL == "" {
			dexJwksURL = dexIssuer + "/keys"
		}
		auth = NewAuthValidator(dexIssuer, dexJwksURL, dexClientID)
	}
	return &Server{
		wsAddr: wsAddr,
		nc:     nc,
		logger: logger,
		tracer: otel.Tracer("pusher"),
		auth:   auth,
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)

	srv := &http.Server{Addr: s.wsAddr, Handler: mux}

	// Start JWKS refresh loop if Dex is configured.
	if s.auth != nil && s.auth.issuer != "" {
		go s.auth.startKeyRefresh(ctx)
	}

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
		s.nc.Close()
	}()

	return srv.ListenAndServe()
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

	// Validate the id_token via Dex JWKS. If DEX_URL is not set, fall back
	// to dummy auth for local dev without Dex.
	var sub string
	if s.auth != nil && s.auth.issuer != "" {
		sub, err = s.auth.ValidateToken(auth.GetIdToken())
		if err != nil {
			authSpan.RecordError(err)
			authSpan.SetStatus(codes.Error, "token validation")
			s.logger.Warn("token validation failed", "err", err)
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
		authSpan.SetAttributes(attribute.String("user.sub", sub))
	} else {
		sub = "dev"
	}

	clientID := generateClientID()
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

	s.sessions.Store(clientID, sess)
	defer func() {
		sess.sub.Unsubscribe()
		if sess.avSub != nil {
			sess.avSub.Unsubscribe()
		}
		s.sessions.Delete(clientID)
		s.publishLifecycle(authCtx, "client.disconnected", clientID, sub)
	}()

	// Publish client.connected so World Sim provisions the entity. Use
	// request-reply to get the actual entity ID, which may differ from
	// "e_"+clientID[2:] when a PocketBase-stored identity exists. The
	// client needs the real entity ID to identify its own avatar.
	entityID := ""
	if reply, err := s.publishLifecycleRequest(authCtx, "client.connected", clientID, sub); err != nil {
		s.logger.Warn("client.connected request-reply, using default entity ID", "client", clientID, "err", err)
	} else {
		var ar pb.AuthResultFrame
		if err := proto.Unmarshal(reply, &ar); err != nil {
			s.logger.Warn("client.connected reply unmarshal", "client", clientID, "err", err)
		} else {
			entityID = ar.EntityId
		}
	}

	// Send AuthResultFrame with the entity ID from worldsim.
	result := &pb.ServerFrame{
		Payload: &pb.ServerFrame_AuthResult{
			AuthResult: &pb.AuthResultFrame{
				Ok:       true,
				ClientId: clientID,
				EntityId: entityID,
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
				c.Close(websocket.StatusPolicyViolation, "keepalive timeout")
				return
			}
			cancel()
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
// the provisioned entity ID.
func (s *Server) publishLifecycleRequest(ctx context.Context, subject, clientID, sub string) ([]byte, error) {
	msg, _ := proto.Marshal(&pb.AuthResultFrame{ClientId: clientID, Sub: sub})
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
