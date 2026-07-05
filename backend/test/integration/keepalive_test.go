package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/lstep/pixeleruv/backend/internal/pb"
	"github.com/lstep/pixeleruv/backend/internal/pusher"
)

// TestPusherSendsKeepalivePing verifies the pusher sends WebSocket
// protocol-level pings on idle connections so they don't die after long
// idle periods (the original bug: idle player → no traffic → TCP death →
// "The network connection was lost" → player can't move).
//
// The browser auto-responds to protocol pings with a pong, so there is no
// client-side keepalive code; this test uses coder/websocket's OnPingReceived
// dial callback to observe the server-initiated ping directly.
//
// Prerequisites: docker compose up (nats). The pusher is started in-process
// by TestMain (no Dex) so IdToken="dev" is accepted.
func TestPusherSendsKeepalivePing(t *testing.T) {
	// Speed up the ping interval for a fast, deterministic test.
	prev := pusher.PingInterval
	pusher.PingInterval = 200 * time.Millisecond
	defer func() { pusher.PingInterval = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pingCh := make(chan struct{}, 4)

	c, _, err := websocket.Dial(ctx, pusherAddr, &websocket.DialOptions{
		OnPingReceived: func(_ context.Context, _ []byte) bool {
			select {
			case pingCh <- struct{}{}:
			default:
			}
			return true // auto-respond with pong
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Send AuthFrame.
	auth := &pb.ClientFrame{Payload: &pb.ClientFrame_Auth{Auth: &pb.AuthFrame{IdToken: "dev"}}}
	if err := c.Write(ctx, websocket.MessageBinary, mustMarshal(auth)); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	// Read AuthResult (and keep the reader pumping afterward so incoming
	// ping control frames are processed and OnPingReceived fires).
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read auth result: %v", err)
	}
	go func() {
		for {
			readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
			_, _, err := c.Read(readCtx)
			readCancel()
			if err != nil {
				return
			}
		}
	}()

	// Expect at least one ping within a generous grace of the interval.
	select {
	case <-pingCh:
		t.Logf("received keepalive ping")
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive keepalive ping within 2s")
	}
}
