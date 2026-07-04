---
name: nats-go
version: 0.1.0
author: Hermes
description: "Use the NATS Go client for pub/sub and JetStream."
metadata:
  hermes:
    tags: [Go, NATS, Messaging, JetStream]
---

# NATS Go Client

Reference for `github.com/nats-io/nats.go` — the official Go client for the NATS messaging system. Covers core pub/sub, request/reply, queue groups, wildcard subscriptions, TLS/auth, context support, and the JetStream API (`jetstream` package) for persistent messaging, KV store, and object store. No external deps beyond the library itself.

## When to Use

- "I need to publish or subscribe to NATS topics in Go"
- "Set up a JetStream stream and consumer in Go"
- "Request/reply pattern with NATS"
- "NATS queue groups for load balancing"
- "NATS TLS or NKey/credentials authentication"
- "JetStream KeyValue store in Go"
- "Wildcard subscriptions with NATS"

## Prerequisites

- Go 1.21+ (library supports 2 latest minor Go versions)
- Install: `go get github.com/nats-io/nats.go@latest`
- NATS server v2.9+ required for the `jetstream` package
- Import paths:
  - Core: `"github.com/nats-io/nats.go"`
  - JetStream: `"github.com/nats-io/nats.go/jetstream"`

## How to Run

Write Go code using the library. Compile and run through the `terminal` tool:

```bash
go run main.go
```

Alternatively, use `execute_code` to scaffold a full project structure.

## Quick Reference

### Core NATS (`nats` package)

| Function | Purpose |
|----------|---------|
| `nats.Connect(url string, options ...Option) (*Conn, error)` | Connect to server |
| `nc.Publish(subject string, data []byte)` | Publish message |
| `nc.Subscribe(subject string, cb func(*nats.Msg))` | Async subscriber |
| `nc.SubscribeSync(subject string)` | Sync subscriber |
| `nc.ChanSubscribe(subject string, ch chan *nats.Msg)` | Channel subscriber |
| `nc.QueueSubscribe(subject, queue string, cb)` | Queue group subscriber |
| `nc.Request(subject string, data []byte, timeout)` | Request/reply |
| `nc.RequestWithContext(ctx, subject, data)` | Request with context |
| `nc.Drain()` | Graceful close (preferred over Close) |
| `nc.Close()` | Close connection |
| `nc.Flush()` | Flush pending messages |
| `nc.FlushTimeout(timeout)` | Flush with deadline |
| `nats.NewEncodedConn(nc, encType)` | Encoded connection (JSON, gob, etc.) |
| `nats.DefaultURL` | `"nats://127.0.0.1:4222"` |

### Connect Options

| Option | Purpose |
|--------|---------|
| `nats.UserCredentials(credsFile)` | JWT+NKey auth from creds file |
| `nats.UserCredentials(jwtFile, seedFile)` | Separate JWT and seed |
| `nats.UserInfo(user, password)` | Basic auth |
| `nats.Token(token)` | Token auth |
| `nats.Nkey(pubKey, sigCB)` | NKey auth |
| `nats.NkeyOptionFromSeed(seedFile)` | NKey from seed file |
| `nats.RootCAs(caFile)` | TLS with custom CA |
| `nats.ClientCert(certFile, keyFile)` | TLS client cert |
| `nats.Secure(tlsConfig)` | Full tls.Config |
| `nats.MaxReconnects(n)` | Max reconnect attempts |
| `nats.ReconnectWait(d)` | Reconnect delay |
| `nats.ReconnectJitter(d, tlsD)` | Add jitter to reconnects |
| `nats.DontRandomize()` | Disable server pool randomization |
| `nats.RetryOnFailedConnect(true)` | Retry on initial connect failure |
| `nats.Name(name)` | Client name |
| `nats.NoReconnect()` | Disable reconnects |
| `nats.NoEcho()` | Disable message echo |
| `nats.DisconnectErrHandler(cb)` | Disconnect callback |
| `nats.ReconnectHandler(cb)` | Reconnect callback |
| `nats.ClosedHandler(cb)` | Close callback |
| `nats.ErrorHandler(cb)` | Error callback |

