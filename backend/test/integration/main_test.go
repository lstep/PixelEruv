package integration_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/pusher"
)

// pusherAddr is the address of the in-process pusher started by TestMain.
// It connects to the docker-exposed NATS at localhost:4222 so that
// worldsim (also in docker) receives auth/input events and publishes
// replication back. No Dex is configured, so IdToken="dev" is accepted.
var pusherAddr string

func TestMain(m *testing.M) {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}

	// Pick a free port for the in-process pusher.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv, err := pusher.New(addr, natsURL, "", "", "", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go srv.Run(ctx)

	// Wait for the pusher to be accepting connections.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	pusherAddr = "ws://" + addr + "/ws"

	code := m.Run()
	cancel()
	os.Exit(code)
}
