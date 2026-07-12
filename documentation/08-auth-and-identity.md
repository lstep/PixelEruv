# Authentication and Identity

This document specifies how users are authenticated, how their identity is
mapped to an in-world entity, and how that identity is validated at each
trust boundary. It builds on `04-tech-stack.md` (technology choices),
`05-architecture.md` (component wiring), and
`06-data-model-and-persistence.md` (where identity data is stored).

---

## 1. Principles

- **Dex is the single OIDC token issuer.** Every human user and every
  protected service call is authenticated via a JWT signed by Dex. No other
  component issues tokens.
- **The Pusher is the single validation point for game-state access.** It
  validates the token on the WebSocket upgrade and extracts the `sub` claim.
  It does not do identity provisioning — it forwards the validated `sub` to
  the World Simulator via NATS. Downstream components (NATS, LiveKit Bridge)
  trust the Pusher's validation within the same internal Docker network.
  PocketBase is embedded in the World Simulator as a Go library and receives
  the validated `sub` in-process (see §5). None of these are reachable from
  outside.
- **The World Simulator maps identity to entities.** It receives the
  validated `sub` from the Pusher, looks up or creates the user in
  PocketBase, and registers the entity in the ECS. The Pusher never touches
  PocketBase.
- **PocketBase is never in the auth path.** It stores identity metadata
  (avatar appearance, preferences) but never issues or validates tokens.
- **Zone isolation is enforced by the World Simulator, not by the client.**
  The OIDC identity is the root of that enforcement: the World Sim maps
  `sub` → entity ID → zone membership → permissions.

---

## 2. Dex as the OIDC provider

### Role

Dex is a federation bridge. It accepts credentials from any configured
upstream identity source and issues standard OIDC JWTs downstream. The rest
of the application only ever sees Dex-signed tokens and does not need to know
which upstream was used.

### Supported upstream connectors (Dex built-in)

| Connector | Use case |
|---|---|
| **LDAP** | On-premise Active Directory, OpenLDAP |
| **OIDC** | Azure AD / Entra ID, Google Workspace, any OIDC-compliant IdP |
| **SAML** | Legacy enterprise IdPs |
| **GitHub / GitLab** | Developer-focused teams |
| **Local password** | Small deployments without an external IdP |

The connector(s) to enable are configured in Dex's single YAML file at
deploy time. The application code does not change between connector types.

### Dex deployment

- Runs as a **standalone Docker Compose service** (`dex`).
- Persists OAuth sessions and connector state in its own **SQLite volume**
  (`dex-data`). This volume is separate from the World Simulator's data
  (PocketBase is embedded in worldsim, see §5).
- Exposes its OIDC discovery endpoint at `/.well-known/openid-configuration`
  on an internal port, routed through Traefik for the browser-facing flows.
- The **JWKS endpoint** (`/keys`) is consumed by the Pusher at startup and
  cached locally for token validation (see § 4).

### Token claims used by the application

| Claim | Type | Used for |
|---|---|---|
| `sub` | string | Stable unique user identifier; join key to PocketBase `users.user_id` |
| `email` | string | Display fallback if no `display_name` exists in PocketBase |
| `name` | string | Pre-filled `display_name` on first login |
| `exp` | int (Unix) | Token expiry; enforced on upgrade and on refresh |
| `iss` | string | Verified to match Dex's issuer URL to reject foreign tokens |

No other claims are required or trusted by the application.

---

## 3. Authentication flows

### 3.1 Human user login (browser)