### Wildcard Subjects

- `*` matches any single token: `foo.*.baz` matches `foo.bar.baz`
- `>` matches any tail length (must be last token): `foo.>` matches `foo.bar`, `foo.bar.baz`

### JetStream (`jetstream` package)

| Function | Purpose |
|----------|---------|
| `jetstream.New(nc *nats.Conn) (JetStream, error)` | Create JS context |
| `js.CreateStream(ctx, StreamConfig)` | Create stream (idempotent) |
| `js.UpdateStream(ctx, StreamConfig)` | Update stream |
| `js.Stream(ctx, name)` | Get stream handle |
| `js.DeleteStream(ctx, name)` | Delete stream |
| `js.ListStreams(ctx)` | List streams |
| `js.StreamNames(ctx)` | List stream names |
| `js.Publish(ctx, subject, data)` | Sync publish |
| `js.PublishAsync(ctx, subject, data)` | Async publish |
| `js.CreateConsumer(ctx, stream, ConsumerConfig)` | Create pull consumer |
| `js.CreateOrUpdateConsumer(ctx, stream, cfg)` | Create or update |
| `js.Consumer(ctx, stream, name)` | Get consumer handle |
| `js.DeleteConsumer(ctx, stream, name)` | Delete consumer |
| `js.OrderedConsumer(ctx, stream, cfg)` | Ordered consumer |
| `c.Fetch(n)` | Fetch n messages |
| `c.Consume(cb func(jetstream.Msg))` | Continuous callback consumer |
| `c.Messages()` | Iterator consumer |
| `msg.Ack()` | Acknowledge message |
| `msg.Nak()` | Negative ack |
| `msg.Term()` | Terminate message |
| `s.Purge(ctx, ...PurgeOption)` | Purge messages |
| `s.GetMsg(ctx, seq)` | Get message by sequence |
| `s.GetLastMsgForSubject(ctx, subject)` | Last msg for subject |
| `s.DeleteMsg(ctx, seq)` | Delete message by sequence |
| `s.Info(ctx)` | Stream info (live) |
| `s.CachedInfo()` | Cached stream info |

### JetStream KV Store

| Function | Purpose |
|----------|---------|
| `js.CreateKeyValue(ctx, KeyValueConfig)` | Create KV bucket |
| `js.KeyValue(ctx, bucket)` | Get KV handle |
| `js.DeleteKeyValue(ctx, bucket)` | Delete KV bucket |
| `kv.Put(ctx, key, value)` | Put value |
| `kv.Get(ctx, key)` | Get value |
| `kv.Delete(ctx, key)` | Delete key |
| `kv.Watch(ctx, filter)` | Watch for changes |

## Procedure

### 1. Core pub/sub

```go
nc, _ := nats.Connect(nats.DefaultURL)
defer nc.Drain()

// Publish
nc.Publish("foo", []byte("Hello World"))

// Async subscribe
nc.Subscribe("foo", func(m *nats.Msg) {
    fmt.Printf("Received: %s\n", string(m.Data))
})

// Sync subscribe
sub, _ := nc.SubscribeSync("foo")
msg, _ := sub.NextMsg(2 * time.Second)

// Channel subscribe
ch := make(chan *nats.Msg, 64)
nc.ChanSubscribe("foo", ch)
msg = <-ch

// Queue group
nc.QueueSubscribe("foo", "job_workers", func(m *nats.Msg) {
    // only one subscriber per queue group receives each message
})
```

### 2. Request/reply

```go
// Request
msg, err := nc.Request("help", []byte("help me"), 10*time.Millisecond)

// Reply handler
nc.Subscribe("help", func(m *nats.Msg) {
    m.Respond([]byte("answer is 42"))
})
```

### 3. TLS connection

```go
// TLS scheme (verifies server name)
nc, _ := nats.Connect("tls://nats.demo.io:4443")

// Self-signed CA
nc, _ = nats.Connect("tls://localhost:4443", nats.RootCAs("./ca.pem"))

// Client cert
nc, _ = nats.Connect("tls://localhost:4443",
    nats.ClientCert("./client-cert.pem", "./client-key.pem"))
```

