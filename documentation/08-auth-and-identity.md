# Authentication and Identity

This document specifies how users are authenticated, how their identity is
mapped to an in-world entity, and how that identity is validated at each
trust boundary. It builds on `04-tech-stack.md` (technology choices),
`05-architecture.md` (component wiring), and
`06-data-model-and-persistence.md` (where identity data is stored).

---

## 1. Principles

- **PocketBase is the single authentication provider.** Every human user
  registers and authenticates directly against PocketBase's built-in `users`
  auth collection (email/password, OAuth2 social login). PocketBase issues
  HS256-signed JWTs. No external identity provider is required.
- **The Pusher is the single validation point for game-state access.** It
  validates the PocketBase JWT on the WebSocket upgrade by calling the
  PocketBase API to verify the token and extract the user ID. It does not do
  identity provisioning — it forwards the validated `user_id` to the World
  Simulator via NATS. Downstream components (NATS, LiveKit Bridge) trust the
  Pusher's validation within the same internal Docker network. PocketBase is
  embedded in the World Simulator as a Go library (see §5).
- **The World Simulator maps identity to entities.** It receives the
  validated `user_id` from the Pusher, looks up or creates the player record
  in PocketBase, and registers the entity in the ECS. The Pusher never
  touches PocketBase for game-state auth (it only calls the PB API to
  validate tokens).
- **Zone isolation is enforced by the World Simulator, not by the client.**
  The PocketBase user ID is the root of that enforcement: the World Sim maps
  `user_id` → entity ID → zone membership → permissions.

---

## 2. PocketBase as the authentication provider

### Role

PocketBase is embedded in the World Simulator as a Go library. Its built-in
`users` auth collection handles registration, login, email verification,
password reset, and OAuth2 social login. PocketBase issues HS256-signed JWTs
that are used throughout the stack for authentication.

### Supported authentication methods

| Method | Use case |
|---|---|
| **Email/password** | Default. Self-service registration with email verification. |
| **Google OAuth2** | Social login. Enabled when `OAUTH2_GOOGLE_CLIENT_ID` / `OAUTH2_GOOGLE_SECRET` are set. |
| **GitHub OAuth2** | Social login. Enabled when `OAUTH2_GITHUB_CLIENT_ID` / `OAUTH2_GITHUB_SECRET` are set. |
| **Facebook OAuth2** | Social login. Enabled when `OAUTH2_FACEBOOK_CLIENT_ID` / `OAUTH2_FACEBOOK_SECRET` are set. |

OAuth2 providers are configured via environment variables on the `worldsim`
service. Leave them empty to disable social login. The application code does
not change between enabled/disabled providers.

### PocketBase auth deployment

- PocketBase is **embedded in the World Simulator** as a Go library — it is
  not a separate Docker service.
- The `users` auth collection is created by a Go migration
  (`1753300000_create_users_auth_collection.go`) compiled into the worldsim
  binary. No external migration files are needed.
- User data (credentials, verified status, OAuth2 links) is stored in
  PocketBase's SQLite database, in the same `pb_data` volume as game data.
- The **PocketBase admin UI** (`/_/`) is served by worldsim on port 8090 and
  proxied by the container nginx behind admin auth (see §8).
- SMTP for email verification and password reset is configured via
  `SMTP_HOST`, `SMTP_PORT`, `SMTP_FROM`, and `SMTP_SENDER_NAME` on the
  `worldsim` service. In dev, MailHog is used; in production, point at a
  real SMTP server.
- The `APP_URL` environment variable controls the base URL used in
  verification and password-reset email links.

### Token claims used by the application

| Claim | Type | Used for |
|---|---|---|
| `id` | string | PocketBase user ID; join key to `players.user_id` |
| `email` | string | Display fallback if no `display_name` exists in PocketBase |
| `exp` | int (Unix) | Token expiry; enforced on upgrade |
| `iss` | string | PocketBase issuer URL |

No other claims are required or trusted by the application.

---

## 3. Authentication flows

### 3.1 Human user registration and login (browser)