```
Browser            Traefik          Dex           Pusher        NATS        WorldSim    PocketBase
   |                  |              |              |             |            |            |
   |-- GET /login --> |              |              |             |            |            |
   |              (routes to Dex)   |              |             |            |            |
   |<-- redirect to Dex login ------+              |             |            |            |
   |                                |              |             |            |            |
   |-- credentials (LDAP, OAuth2) ->|              |             |            |            |
   |<-- authorization code ---------+              |             |            |            |
   |                                |              |             |            |            |
   |-- code exchange -------------->|              |             |            |            |
   |<-- OIDC JWT (id_token) --------+              |             |            |            |
   |                                               |             |            |            |
   |-- WS upgrade ----------------->|------------->|             |            |            |
   |                                  (waiting)     |             |            |            |
   |-- AUTH frame (id_token) ---------------------->|             |            |            |
   |                                  validate      |             |            |            |
   |                                  sig + exp     |             |            |            |
   |                                  + iss + aud   |             |            |            |
   |                                  extract sub   |             |            |            |
   |                                               |-- client.connected -->|   |            |
   |                                               |  (sub, client_id)    |   |            |
   |                                               |             |       |-- lookup sub->|
   |                                               |             |       |<-- user rec --|
   |                                               |             |       |   (or create)|
   |                                               |             |       |-- restore pos->(KV)
   |                                               |             |       |   register ECS|
   |                                               |             |       |   compute AOI|
   |                                               |<-- replication batch --|            |
   |<-- WS established + initial world snapshot ---|<-----------|            |            |
```

> **Note:** PocketBase is no longer a separate Docker service. It is embedded
> in the World Simulator as a Go library; the "lookup sub" and "user rec"
> steps above are in-process DAO calls, not HTTP requests. worldsim serves
> PB's HTTP API on port 8090 (for the admin UI and extensions that still
> access PB via HTTP — see `18-extensions.md`).

### 3.2 Token delivery on WebSocket upgrade

The browser's native `WebSocket` API does not support custom HTTP headers.
The OIDC `id_token` is delivered as the **first WebSocket message** after
the connection is opened — a dedicated `AUTH` control frame sent before any
game frame.

**[DECISION]** First-frame `AUTH` is preferred over a URL query parameter
(`?token=`) because it keeps the token out of Traefik access logs, which
matters for enterprise deployments with strict audit requirements. The `AUTH`
frame format will be defined in `07-network-protocol.md`.

The Pusher **does not send any game state** until it has received and
validated a well-formed `AUTH` frame. If no `AUTH` frame arrives within
**5 seconds** of the WebSocket upgrade, the Pusher closes the connection
with `4401 Unauthorized`.

Once the token is validated, the Pusher publishes a `client.connected` event
to NATS Core (containing the `sub`, a generated `client_id`, the client IP,
and the `device_id` from the `AUTH` frame). The World
Simulator receives this event, performs the identity → entity provisioning
(see §5), and publishes the initial replication batch back to the Pusher via
NATS. The Pusher forwards it to the client.

### 3.3 Token lifetime and refresh

- `id_token` lifetime: **15 minutes** (configured in Dex). Short enough to
  limit the blast radius of a leaked token.
- The browser must obtain a new `id_token` before expiry using the
  `refresh_token` issued by Dex alongside the `id_token`.
- **The WebSocket connection must not be dropped on token refresh.** The
  refresh happens in the background (Phaser client, outside the game loop).
  Once a new `id_token` is obtained, the client sends it to the Pusher via a
  dedicated `TOKEN_REFRESH` control frame (to be defined in
  `07-network-protocol.md`). The Pusher re-validates and updates the session
  in-memory. No reconnect required.
- If the refresh fails (e.g. the `refresh_token` is revoked by the admin),
  the Pusher closes the WebSocket with a `4401 Unauthorized` close code and
  the client redirects to the Dex login page.

---

## 4. Token validation in the Pusher

### JWKS caching

On startup, the Pusher fetches Dex's JWKS from the discovery endpoint and
caches the public keys in memory. It refreshes the JWKS cache every
**10 minutes** to pick up key rotations without restarting.

Validation steps for every token (upgrade + `TOKEN_REFRESH` frames):

1. Verify the JWT signature against the cached JWKS.
2. Verify `iss` matches the configured Dex issuer URL.
3. Verify `exp` is in the future (clock skew tolerance: 30 seconds).
4. Verify `aud` contains the application's client ID.

If any step fails: reject with `4401`. No game state is sent.

---

## 5. Identity → entity mapping (first login provisioning)

