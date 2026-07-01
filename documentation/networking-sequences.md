# Networking Sequences

End-to-end sequence diagrams for the client-facing networking flows.
Companion to `networking-flow.svg` (station view) and the prose in
`07-network-protocol.md`, `08-auth-and-identity.md`, `09-pusher.md`,
`11-replication.md`.

> Actors are services, not goroutines. `NATS` is the Core bus;
> `KV` is JetStream KV (same process, drawn separately for clarity).
> All client ↔ Pusher frames are binary protobuf `ClientFrame` / `ServerFrame`
> over a single WSS connection.

---

## 1. Connection handshake

From WebSocket open to steady state. Covers the 5 s auth timeout, the
`4401` failure branch, and the asynchronous entity provisioning + initial
snapshot.

```mermaid
sequenceDiagram
    autonumber
    actor B as Browser
    participant T as Traefik
    participant P as Pusher
    participant D as Dex
    participant N as NATS
    participant W as WorldSim
    participant PB as PocketBase
    participant KV as KV (JetStream)

    Note over B: user completes OIDC login with Dex (out of band)
    B->>T: WS upgrade (WSS)
    T->>P: route (sticky session)
    Note over P: start 5 s auth timer

    B->>P: AuthFrame { id_token }
    P->>D: (cached JWKS, refreshed every 10 min)
    Note over P: verify sig + iss + exp + aud, extract sub

    alt token invalid / 5 s timeout
        P-->>B: ErrorFrame { code: 4401 }
        P-->>B: WS close 4408 (timeout) or 4401 (bad token)
    else token valid
        P->>P: assign client_id, register session
        P->>N: publish client.connected { client_id, sub, pusher_instance }
        N->>W: deliver client.connected

        par WorldSim provisioning (async, < 100 ms budget)
            W->>PB: lookup users by oidc_sub (or create on first login)
            PB-->>W: user rec + avatar + preferences
            W->>KV: read users.<entity_id>.position (or default spawn)
            KV-->>W: stored position
            W->>W: register entity in ECS, compute AOI snapshot
        end

        W->>N: publish client.<client_id>.replication (ReplicationBatch of SpawnEntity × N)
        N->>P: deliver batch
        P-->>B: AuthResultFrame { ok, client_id, entity_id }
        P-->>B: ServerFrame.replication (initial snapshot)
        Note over B: create local entities, enter steady state
    end
```

---

## 2. Steady-state input + replication loop

The hot loop, repeated every client input and every World Sim tick (20 Hz).
Shows why the echoed `seq` enables client-side prediction reconciliation.

```mermaid
sequenceDiagram
    autonumber
    actor B as Browser
    participant P as Pusher
    participant N as NATS
    participant W as WorldSim

    Note over B,W: per client input (on change or every tick while held — TBD)

    B->>B: capture InputState, seq++
    B->>P: InputFrame { seq, client_tick, state }
    Note over P: pass-through, no interpretation
    P->>N: publish client.<client_id>.input
    N->>W: deliver InputFrame

    Note over W: apply authoritatively in next tick
    W->>W: PlayerMovementSystem: compute target tile, evaluate triggers, update Position → dirty flag set

    Note over W: replication scheduler (once per tick, per client)
    W->>W: collect dirty components
    W->>W: AOI filter (same map + radius)
    W->>W: delta encode: Spawn / Update / Destroy
    W->>W: serialize components by component_id (protobuf)
    W->>W: build ReplicationBatch { snapshot_seq, messages[] }

    W->>N: publish client.<client_id>.replication
    N->>P: deliver batch
    P-->>B: ServerFrame.replication (single WS frame)

    Note over B: decode batch
    Note over B: own avatar: reconcile prediction by echoed seq
    Note over B: other entities: snapshot interpolation
    Note over B: detect snapshot_seq gaps → request full snapshot (see §4)
```

---

## 3. Interaction with extension routing

