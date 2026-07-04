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
	sessions sync.Map // client_id -> *session
}

type session struct {
	clientID  string
	conn      *websocket.Conn
	sub       *nats.Subscription
	closeOnce sync.Once
}

func New(wsAddr, natsURL string, logger *slog.Logger) (*Server, error) {
	nc, err := nats.Connect(natsURL,
		nats.Name("pusher"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	return &Server{
		wsAddr: wsAddr,
		nc:     nc,
		logger: logger,
		tracer: otel.Tracer("pusher"),
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)

	srv := &http.Server{Addr: s.wsAddr, Handler: mux}

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

	// Dummy auth — accept any token.
	clientID := generateClientID()
	span.SetAttributes(attribute.String("client.id", clientID))
	sess := &session{clientID: clientID, conn: c}

	// Subscribe to replication + control subjects for this client.
	repSub, err := s.nc.Subscribe(fmt.Sprintf("client.%s.replication", clientID), func(m *nats.Msg) {
		// Continue the trace started by worldsim's replication publish.
		rctx, rspan := s.tracer.Start(otelinternal.Extract(ctx, m), "pusher.ws.write_replication")
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
		s.publishLifecycle(ctx, "client.disconnected", clientID)
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
	if err := c.Write(ctx, websocket.MessageBinary, resultBytes); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "ws write auth result")
		s.logger.Warn("ws write auth result", "client", clientID, "err", err)
		return
	}

	// Publish client.connected so World Sim provisions the entity.
	s.publishLifecycle(ctx, "client.connected", clientID)

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
			pctx, pspan := s.tracer.Start(ctx, "pusher.nats.publish.input")
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
		}
	}
}

// publishLifecycle publishes a client.connected/disconnected event carrying
// the current span context so worldsim's provision/despawn spans parent here.
func (s *Server) publishLifecycle(ctx context.Context, subject, clientID string) {
	msg, _ := proto.Marshal(&pb.AuthResultFrame{ClientId: clientID})
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