```
Browser            Nginx          PocketBase      Pusher        NATS        WorldSim
   |                 |           (in worldsim)      |             |            |
   |-- POST /api/users/auth-with-password --------->|             |            |
   |<-- JWT + user record -------------------------|             |            |
   |                                               |             |            |
   |-- WS upgrade --------------------------------->|------------>|            |
   |                                  (waiting)     |             |            |
   |-- AUTH frame (JWT) ---------------------------------------->|            |
   |                                  validate JWT via PB API     |            |
   |                                  extract user_id            |            |
   |                                               |-- client.connected -->|   |
   |                                               |  (user_id, client_id)|   |
   |                                               |             |       |-- lookup player->|
   |                                               |             |       |<-- player rec --|
   |                                               |             |       |   (or create)  |
   |                                               |             |       |-- restore pos->(KV)
   |                                               |             |       |   register ECS |
   |                                               |             |       |   compute AOI |
   |                                               |<-- replication batch --|
   |<-- WS established + initial world snapshot ---|<-----------|            |
```

> **Note:** PocketBase is embedded in the World Simulator as a Go library.
> The browser authenticates directly against PocketBase's HTTP API (proxied
> by nginx at `/api/`). The Pusher validates the JWT by calling the
> PocketBase API. The "lookup player" and "player rec" steps are in-process
> DAO calls within worldsim, not HTTP requests.

### 3.2 Token delivery on WebSocket upgrade

The browser's native `WebSocket` API does not support custom HTTP headers.
The PocketBase JWT is delivered as the **first WebSocket message** after
the connection is opened — a dedicated `AUTH` control frame sent before any
game frame.

**[DECISION]** First-frame `AUTH` is preferred over a URL query parameter
(`?token=`) because it keeps the token out of nginx access logs, which
matters for deployments with strict audit requirements. The `AUTH`
frame format is defined in `07-network-protocol.md`.

The Pusher **does not send any game state** until it has received and
validated a well-formed `AUTH` frame. If no `AUTH` frame arrives within
**5 seconds** of the WebSocket upgrade, the Pusher closes the connection
with `4401 Unauthorized`.

Once the token is validated, the Pusher publishes a `client.connected` event
to NATS Core (containing the `user_id`, a generated `client_id`, the client IP,
and the `device_id` from the `AUTH` frame). The World
Simulator receives this event, performs the identity → entity provisioning
(see §5), and publishes the initial replication batch back to the Pusher via
NATS. The Pusher forwards it to the client.

### 3.3 Token lifetime and refresh

- JWT lifetime: configured in PocketBase settings (default varies). Short
  enough to limit the blast radius of a leaked token.
- The browser must obtain a new JWT before expiry. The PocketBase JS SDK
  handles token refresh automatically using the stored auth token.
- **The WebSocket connection must not be dropped on token refresh.** The
  refresh happens in the background (Phaser client, outside the game loop).
  Once a new JWT is obtained, the client sends it to the Pusher via a
  dedicated `TOKEN_REFRESH` control frame (defined in
  `07-network-protocol.md`). The Pusher re-validates and updates the session
  in-memory. No reconnect required.
- If the refresh fails (e.g. the token is revoked by the admin),
  the Pusher closes the WebSocket with a `4401 Unauthorized` close code and
  the client redirects to the login page.

---

## 4. Token validation in the Pusher

### PocketBase API validation

The Pusher validates PocketBase JWTs by calling the PocketBase API
(`/api/collections/users/auth-refresh` or equivalent). This delegates
signature verification and expiry checks to PocketBase itself, ensuring
the token is still valid and not revoked.

Validation steps for every token (upgrade + `TOKEN_REFRESH` frames):

1. Call the PocketBase API with the JWT to verify it.
2. Extract the `user_id` from the validated token response.
3. Verify `exp` is in the future (clock skew tolerance: 30 seconds).

If any step fails: reject with `4401`. No game state is sent.

---

## 5. Identity → entity mapping (first login provisioning)

The Pusher validates the token and extracts the `user_id`, but **does not
do identity provisioning**. That is the responsibility of the **World
Simulator**, which is the only service that accesses PocketBase and JetStream
KV.

When the Pusher publishes a `client.connected` event to NATS Core (containing
the validated `user_id` and a `client_id`), the World Simulator:

1. Queries **PocketBase** `players` by `user_id`:
   - **First login**: creates the `players` record linked to the `users`
     record via `user_id`; assigns a new `entity_id` (a string-encoded
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

LiveKit requires its own room-scoped JWT (distinct from the PocketBase JWT) to
authorise a participant to join a room. This token is issued by the **LiveKit
Bridge**, not by PocketBase:

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

- NPC processes authenticate using a **PocketBase service account** — a
  dedicated `users` record with a long-lived token stored in a Docker secret.
- NPCs are driven by **extensions**, not by the World Sim's in-process
  systems. An NPC extension registers with the World Sim via NATS and
  spawns/updates NPC entities (see `18-extensions.md`). The extension itself
  may authenticate with PocketBase using a service account to access protected
  resources (e.g. LLM APIs), but the NPC entity does not need a JWT — it is
  not a WebSocket client.
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
separate container. It is proxied by the container nginx at `/_/` behind
admin auth (via the admin portal's `auth_request`). It is protected by
PocketBase's own superuser password (set at first-run via the
`PB_ADMIN_EMAIL` / `PB_ADMIN_PASSWORD` environment variables, consumed by
worldsim's initial superuser migration). It is used solely for:

- Inspecting and editing durable data (user profiles, world config).
- Schema migrations.
- Audit log queries.
- Issuing bans (adding records to the `bans` collection — see §9).

The admin portal (`/admin/`) is a standalone Go service that handles
email/password login against PocketBase and sets a signed session cookie.
nginx uses `auth_request` to check the cookie before proxying to `/_/`
(PB admin) and `/audit/`. Only users with `is_admin=true` in PocketBase
can log in to the admin portal.

### 8a. MCP server access (LLM admin tooling)

The MCP server (`/mcp`, `backend/cmd/mcp`) is a third admin surface,
distinct from the admin portal. It exposes world state, audit history,
and admin actions (kick, ban, teleport, chat-as, set_*) to LLM clients
(Claude Desktop, Devin, Cursor) over HTTP/SSE. Auth is a **bearer
token** (`MCP_AUTH_TOKEN` env, required — the server refuses to start
without it), NOT the admin portal cookie. nginx does NOT apply the
admin `auth_request` to `/mcp` — MCP clients present a bearer token in
the `Authorization` header, not a browser session cookie.

The bearer token is a shared secret: anyone who has it can kick, ban,
read full PII (IP, device_id, client_id), edit server-wide runtime
config, and impersonate any entity in chat. Treat it as a root
credential. Do NOT expose the MCP server on the public internet
without a strong token and network-level restrictions (firewall / VPN
/ Tailscale). See `documentation/25-mcp-server.md` for the full
reference and `documentation/plans/2026-07-19-mcp-server-design.md`
for the design.

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
PocketBase admin dashboard, or via the MCP server's `ban_player` tool
(which calls `worldsim.client.ban` → `BanStore.Add` + kicks any matching
connected client). An in-game admin ban command is planned.

### Ban check

The world simulator checks the ban list during `provisionClient`, after
the PocketBase user lookup that determines `isAdmin`:

1. The pusher extracts `user_id`, `ip`, and `device_id` from the `AuthFrame`
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
clear the stored JWT or `device_id` — the ban is server-side, not
a token issue.

---

## Open questions

- **[RESOLVED] NPC driving mechanism**: NPCs are extension-driven via NATS
  (see `18-extensions.md`). There is no in-process injection.
- **[OPEN] OAuth2 provider configuration**: the Docker Compose setup should
  ship with a documented template for configuring Google, GitHub, and
  Facebook OAuth2 providers. To be created alongside `20-roadmap.md`.
- **[DECISION] Token revocation on admin kick**: the revocation **policy**
  (who can kick, under what conditions) is deployment-specific and lives in an
  admin extension. The revocation **execution** is in the World Sim: when the
  admin extension triggers a kick (via NATS), the World Sim publishes
  `admin.revoke.<entity_id>` on Core NATS. The Pusher subscribes to this
  subject and immediately closes the matching WebSocket with
  `4401 Unauthorized`. This provides instant eviction without waiting for
  JWT expiry. The World Sim also removes the entity from the
  ECS and tears down its zone membership. For preventing a kicked user from
  reconnecting, see the ban system (§9).