The Pusher validates the token and extracts the `sub` claim, but **does not
do identity provisioning**. That is the responsibility of the **World
Simulator**, which is the only service that accesses PocketBase and JetStream
KV.

When the Pusher publishes a `client.connected` event to NATS Core (containing
the validated `sub` and a `client_id`), the World Simulator:

1. Queries **PocketBase** `users` by `user_id`:
   - **First login**: creates the `users`, `avatar_appearance`, and
     `user_preferences` records; assigns a new `entity_id` (a string-encoded
     ID assigned by the ECS).
   - **Returning user**: reads existing profile and appearance.
2. Reads **NATS JetStream KV** `users.<entity_id>.position`:
   - If present: spawns the entity at the stored position.
   - If absent (first login or expired): spawns at the world default spawn
     point from PocketBase `worlds`, and writes the initial position to KV.
3. Registers the entity in the ECS and computes the initial world snapshot
   for the client's AOI.
4. Publishes the initial replication batch to NATS Core (subject
   `client.<client_id>.replication`). The Pusher forwards it to the browser.
5. After provisioning, the World Sim publishes a `client.provisioned` event to
   NATS Core (with `client_id`, `entity_id`, and initial `zone_id`). The
   LiveKit Bridge subscribes to this event to issue a LiveKit room token
   (see §6).

This provisioning happens **asynchronously** — the Pusher does not block the
WebSocket while the World Sim processes the `client.connected` event. The
client sees a brief delay (< 100 ms budget: one PocketBase read + one KV read
on returning login; two PB writes + one KV write on first login) before
receiving the initial snapshot.

---

## 6. LiveKit token issuance

LiveKit requires its own room-scoped JWT (distinct from the OIDC token) to
authorise a participant to join a room. This token is issued by the **LiveKit
Bridge**, not by Dex:

1. After the World Simulator provisions the entity (end of § 5), it publishes
   a `client.provisioned` event to NATS Core, including the `client_id`,
   `entity_id`, and current `zone_id`. The LiveKit Bridge subscribes to this
   subject.
2. The LiveKit Bridge signs the token with the LiveKit API secret and publishes
   it to NATS Core on subject `client.<client_id>.control`.
3. The Pusher subscribes to `client.<client_id>.control` and forwards the
   control frame (containing the LiveKit token) to the client over WebSocket.
4. The Phaser client passes the LiveKit token directly to the LiveKit JS SDK
   to join the SFU room.

When a user moves between zones, the zone's owning extension writes the new
zone state to JetStream KV. The LiveKit Bridge reacts via `kv.Watch`, issues a
new room token, and publishes it to `client.<client_id>.control`. The Pusher
forwards it to the client.

---

## 7. NPC and service-account authentication

NPCs (bots, AI agents) are out of MVP scope but the architecture must not
preclude them.

**Proposed approach (to be confirmed in `13-ecs-design.md`):**

- NPC processes authenticate using a **Dex static password connector** entry
  — a dedicated service account with a non-expiring `refresh_token` stored
  in a Docker secret.
- NPCs are driven by **extensions**, not by the World Sim's in-process
  systems. An NPC extension registers with the World Sim via NATS and
  spawns/updates NPC entities (see `18-extensions.md`). The extension itself
  may authenticate with Dex using a service account to access protected
  resources (e.g. LLM APIs), but the NPC entity does not need an OIDC token —
  it is not a WebSocket client.
- The NPC's `entity_id` is assigned by the extension when it spawns the
  entity. If the NPC needs a persistent identity (e.g. for audit logs), the
  extension can create a record in PocketBase at deploy time with a flag
  `is_npc: true`.
- NPCs have no `NetworkSession` component in the ECS; their behavior is driven
  by the extension via NATS updates (see `18-extensions.md`).

> NPCs are extension-driven by design (see `18-extensions.md`). The auth
> model above covers the extension's own authentication if it needs to access
> protected resources.

---

## 8. Admin access