An `ActionFrame` targets a tile the player clicks (or the facing tile for
keypress interactions — `InteractFrame` has been deprecated and replaced by
`ActionFrame`). The World Simulator validates range and line-of-sight (if
action triggers require them), then routes: action triggers on the clicked
tile take priority; if none exist, the kernel falls back to entity interaction
routing based on the `ExtensionOwner` component or entity-bound `notify`
triggers. The kernel has no TriggerSystem — all interaction behavior is in
extensions.

```mermaid
sequenceDiagram
    autonumber
    actor B as Browser
    participant P as Pusher
    participant N as NATS
    participant W as WorldSim
    participant E as Extension

    B->>P: ActionFrame { seq, target_map_id, target_x, target_y, params }
    P->>N: publish client.<client_id>.input
    N->>W: deliver ActionFrame

    W->>W: look up action triggers on clicked tile

    alt action trigger exists
        W->>W: validate range + LOS (Bresenham raycast)
        alt validation fails
            W-->>N: publish client.<client_id>.replication (ActionResultFrame { ok: false, reason })
            N->>P: deliver batch
            P-->>B: ServerFrame.action_result
        else validation passes
            W->>N: publish trigger.<trigger_id>.action { equipment snapshot, reply_to }
            N->>E: deliver action
            E->>E: run custom logic (based on equipment)
            E->>N: publish trigger.<trigger_id>.action.reply.<req_id> { updates, consume_items }
            N->>W: deliver reply
            Note over W: apply reply → dirty flags
        end
    else no action trigger, entity on tile
        W->>W: fallback to entity interaction routing
        alt entity has ExtensionOwner or notify trigger
            W->>N: publish entity.<entity_id>.interact { req_id, params }
            N->>E: deliver interaction
            E->>E: run custom logic
            E->>N: publish entity.<entity_id>.interact.reply.<req_id>
            N->>W: deliver reply
            Note over W: apply reply → dirty flags
        else no ExtensionOwner and no notify trigger
            W-->>N: publish ActionResultFrame { ok: false, reason: "no_target" }
            N->>P: deliver batch
            P-->>B: ServerFrame.action_result
        end
    else no action trigger, no entity
        W-->>N: publish ActionResultFrame { ok: false, reason: "no_target" }
        N->>P: deliver batch
        P-->>B: ServerFrame.action_result
    end

    Note over W: result flows back to client via the §2 replication loop
    W->>N: publish client.<client_id>.replication (UpdateComponent / PlayAnimation)
    N->>P: deliver batch
    P-->>B: ServerFrame.replication
```

---

## 4. Reconnect and snapshot recovery

Two recovery paths: (a) WebSocket drop with sticky-session reconnect to the
same Pusher, and (b) in-stream `snapshot_seq` gap detection without a drop.

```mermaid
sequenceDiagram
    autonumber
    actor B as Browser
    participant T as Traefik
    participant P as Pusher
    participant N as NATS
    participant W as WorldSim

    Note over B,P: (a) WebSocket dropped

    B->>T: WS upgrade (WSS) — reconnect
    T->>P: route (sticky → same Pusher instance)
    B->>P: AuthFrame { id_token }
    Note over P: re-validate (JWKS), same client_id rebinds to new ws
    P->>N: publish client.connected { client_id, sub, pusher_instance }
    N->>W: deliver client.connected

    Note over W: entity was never gone — World Sim kept it alive
    W->>W: recompute AOI snapshot for current position
    W->>N: publish client.<client_id>.replication (full SpawnEntity × N)
    N->>P: deliver batch
    P-->>B: AuthResultFrame + full snapshot
    Note over B: reconcile local store, resume steady state

    Note over B,W: ─────────────────────────────────────
    Note over B,W: (b) in-stream snapshot_seq gap (no drop)

    W-->>N: publish batch (snapshot_seq = k)
    N-->>P: deliver
    P-->>B: ServerFrame.replication
    Note over B: record last_seq = k

    W-->>N: publish batch (snapshot_seq = k+2)  %% k+1 lost on NATS Core hop
    N-->>P: deliver
    P-->>B: ServerFrame.replication
    Note over B: detect gap (expected k+1, got k+2)

    B->>P: ControlFrame (snapshot request)  %% [OPEN] exact frame TBD
    P->>N: publish client.<client_id>.input (snapshot request)
    N->>W: deliver request

    W->>W: rebuild full AOI snapshot
    W->>N: publish client.<client_id>.replication (SpawnEntity × N)
    N->>P: deliver
    P-->>B: full snapshot
    Note over B: spawns/destroys reconciled authoritatively
```