### 4. Auth — user credentials and NKeys

```go
// JWT + NKey from single creds file
nc, _ := nats.Connect(url, nats.UserCredentials("user.creds"))

// Separate JWT and seed
nc, _ = nats.Connect(url, nats.UserCredentials("user.jwt", "user.nk"))

// NKey from seed file
nc, _ = nats.Connect(url, nats.NkeyOptionFromSeed("seed.txt"))
```

### 5. Clustered connection with callbacks

```go
servers := "nats://host1:4222, nats://host2:4222, nats://host3:4222"
nc, _ := nats.Connect(servers,
    nats.MaxReconnects(5),
    nats.ReconnectWait(2*time.Second),
    nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
        fmt.Printf("Disconnected: %q\n", err)
    }),
    nats.ReconnectHandler(func(nc *nats.Conn) {
        fmt.Printf("Reconnected to %v\n", nc.ConnectedUrl())
    }),
    nats.ClosedHandler(func(nc *nats.Conn) {
        fmt.Printf("Closed: %q\n", nc.LastError())
    }),
)
```

### 6. JetStream stream + consumer

```go
import "github.com/nats-io/nats.go/jetstream"

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

nc, _ := nats.Connect(nats.DefaultURL)
js, _ := jetstream.New(nc)

// Create stream (idempotent)
s, _ := js.CreateStream(ctx, jetstream.StreamConfig{
    Name:     "ORDERS",
    Subjects: []string{"ORDERS.*"},
})

// Publish
js.Publish(ctx, "ORDERS.new", []byte("hello"))

// Create durable pull consumer
c, _ := s.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
    Durable:   "CONS",
    AckPolicy: jetstream.AckExplicitPolicy,
})

// Continuous consume via callback
cc, _ := c.Consume(func(msg jetstream.Msg) {
    fmt.Println(string(msg.Data()))
    msg.Ack()
})
defer cc.Stop()

// Or batch fetch
msgs, _ := c.Fetch(10)
for msg := range msgs.Messages() {
    msg.Ack()
    fmt.Println(string(msg.Data()))
}
```

### 7. JetStream KeyValue store

```go
kv, _ := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
    Bucket: "MY_KV",
})

kv.Put(ctx, "key1", []byte("value1"))
entry, _ := kv.Get(ctx, "key1")
fmt.Println(string(entry.Value()))

// Watch for changes
watcher, _ := kv.Watch(ctx, ">")
for entry := range watcher.Updates() {
    if entry != nil {
        fmt.Printf("%s: %s\n", entry.Key(), string(entry.Value()))
    }
}
```

## Pitfalls

- **JetStream requires server ≥ 2.9.0**: The `jetstream` package won't work correctly with older servers.
- **`Drain()` vs `Close()`**: Always prefer `nc.Drain()` — it flushes pending messages and subscriptions before closing. `Close()` is immediate.
- **Encoded connections**: `nats.NewEncodedConn` supports JSON/gob but requires registering encoders for custom types via `nats.RegisterEncoder`.
- **Context required for JetStream**: Nearly all `jetstream` API calls take `context.Context` — always set a timeout.
- **Pull vs push consumers**: Use pull consumers (default) for fine-grained control. Push consumers are mainly for migration from the legacy `nats` package. Push consumers require `DeliverSubject` and use `CreatePushConsumer` / `Consumer` methods.
- **`CreateConsumer` is idempotent only if config matches**: If a consumer with the same durable name exists but has different config, an error is returned. Use `CreateOrUpdateConsumer` to avoid this.
- **Ordered consumers are pull-only**: Push consumers do not support ordered mode.
- **Wildcard `>` must be the last token**: `foo.>` is valid, `foo.>.bar` is not.

## Verification

```bash
go run -v main.go
```

A minimal program that connects, publishes, subscribes, and prints received messages confirms the library works.
