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
            W->>PB: lookup users by user_id (or create on first login)
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

## 3. Interaction with input trigger broadcast

An `ActionFrame` carries an `input` (e.g. `click:left`, `key:E`,
`action:execute`) and optional `entity_id`/`action_id` (for popup-mode
choices). The World Simulator computes contextual data (range, LOS, entities
on tile / adjacent entities, target entities from `interactions` target_ids,
equipment snapshot) and broadcasts to all extensions that registered for that
input type. Each extension self-filters and replies asynchronously. All
replies within the timeout are applied. The kernel has no TriggerSystem — all
interaction behavior is in extensions.

For the two-phase interaction flow (see
`documentation/plans/2026-07-15-interaction-system-design.md`), the first
`key:E` press may return `available_actions` (popup mode) without executing
effects. The user picks an action, and the client sends a second
`ActionFrame` with `input: "action:execute"`, `entity_id`, and `action_id`.

```mermaid
sequenceDiagram
    autonumber
    actor B as Browser
    participant P as Pusher
    participant N as NATS
    participant W as WorldSim
    participant E as Extension(s)

    B->>P: ActionFrame { seq, input: "key:E" }
    P->>N: publish client.<client_id>.input
    N->>W: deliver ActionFrame

    W->>W: look up extensions registered for "key:E"

    alt no extension registered
        W-->>N: publish client.<client_id>.replication (ActionResultFrame { ok: false, reason: "no_handler" })
        N->>P: deliver batch
        P-->>B: ServerFrame.action_result
    else extensions registered
        W->>W: compute adjacent_entities, target_entities (from interactions), equipment snapshot
        W->>N: publish extension.<ext_id>.action { source_entity_id, input, adjacent_entities, target_entities, entity_id, action_id }
        N->>E: deliver to all registered extensions

        par Each extension self-filters and replies
            E->>E: self-filter by owner_extension, process effects (toggle, set_state, ...)
            E->>N: reply { handled, updates, appearance_updates, animations, available_actions }
            N->>W: deliver reply
        end

        Note over W: collect all replies within timeout
        W->>W: apply updates + appearance_updates + animations → dirty flags
        W-->>N: publish client.<client_id>.replication (ActionResultFrame { ok: true, available_actions: [...] })
        N->>P: deliver batch
        P-->>B: ServerFrame.action_result

        opt available_actions non-empty (popup mode)
            B->>B: show interaction popup with available_actions
            B->>P: ActionFrame { seq, input: "action:execute", entity_id, action_id }
            P->>N: publish client.<client_id>.input
            N->>W: deliver ActionFrame
            W->>N: publish extension.<ext_id>.action { ..., action_id }
            N->>E: deliver to extension
            E->>N: reply { handled, updates, appearance_updates, animations }
            N->>W: deliver reply
            W->>W: apply updates → dirty flags
            W-->>N: publish replication (ActionResultFrame { ok: true })
            N->>P: deliver batch
            P-->>B: ServerFrame.action_result
        end
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
extension. The revocation **execution** is in the World Sim: it despawns
the entity and publishes `client.<client_id>.force_close` with a marshaled
`ServerFrame` (`AuthResult{kicked=true, kick_reason}`); the Pusher instance
owning that client forwards the frame to the WebSocket, then closes it so
the browser shows the "kicked" overlay and stops reconnecting.

Kick can be triggered three ways: MCP server (`worldsim.client.kick` by
client_id or entity_id), admin browser dropdown (KickFrame → pusher →
`worldsim.client.kick`), or ban (`worldsim.client.ban` — despawn +
force_close the matching client).

```mermaid
sequenceDiagram
    autonumber
    participant Admin as Admin (MCP / browser dropdown / ban)
    participant W as WorldSim
    participant N as NATS
    participant P as Pusher (owning the client)
    participant B as Browser (target)

    Admin->>N: worldsim.client.kick { client_id or entity_id, reason }
    N->>W: deliver kick request
    W->>W: despawnClient (save position, zone.exit, proximity.leave, DestroyEntity)
    W->>N: publish client.<client_id>.force_close { AuthResult{kicked=true, kick_reason} }
    N->>P: deliver force_close to the owning Pusher

    Note over P: Pusher finds the session in its map
    alt matching client_id on this Pusher
        P-->>B: ServerFrame { AuthResult { kicked: true, kick_reason } }
        P-->>B: WS close (policy violation)
        Note over B: show "You have been kicked" overlay, stop reconnecting
    else no match on this instance
        Note over P: no-op (session already gone)
    end

    Note over W: also publishes client.disconnected via the normal close path
```

---

## 7. Dual-connect confirmation

When a logged-in user opens a second browser window, both windows mint the
same persistent `entity_id` (from PocketBase). Without detection, the
second `provisionClient` silently overwrites the first entity, and the old
window becomes a frozen zombie (drops input, receives no replication,
oscillates position saves). The dual-connect flow detects this and asks
the user to confirm before displacing the old session.

```mermaid
sequenceDiagram
    autonumber
    participant B1 as Browser (old window)
    participant B2 as Browser (new window)
    participant P as Pusher
    participant W as WorldSim
    participant N as NATS

    Note over B1: already connected, entity e_user active on c_old

    B2->>P: WS connect + AuthFrame { id_token, force=false }
    P->>N: client.connected { client_id=c_new, sub, force=false }
    N->>W: deliver client.connected
    W->>W: provisionClient: entityID e_user already in s.entities with c_old active
    Note over W: dual-connect detected (same entityID, different clientID, old still in s.clients)
    W-->>N: reply AuthResult { ok=false, already_connected=true }
    N-->>P: deliver reply
    P-->>B2: ServerFrame { AuthResult { ok=false, already_connected=true } }
    Note over B2: show confirm popup: "Already connected in another window. Connect here?"

    alt user clicks "Yes"
        B2->>P: AuthFrame { id_token, force=true }
        P->>N: client.connected { client_id=c_new, sub, force=true }
        N->>W: deliver client.connected (force=true)
        W->>W: despawnClientLocked(c_old): save position, zone.exit, proximity.leave, DestroyEntity
        W->>W: provision new entity for c_new
        W->>N: publish client.c_old.force_close { AuthResult{kicked=true, kick_reason} }
        N-->>P: deliver force_close for c_old
        P-->>B1: ServerFrame { AuthResult { kicked=true, kick_reason } }
        P-->>B1: WS close (policy violation)
        Note over B1: show "You have been kicked" overlay, stop reconnecting
        W-->>N: reply AuthResult { ok=true, entity_id=e_user, ... }
        N-->>P: deliver reply
        P-->>B2: ServerFrame { AuthResult { ok=true, entity_id, map_id, ... } }
        Note over B2: load game (normal connect flow continues)
    else user clicks "No"
        B2->>P: WS close
        Note over B2: stay on loading screen
    end
```

The reconnect race (old session already gone from `s.clients` before the
new `provisionClient` runs) is NOT a dual-connect: `provisionClient`
treats it as a normal reconnect and reuses the stale entity via the
`removeStaleMobileZone` path. See `worldsim_reconnect_race_test.go` and
`worldsim_dualconnect_test.go`.

---

## Coverage and gaps

These seven cover the complete client-facing networking story: connect,
steady state, interact, recover, refresh, revoke, dual-connect.

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
