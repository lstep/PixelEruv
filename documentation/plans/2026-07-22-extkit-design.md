# extkit: shared helpers for extension mains

## Problem

Five extension mains (`ext-{demo,walls,props,av,rec}`) duplicate the same
lifecycle boilerplate 5x: `envOr`, `publishHealth`, signal context, OTEL
init, NATS connect with identical options, `worldsim.ready` wait + register
protocol, heartbeat loop, and the `registerMsg`/`optionFieldDef` types.

## Approach

Helper library, not a framework. `extkit` exposes functions and types that
each main calls inline. No `Run(spec)` lifecycle container — each main
stays in control of its own lifecycle. This avoids forcing ext-rec's
unique lifecycle (worldOptions, dynamic semaphore, auto-stop ticker,
orphan reconciliation) into a one-size-fits-all shape.

## API

### Tier 1: trivial helpers

```go
func EnvOr(key, fallback string) string
func PublishHealth(nc *nats.Conn, service string, startTime time.Time)
func ConnectNATS(url, extID string) (*nats.Conn, error)

type RegisterMsg struct {
    ExtensionID        string           `json:"extension_id"`
    HeartbeatIntervalS int              `json:"heartbeat_interval_s"`
    OptionsSchema      []OptionFieldDef `json:"options_schema,omitempty"`
}

type OptionFieldDef struct {
    Name    string          `json:"name"`
    Type    string          `json:"type"`
    Default json.RawMessage `json:"default"`
}
```

### Tier 2: callback-shaped helpers

```go
// WaitForReady subscribes to worldsim.ready, calls onReady with the map
// name (or "" on timeout), and returns. onReady does extension-specific
// init + registration in both cases.
func WaitForReady(nc *nats.Conn, logger *slog.Logger, timeout time.Duration, onReady func(mapName string))

// HeartbeatLoop runs until ctx.Done. Publishes heartbeat every tick,
// calls onReRegister every 3rd tick, publishes health every tick.
func HeartbeatLoop(ctx context.Context, nc *nats.Conn, extID string, heartbeatS int, onReRegister func())

// SubscribeOptions subscribes to extension.{id}.options, unmarshals into
// opts (pointer), rolls back on parse error, calls onReload on success.
func SubscribeOptions(nc *nats.Conn, extID string, opts any, mu *sync.Mutex, logger *slog.Logger, onReload func()) error
```

## What stays in each main

- Extension-specific option structs (`demoOptions`, `wallsOptions`, etc.)
- Gameplay NATS subscriptions (zone events, action handlers, recording)
- ext-rec: worldOptions pattern, dynamic semaphore, auto-stop ticker
- ext-av: LiveKit credential validation
- ext-walls/ext-props: action protocol types (`Effect`,
  `adjacentEntityInfo`, `actionDispatchMsg`, `actionReplyMsg`) — these
  are protocol types shared between two extensions, not lifecycle
  boilerplate. ext-props extends them with light fields.

## Migration

One extension at a time, ext-demo first (simplest). Each main deletes its
local copies of the boilerplate and calls extkit. Build + test after each.

## Testing

extkit test uses embedded NATS (pattern from `cmd/mcp/server_test.go`)
verifying:
- ConnectNATS connects and closes
- WaitForReady timeout path calls onReady("")
- HeartbeatLoop exits on ctx cancellation
- SubscribeOptions unmarshals, rolls back on bad JSON, calls onReload
