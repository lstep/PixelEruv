// Package pusher is a thin WebSocket proxy between the browser and NATS Core.
// It handles WebSocket I/O, dummy token validation, session management, and
// NATS forwarding. No game logic.
package pusher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	s.sessions.Store(clientID, sess)
	defer func() {
		sess.sub.Unsubscribe()
		s.sessions.Delete(clientID)
		s.publishLifecycle(authCtx, "client.disconnected", clientID, sub)
	}()

	// Send AuthResultFrame.
	// The World Sim will send the entity_id via the first replication batch
	// (SpawnEntity). For now, entity_id is empty here.
	result := &pb.ServerFrame{
		Payload: &pb.ServerFrame_AuthResult{
			AuthResult: &pb.AuthResultFrame{
				Ok:       true,
				ClientId: clientID,
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

	// Publish client.connected so World Sim provisions the entity.
	s.publishLifecycle(authCtx, "client.connected", clientID, sub)

	s.logger.Info("client connected", "client", clientID)

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

func generateClientID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "c_" + hex.EncodeToString(b)
}
