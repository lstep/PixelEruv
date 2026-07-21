package extkit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats-server/v2/test"
)

func startEmbeddedNATS(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	host, port, _ := net.SplitHostPort(addr)
	opts := test.DefaultTestOptions
	opts.Host = host
	opts.Port, _ = net.LookupPort("tcp", port)
	srv, err := server.NewServer(&opts)
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(2 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return "nats://" + addr
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestEnvOr(t *testing.T) {
	t.Setenv("EXTKIT_TEST_VAR", "value")
	if got := EnvOr("EXTKIT_TEST_VAR", "fallback"); got != "value" {
		t.Errorf("got %q, want %q", got, "value")
	}
	if got := EnvOr("EXTKIT_NONEXISTENT", "fallback"); got != "fallback" {
		t.Errorf("got %q, want %q", got, "fallback")
	}
}

func TestConnectNATS(t *testing.T) {
	url := startEmbeddedNATS(t)
	nc, err := ConnectNATS(url, "test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	if !nc.IsConnected() {
		t.Fatal("not connected")
	}
}

func TestPublishHealth(t *testing.T) {
	url := startEmbeddedNATS(t)
	nc, err := ConnectNATS(url, "test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	sub, err := nc.SubscribeSync("healthz")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })

	PublishHealth(nc, "ext-test", time.Now())

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no health msg: %v", err)
	}
	var health map[string]any
	if err := json.Unmarshal(msg.Data, &health); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if health["service"] != "ext-test" {
		t.Errorf("service: got %v, want ext-test", health["service"])
	}
	if health["status"] != "OK" {
		t.Errorf("status: got %v, want OK", health["status"])
	}
}

func TestWaitForReady_Received(t *testing.T) {
	url := startEmbeddedNATS(t)
	nc, err := ConnectNATS(url, "test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	var calledMu sync.Mutex
	var readyMap string
	called := false

	// Publish worldsim.ready after a short delay so the subscription is live.
	go func() {
		time.Sleep(100 * time.Millisecond)
		nc.Publish("worldsim.ready", []byte("main"))
	}()

	WaitForReady(nc, testLogger(), 5*time.Second, func(mapName string) {
		calledMu.Lock()
		called = true
		readyMap = mapName
		calledMu.Unlock()
	})

	calledMu.Lock()
	defer calledMu.Unlock()
	if !called {
		t.Fatal("onReady not called")
	}
	if readyMap != "main" {
		t.Errorf("mapName: got %q, want %q", readyMap, "main")
	}
}

func TestWaitForReady_Timeout(t *testing.T) {
	url := startEmbeddedNATS(t)
	nc, err := ConnectNATS(url, "test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	var calledMu sync.Mutex
	called := false

	WaitForReady(nc, testLogger(), 50*time.Millisecond, func(mapName string) {
		calledMu.Lock()
		called = true
		if mapName != "" {
			t.Errorf("mapName on timeout: got %q, want empty", mapName)
		}
		calledMu.Unlock()
	})

	calledMu.Lock()
	defer calledMu.Unlock()
	if !called {
		t.Fatal("onReady not called on timeout")
	}
}

func TestHeartbeatLoop_Cancel(t *testing.T) {
	url := startEmbeddedNATS(t)
	nc, err := ConnectNATS(url, "test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		HeartbeatLoop(ctx, nc, "test", 1, func() {})
		close(done)
	}()

	// Cancel immediately — loop should exit without waiting for the first tick.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HeartbeatLoop did not exit on ctx cancel")
	}
}

func TestHeartbeatLoop_Publishes(t *testing.T) {
	url := startEmbeddedNATS(t)
	nc, err := ConnectNATS(url, "test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	sub, err := nc.SubscribeSync("extension.test.heartbeat")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go HeartbeatLoop(ctx, nc, "test", 1, func() {})

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no heartbeat: %v", err)
	}
	if string(msg.Data) != "test" {
		t.Errorf("heartbeat data: got %q, want %q", string(msg.Data), "test")
	}
}

func TestSubscribeOptions(t *testing.T) {
	url := startEmbeddedNATS(t)
	nc, err := ConnectNATS(url, "test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	type testOpts struct {
		Foo string `json:"foo"`
		Bar int    `json:"bar"`
	}
	opts := testOpts{Foo: "default", Bar: 42}
	var mu sync.Mutex
	reloadCalled := false

	if err := SubscribeOptions(nc, "test", &opts, &mu, testLogger(), func() {
		reloadCalled = true
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Publish valid options.
	nc.Publish("extension.test.options", []byte(`{"foo":"updated","bar":99}`))
	nc.Flush()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	if opts.Foo != "updated" || opts.Bar != 99 {
		t.Errorf("after valid update: foo=%q bar=%d, want updated/99", opts.Foo, opts.Bar)
	}
	if !reloadCalled {
		t.Error("onReload not called on valid update")
	}
	mu.Unlock()

	// Publish invalid JSON — should roll back and not call onReload.
	reloadCalled = false
	nc.Publish("extension.test.options", []byte(`{bad json`))
	nc.Flush()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	if opts.Foo != "updated" || opts.Bar != 99 {
		t.Errorf("after invalid update: foo=%q bar=%d, want rolled back to updated/99", opts.Foo, opts.Bar)
	}
	if reloadCalled {
		t.Error("onReload called on invalid update")
	}
	mu.Unlock()
}