---

## 5. Token refresh

Background refresh that must not drop the WebSocket. The `id_token` lifetime
is 15 minutes; the client obtains a fresh one via Dex's `refresh_token` and
sends it in-band.

```mermaid
sequenceDiagram
    autonumber
    actor B as Browser
    participant D as Dex
    participant P as Pusher
    participant N as NATS

    Note over B: id_token nearing 15 min expiry
    Note over B: refresh happens outside the game loop (Phaser client)

    B->>D: refresh_token exchange (HTTP, not WS)
    alt refresh_token revoked by admin
        D-->>B: error
        B->>B: redirect to Dex login page
    else refresh succeeds
        D-->>B: new id_token
        B->>P: TokenRefreshFrame { id_token }
        Note over P: re-validate sig + iss + exp + aud (same JWKS cache)
        alt invalid
            P-->>B: ErrorFrame { code: 4401 }
            P-->>B: WS close 4401
            B->>B: redirect to Dex login
        else valid
            P->>P: update session in-memory (no reconnect)
            Note over B,P: WebSocket stays open — game loop uninterrupted
        end
    end
```

---

## 6. Admin kick / revocation

Instant eviction without waiting for the 15-minute `id_token` expiry. The
revocation **policy** (who can kick, under what conditions) is in an admin
extension. The revocation **execution** is in the World Sim: it publishes
`admin.revoke.<entity_id>`; every Pusher instance subscribes and closes the
matching WebSocket.

```mermaid
sequenceDiagram
    autonumber
    participant Admin as Admin (via admin extension)
    participant E as Admin Extension
    participant W as WorldSim
    participant N as NATS
    participant P as Pusher (all instances)
    participant B as Browser (target)

    Admin->>E: revoke user (via admin extension API)
    E->>N: publish admin kick request (to World Sim)
    N->>W: deliver kick request
    W->>W: remove entity from ECS, tear down zone membership
    W->>N: publish admin.revoke.<entity_id> { entity_id, reason }
    N->>P: deliver to every Pusher instance

    Note over P: each Pusher checks its session map
    alt matching client_id on this Pusher
        P-->>B: ErrorFrame { code: 4401, message: "revoked" }
        P-->>B: WS close 4401
        Note over B: redirect to Dex login
    else no match on this instance
        Note over P: no-op (another instance holds the session)
    end

    Note over W: also publishes client.disconnected via the normal close path
```

---

## Coverage and gaps

These six cover the complete client-facing networking story: connect,
steady state, interact, recover, refresh, revoke.

Not yet diagrammed (available on request):

- **Extension lifecycle** — `extension.register` → `registered` →
  `register_components` → `register_triggers` → `register_zone` → `spawn` →
  `batch_update` → `interact` routing → `despawn` → `deregister` + heartbeat.
  Different actor set (Extension ↔ WorldSim, no client).
- **LiveKit media token issuance** — `client.provisioned` → Bridge signs
  room JWT → `client.<client_id>.control` → `ControlFrame.livekit_token` →
  client joins SFU. Media plane handoff.
- **Cross-shard entity transfer** — `world.<shard_id>.volatile` between
  World Sim shards. Niche; defer unless sharding is being designed actively.
```