The PocketBase admin dashboard (superuser) is served by the World Simulator
on port 8090 — PocketBase is embedded in worldsim as a Go library, not a
separate container. It is not routed through Traefik. It is protected by
PocketBase's own superuser password (set at first-run via the
`PB_ADMIN_EMAIL` / `PB_ADMIN_PASSWORD` environment variables, consumed by
worldsim's initial superuser migration). It is used solely for:

- Inspecting and editing durable data (user profiles, world config).
- Schema migrations.
- Audit log queries.
- Issuing bans (adding records to the `bans` collection — see §9).

It has no connection to Dex or to the game-state auth flow.

---

## 9. Ban system

The ban system lets admins block griefers by any of three identifiers,
providing defense in depth against evasion.

### Identifiers

| Target type | Identifier | Strength | Covers |
|---|---|---|---|
| `user_id` | PocketBase user ID | Robust — evading requires a new account | Logged-in users |
| `ip` | Client IP address | Coarse — collateral damage on shared IPs (NAT, household) | Everyone |
| `device_id` | Client-generated UUID in `localStorage` | Weak — evadable by clearing storage or incognito | Primarily guests |

The `device_id` is generated by the browser on first visit
(`crypto.randomUUID()`) and stored in `localStorage`. It is sent in the
`AuthFrame` alongside the `id_token`. It is stable across sessions for the
same browser but not across devices. It is the weakest layer — its purpose
is to catch casual griefers who haven't cleared their storage, especially
when IP-banning would hit innocent users on the same network.

### Ban records

Bans are stored in the PocketBase `bans` collection (see
`06-data-model-and-persistence.md`). Each record specifies a `target_type`,
`target_value`, optional `reason`, and optional `banned_until` (unix
timestamp; 0 or empty = permanent). Bans are currently issued via the
PocketBase admin dashboard. An in-game admin ban command is planned.

### Ban check

The world simulator checks the ban list during `provisionClient`, after
the PocketBase user lookup that determines `isAdmin`:

1. The pusher extracts `sub`, `ip`, and `device_id` from the `AuthFrame`
   and forwards them to worldsim via the `client.connected` NATS message.
2. Worldsim looks up the user in PocketBase by `user_id` (for logged-in
   users) and determines `isAdmin`.
3. If `isAdmin` is true, the ban check is skipped — admins are always
   exempt.
4. Otherwise, worldsim queries the `bans` collection for active bans
   matching any of the three identifiers. An active ban is one where
   `banned_until` is 0 (permanent) or greater than the current time.
5. If a ban matches, worldsim returns `banned: true` with the reason and
   expiry in the `AuthResultFrame` reply. The pusher sends this to the
   browser and closes the WebSocket. No entity is created.
6. If no ban matches, provisioning proceeds normally.

### Browser behavior

When the browser receives an `AuthResultFrame` with `ok: false` and a
non-empty `ban_reason`, it displays the ban reason and expiry in the
disconnect overlay and stops attempting to reconnect. The client does not
clear the stored `id_token` or `device_id` — the ban is server-side, not
a token issue.

---

## Open questions

- **[RESOLVED] NPC driving mechanism**: NPCs are extension-driven via NATS
  (see `18-extensions.md`). There is no in-process injection.
- **[OPEN] Dex connector selection per deployment**: the Docker Compose setup
  should ship with a documented `dex-config.yaml` template covering LDAP,
  Microsoft OIDC, and local-password connectors. To be created alongside
  `20-roadmap.md`.
- **[DECISION] Token revocation on admin kick**: the revocation **policy**
  (who can kick, under what conditions) is deployment-specific and lives in an
  admin extension. The revocation **execution** is in the World Sim: when the
  admin extension triggers a kick (via NATS), the World Sim publishes
  `admin.revoke.<entity_id>` on Core NATS. The Pusher subscribes to this
  subject and immediately closes the matching WebSocket with
  `4401 Unauthorized`. This provides instant eviction without waiting for the
  15-minute `id_token` expiry. The World Sim also removes the entity from the
  ECS and tears down its zone membership. For preventing a kicked user from
  reconnecting, see the ban system (§9).
