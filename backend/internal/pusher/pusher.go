// Package pusher is a thin WebSocket proxy between the browser and NATS Core.
// It handles WebSocket I/O, dummy token validation, session management, and
// NATS forwarding. No game logic.
package pusher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats.go"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	wsAddr  string
	nc      *nats.Conn
	sessions sync.Map // client_id -> *session
}

type session struct {
	clientID  string
	conn      *websocket.Conn
	sub       *nats.Subscription
	closeOnce sync.Once
}

func New(wsAddr, natsURL string) (*Server, error) {
	nc, err := nats.Connect(natsURL,
		nats.Name("pusher"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	return &Server{wsAddr: wsAddr, nc: nc}, nil
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
		log.Printf("ws accept: %v", err)
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Read the first frame — must be AuthFrame.
	ctx := r.Context()
	typ, data, err := c.Read(ctx)
	if err != nil {
		log.Printf("ws read auth: %v", err)
		return
	}
	if typ != websocket.MessageBinary {
		c.Close(websocket.StatusPolicyViolation, "expected binary protobuf frame")
		return
	}

	var cf pb.ClientFrame
	if err := proto.Unmarshal(data, &cf); err != nil {
		c.Close(websocket.StatusPolicyViolation, "bad protobuf")
		return
	}
	auth := cf.GetAuth()
	if auth == nil {
		c.Close(websocket.StatusPolicyViolation, "first frame must be AuthFrame")
		return
	}

	// Dummy auth — accept any token.
	clientID := generateClientID()
	sess := &session{clientID: clientID, conn: c}

	// Subscribe to replication + control subjects for this client.
	repSub, err := s.nc.Subscribe(fmt.Sprintf("client.%s.replication", clientID), func(m *nats.Msg) {
		// Forward raw ServerFrame bytes to the WebSocket.
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := c.Write(writeCtx, websocket.MessageBinary, m.Data); err != nil {
			log.Printf("ws write replication [%s]: %v", clientID, err)
		}
	})
	if err != nil {
		log.Printf("nats sub replication [%s]: %v", clientID, err)
		return
	}
	sess.sub = repSub
	s.sessions.Store(clientID, sess)
	defer func() {
		sess.sub.Unsubscribe()
		s.sessions.Delete(clientID)
		s.publishLifecycle("client.disconnected", clientID)
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
	if err := c.Write(r.Context(), websocket.MessageBinary, resultBytes); err != nil {
		log.Printf("ws write auth result [%s]: %v", clientID, err)
		return
	}

	// Publish client.connected so World Sim provisions the entity.
	s.publishLifecycle("client.connected", clientID)

	log.Printf("client connected: %s", clientID)

	// Main read loop — forward InputFrames to NATS.
	for {
		msgType, msgData, err := c.Read(ctx)
		if err != nil {
			log.Printf("client %s disconnected: %v", clientID, err)
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
			inputBytes, _ := proto.Marshal(p.Input)
			subject := fmt.Sprintf("client.%s.input", clientID)
			if err := s.nc.Publish(subject, inputBytes); err != nil {
				log.Printf("nats publish input [%s]: %v", clientID, err)
			}
		case *pb.ClientFrame_Ping:
			pong := &pb.ServerFrame{Payload: &pb.ServerFrame_Pong{Pong: &pb.PongFrame{}}}
			pongBytes, _ := proto.Marshal(pong)
			c.Write(ctx, websocket.MessageBinary, pongBytes)
		}
	}
}

func (s *Server) publishLifecycle(subject, clientID string) {
	msg, _ := proto.Marshal(&pb.AuthResultFrame{ClientId: clientID})
	if err := s.nc.Publish(subject, msg); err != nil {
		log.Printf("nats publish %s: %v", subject, err)
	}
}

func generateClientID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "c_" + hex.EncodeToString(b)
}
