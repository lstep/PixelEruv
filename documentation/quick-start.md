# Quick Start for Admins

This guide takes a new admin from zero to a running Pixel Eruv instance on a
remote host: copy the pre-built `dist/` directory, start the stack with
Docker Compose, optionally put nginx in front for a real TLS certificate,
log in, and design/upload a map.

It assumes the **`dist/` path** (pre-built binaries + web assets, no source
code or Go/Node toolchain needed on the host). For building `dist/` from
source on a dev machine, see the [README](../README.md) (`make dist-x86`).

---

## 1. Prerequisites on the host

- A Linux host (amd64) reachable on the public/LAN IP you want to serve from.
- Docker Engine + Docker Compose v2.
- For a real domain + Let's Encrypt cert: nginx (host-level) for TLS
  termination (section 5). For LAN testing with a self-signed cert, this is
  not needed — the in-container nginx handles HTTPS on `:4043`.
- A domain name (recommended) or a static IP. Browsers require a
  **secure context** (HTTPS or `localhost`) for the PKCE auth flow, so plain
  HTTP only works when browsing from the host itself.

Confirm:

```bash
docker --version
docker compose version
nginx -v   # only needed if you'll use a host nginx (section 5)
```

---

## 2. Get `dist/` onto the host

`dist/` is self-contained: native binaries in `dist/bin/`, built web assets in
`dist/web/`, Docker support files in `dist/docker/`, and the compose file at
the dist root. PocketBase is embedded in the worldsim binary — its collection
schemas are Go migrations compiled in, so there is no `pb_migrations/`
directory to ship.

From the machine where `dist/` was built (e.g. via `make dist-x86`):

```bash
# Option A — rsync (preferred)
rsync -avz --delete dist/ admin@<host-ip>:~/pixeleruv/

# Option B — tarball + scp
tar czf pixeleruv-dist.tgz dist/
scp pixeleruv-dist.tgz admin@<host-ip>:~/
ssh admin@<host-ip>
tar xzf pixeleruv-dist.tgz        # produces dist/
mv dist pixeleruv
```

On the host, the layout should look like:

```
~/pixeleruv/
├── docker-compose.yml
├── bin/                  # pusher, worldsim, ext-* (linux/amd64)
├── web/                  # built frontend (assets/maps/, sprites/, fonts/)
└── docker/               # Dockerfiles, nginx.conf, livekit.yaml, dex/, entrypoint scripts
```

> The compose file's build context is the dist root (`.`), so it expects
> `bin/`, `web/`, and `docker/` as siblings of `docker-compose.yml`. Run all
> `docker compose` commands from that directory.

---

## 3. Configure before first start

For local dev (browsing from `localhost`) the defaults work with no changes.
For remote access or production, review the three settings below.

### 3a. LiveKit API secret (rotate for production)

The dist compose and `dist/docker/livekit.yaml` ship with a shared dev
placeholder (`devsecretdevsecretdevsecretdevsecret123`, 40 chars — valid but
public). For anything beyond local dev, generate a fresh secret and replace
it in **both** files (they must match — `ext-av` signs join tokens with it,
LiveKit verifies them):

```bash
SECRET=$(openssl rand -hex 32)
echo "$SECRET"
```

Edit `docker-compose.yml` → `ext-av.environment`:

```yaml
LIVEKIT_API_SECRET: "<paste the 64-char hex string>"
```

Edit `docker/livekit.yaml` → `keys:`:

```yaml
keys:
  devkey: <paste the same 64-char hex string>
```

### 3b. Public host for remote access

The dist compose exposes the frontend on both HTTP (`:4080`) and HTTPS
(`:4043`, self-signed cert generated at startup). For browsers on other
machines, set `PUBLIC_HOST` to the host's LAN IP or hostname — one variable
drives everything remote access needs:

| Var | Default | Purpose |
|-----|---------|---------|
| `PUBLIC_HOST` | `localhost` | Host IP/hostname remote browsers use to reach the stack |

Setting it auto-configures three things (no manual file edits needed):

1. **TLS cert SAN** — `frontend-entrypoint.sh` appends `PUBLIC_HOST` to the
   self-signed cert's `subjectAltName` so browsers trust
   `https://<PUBLIC_HOST>:4043` (after accepting the warning once).
2. **Dex redirect URI** — `dex-entrypoint.sh` templates `PUBLIC_HOST` into
   the `redirectURIs` entry in `docker/dex/config.yaml` at startup, so the
   OIDC callback works remotely.
3. **LiveKit public URL** — `LIVEKIT_PUBLIC_URL` becomes
   `ws://<PUBLIC_HOST>:7880` so the browser's LiveKit SDK can reach the SFU.

Set it via an env var on the command line, or via a `.env` file next to
`docker-compose.yml` (compose reads it automatically):

```bash
# Option A — env var on the command line
PUBLIC_HOST=192.168.1.10 docker compose up --build -d

# Option B — .env file (compose reads it automatically)
echo "PUBLIC_HOST=192.168.1.10" > .env
docker compose up --build -d
```

Then open `https://192.168.1.10:4043` and accept the self-signed cert warning
once. For a real domain + Let's Encrypt cert, put a host nginx proxy in front
(section 5).

> If you're using a real domain (e.g. `pixeleruv.example.com`) with a host
> nginx proxy terminating TLS, set `PUBLIC_HOST=pixeleruv.example.com` so the
> Dex redirect URI matches. The host nginx forwards to the in-container nginx
> on `:4080` (HTTP) — the in-container HTTPS endpoint is only for the
> self-signed cert path.

### 3c. LiveKit node IP (for A/V over the network)

LiveKit advertises an IP in its WebRTC ICE candidates; browsers must be able
to route media back to it. Set `LIVEKIT_NODE_IP` to the host's public/LAN IP:

```bash
# run from the dist root
export LIVEKIT_NODE_IP=192.168.1.10
```

`LIVEKIT_PUBLIC_URL` (the WebSocket URL the browser's LiveKit SDK uses) is
already driven by `PUBLIC_HOST` (section 3b) — no separate edit needed. If
you proxy LiveKit's signaling WebSocket through a host nginx (section 5c),
override `LIVEKIT_PUBLIC_URL` in `docker-compose.yml` to the proxied path,
e.g. `ws://pixeleruv.example.com/livekit` (the frontend auto-upgrades
`ws://` → `wss://` on HTTPS pages — see section 5c).

> The UDP media range `50000-50020` must also be reachable by browsers —
> open it in the firewall, or proxy via nginx's `stream` module. See
> **Required open ports** below for the full port list.

### 3d. Environment variables reference

All deploy-relevant variables can be set either on the command line or in a
`.env` file next to `docker-compose.yml` (compose reads it automatically, so
you don't need to repeat them on every `docker compose up`):

```bash
cat > .env <<'EOF'
PUBLIC_HOST=pixeleruv.example.org
LIVEKIT_NODE_IP=203.0.113.10
LIVEKIT_PUBLIC_URL=ws://pixeleruv.example.org/livekit
EOF
docker compose up --build -d
```

| Variable | Default | Set on | Purpose |
|----------|---------|--------|---------|
| `PUBLIC_HOST` | `localhost` | compose | Hostname/IP remote browsers use. Drives the self-signed TLS cert SAN, the Dex redirect URI, and the default `LIVEKIT_PUBLIC_URL`. |
| `LIVEKIT_PUBLIC_URL` | `ws://${PUBLIC_HOST:-localhost}:7880` | `ext-av` in compose | WebSocket URL the browser's LiveKit SDK connects to. Override to the proxied `ws://<host>/livekit` path when proxying signaling through nginx (§5c). The frontend auto-upgrades `ws://`→`wss://` on HTTPS. |
| `LIVEKIT_NODE_IP` | `127.0.0.1` | compose (LiveKit `--node-ip`) | IP LiveKit advertises in WebRTC ICE candidates. Must be routable from the browser — set to the host's public/LAN IP for remote A/V. |
| `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` | `devkey` / `devsecret…` | `ext-av` in compose **and** `docker/livekit.yaml` | Shared secret for signing LiveKit join tokens. **Must match** in both places. Rotate for production (§3a). |
| `TLS_HOSTS` | `localhost,127.0.0.1` | `frontend` in compose | Comma-separated DNS/IP entries for the self-signed cert's SAN. `PUBLIC_HOST` is auto-appended at startup, so you usually don't set this directly. |
| `DEFAULT_MAP` | `main` | `worldsim` + `ext-av` + `ext-walls` in compose | Name of the default map record; worldsim seeds this on first run and new players spawn here. |
| `PB_ADMIN_EMAIL` / `PB_ADMIN_PASSWORD` | `admin@pixeleruv.local` / `password123` | `worldsim` in compose | PocketBase superuser credentials (used by worldsim's initial-superuser migration, since PB is embedded). **Change before exposing to the internet.** |
| `PB_DATA_DIR` | `/pb_data` | `worldsim` in compose | Directory worldsim mounts for PocketBase's SQLite data + uploaded files. Backed by the `pb_data` Docker volume. |
| `PB_HTTP_ADDR` | `0.0.0.0:8090` | `worldsim` in compose | Address worldsim's embedded PocketBase listens on (admin UI + file API). |
| `OTEL_ENABLED` | `false` | all backend services | Set to `true` to export OpenTelemetry traces and logs to the configured OTLP endpoint. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://127.0.0.1:27686` | all backend services | OTLP/HTTP endpoint for traces and logs. Points at motel for `make debug`. OpenObserve is not included in the stack by default (requires AES-NI CPU). |
| `AUDIT_RETENTION_HOURS` | `720` (30 days) | `audit` in compose | How long audit events are kept before automatic cleanup. |
| `AUDIT_BASE_PATH` | `/audit` | `audit` in compose | URL prefix when proxied under a path. Must match the nginx `location` block. |
| `AUDIT_AUTH_USER` / `AUDIT_AUTH_PASS` | `admin` / *(empty = no auth)* | `audit` in compose | Basic auth credentials for the audit UI. Set `AUDIT_AUTH_PASS` in `.env` to enable. |

> Only `PUBLIC_HOST` and `LIVEKIT_NODE_IP` are typically needed for a remote
> deploy. The rest have working defaults; override them for production
> hardening (secrets) or non-standard topologies (proxied LiveKit).
> Set `OTEL_ENABLED=true` on all services to ship traces and logs to
> a collector (motel for dev, or add OpenObserve to the stack — see section 10).

---

## 4. Start the stack

From the dist root (`~/pixeleruv/`):

```bash
PUBLIC_HOST=192.168.1.10 LIVEKIT_NODE_IP=192.168.1.10 docker compose up --build -d
```

(Or set them in a `.env` file — compose reads it automatically.)

This starts: `nats`, `dex` (:5556), `pusher` (:8081),
`worldsim` (embeds PocketBase on :8090), `frontend` (host **:4080** HTTP +
**:4043** HTTPS), `ext-demo`, `ext-walls`, `ext-props`, `ext-av`, `livekit`
(:7880 / :7881 / UDP 50000-50020), `audit` (:8082, audit log UI).

Check it came up:

```bash
docker compose ps
docker compose logs -f worldsim     # should see "worldsim ready" + map load
curl -sk https://127.0.0.1:4043/ | head   # frontend HTML (self-signed cert)
```

The frontend is now reachable on:

- `http://<host-ip>:4080` — HTTP (localhost only; PKCE auth needs a secure
  context, so this only works when browsing from the host itself).
- `https://<host-ip>:4043` — HTTPS with a self-signed cert (remote browsers;
  accept the cert warning once). This is the endpoint to use for remote
  access without a host nginx proxy.

For a real domain + Let's Encrypt cert, put a host nginx proxy in front
(section 5).

---

## 5. Host nginx as a TLS proxy (optional — real domain)

The in-container nginx already terminates TLS (self-signed cert from
`PUBLIC_HOST`) and proxies `/ws` → pusher, `/dex/` → dex, and `/api/` →
worldsim (PocketBase) same-origin. For LAN testing you can skip this section entirely
and just use `https://<host-ip>:4043`.

For a real domain with a Let's Encrypt cert, put a host nginx in front that
terminates TLS and forwards everything to the in-container nginx on
`127.0.0.1:4080` (HTTP — the host nginx handles the real cert).

### 5a. TLS certificate

Use Let's Encrypt (real domain) or a self-signed cert (LAN/IP only).

```bash
# Let's Encrypt (real domain)
sudo certbot certonly --nginx -d pixeleruv.example.com

# Self-signed (LAN/IP — browsers will warn once)
sudo openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout /etc/nginx/ssl/pixeleruv.key \
  -out    /etc/nginx/ssl/pixeleruv.crt \
  -subj   "/CN=pixeleruv" \
  -addext "subjectAltName=IP:<host-ip>,DNS:pixeleruv.example.com"
```

### 5b. nginx server block

`/etc/nginx/conf.d/pixeleruv.conf`:

```nginx
server {
    listen 80;
    server_name pixeleruv.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name pixeleruv.example.com;

    ssl_certificate     /etc/letsencrypt/live/pixeleruv.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/pixeleruv.example.com/privkey.pem;
    # For self-signed, point those at the .crt/.key you generated above.

    # WebSocket upgrade map
    map $http_upgrade $connection_upgrade {
        default upgrade;
        ''      close;
    }

    # Everything → in-container nginx on :4080 (which already handles
    # /ws → pusher, /dex/ → dex, and /api/ → worldsim same-origin).
    location / {
        proxy_pass         http://127.0.0.1:4080;
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Forwarded-Host  $host;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;

        # WebSocket support (covers /ws and any wss LiveKit signaling)
        proxy_set_header   Upgrade           $http_upgrade;
        proxy_set_header   Connection        $connection_upgrade;
        proxy_read_timeout 86400;
    }
}
```

A complete single-file variant — two `server` blocks (HTTP redirects to
HTTPS, HTTPS handles SSL and proxying), with access/error logging,
Cloudflare real-IP forwarding, WebSocket upgrade, and streaming-friendly
proxy settings (no buffering, no chunked, unlimited body size):

```nginx
server {
        listen 80;
        server_name pixeleruv.example.org;
        return 301 https://$host$request_uri;
}

server {
        listen 443 ssl;
        http2 on;

        server_name pixeleruv.example.org;
        root /var/www/blank;

        # SSL
        ssl_certificate /etc/nginx/ssl/example.org.crt;
        ssl_certificate_key /etc/nginx/ssl/example.org.key;

        # logging
        access_log /var/log/nginx/pixeleruv.access.log;
        error_log /var/log/nginx/pixeleruv.error.log warn;

        # reverse proxy
        location / {
                proxy_pass http://localhost:4080;
                proxy_http_version 1.1;
                proxy_set_header Upgrade $http_upgrade;
                proxy_set_header Connection "upgrade";
                proxy_set_header Host $host;
                proxy_set_header X-Real-IP $remote_addr;
                proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
                proxy_set_header X-Forwarded-Proto $scheme;
                proxy_set_header CF-Connecting-IP $http_cf_connecting_ip; # only set if this nginx sits behind Cloudflare: real visitor IP from Cloudflare's edge
                proxy_read_timeout 3600s;

                chunked_transfer_encoding off;
                proxy_buffering off;
                proxy_cache off;
        }

        client_max_body_size 0;
        location /robots.txt { return 200 "User-agent: *\nDisallow: /"; }
}
```

> Notes on this variant:
> - It forwards to `http://localhost:4080` (the in-container nginx's HTTP
>   endpoint), which already proxies `/ws` → pusher, `/dex/` → dex, and
>   `/api/` → worldsim (PocketBase) same-origin. No need to re-implement those
>   `location` blocks here.
> - `proxy_buffering off` + `chunked_transfer_encoding off` matter for the
>   WebSocket and any streaming responses; keep them off.
> - `client_max_body_size 0` disables the body limit — needed for map
>   uploads to PocketBase through the proxy.
> - The `CF-Connecting-IP` header is only meaningful when this nginx sits
>   behind Cloudflare; remove that line otherwise.
> - Replace `pixeleruv.example.org` and the cert paths with your real
>   domain and certificate files.

Reload and verify:

```bash
sudo nginx -t && sudo systemctl reload nginx
curl -sk https://pixeleruv.example.org/ | head
```

### 5c. (Optional) Proxy LiveKit signaling through nginx

The in-container nginx already proxies `/livekit/` → the LiveKit SFU, so if
your host nginx forwards everything to `:4080` (the single-file config in
section 5, or `docker/dist/example.nginx.conf`), LiveKit signaling is already
TLS-terminated same-origin — no extra `location` block needed on the host.

If you'd rather not expose `:7880` directly and want a host-level `location`
instead, use a trailing slash on both the location and `proxy_pass` so the
`/livekit` prefix is stripped (the LiveKit SDK appends `/rtc/v1` to the URL):

```nginx
    location /livekit/ {
        proxy_pass         http://127.0.0.1:7880/;
        proxy_http_version 1.1;
        proxy_set_header   Upgrade    $http_upgrade;
        proxy_set_header   Connection $connection_upgrade;
        proxy_read_timeout 86400;
    }
```

Then set `LIVEKIT_PUBLIC_URL: "ws://pixeleruv.example.com/livekit"` in
`docker-compose.yml`. The frontend auto-upgrades `ws://` → `wss://` when the
page is served over HTTPS, so the LiveKit SDK connects to
`wss://pixeleruv.example.com/livekit/rtc/v1` — same origin, no mixed-content
block. (You can also write `wss://` directly in `LIVEKIT_PUBLIC_URL`; either
works.) The UDP media range (`50000-50020`) still needs to reach the host
directly — open it in the firewall or proxy via nginx's `stream` module.

A ready-to-copy host config is provided at
`docker/dist/example.nginx.conf` — copy it to
`/etc/nginx/sites-available/`, symlink into `sites-enabled/`, edit the domain
and cert paths, and reload nginx.

---

## 5d. Required open ports

For the stack to work end-to-end, the browser must be able to reach these
ports on the host. With a host nginx terminating TLS (section 5), only
**443/tcp** and the **LiveKit UDP media range** need to be public; everything
else stays on localhost.

| Port | Proto | Direction | Purpose | Must be public? |
|------|-------|-----------|---------|-----------------|
| 443 | TCP | host → browser | HTTPS (frontend + `/ws` + `/dex/` + `/api/` + `/livekit/`, all proxied by nginx) | **Yes** (or 4043 for the self-signed-cert path without a host nginx) |
| 80 | TCP | host → browser | HTTP → HTTPS redirect | Yes (redirect only; can be dropped if you don't want auto-redirect) |
| 4043 | TCP | host → browser | In-container HTTPS (self-signed cert) — use this instead of 443 when you have no host nginx | Only if not using a host nginx |
| 4080 | TCP | host → browser | In-container HTTP — localhost only (PKCE auth needs a secure context) | No |
| 50000-50020 | UDP | host ↔ browser | **LiveKit WebRTC media (audio/video).** Browsers send and receive RTP here. | **Yes** — this is the one that bites people |
| 7880 | TCP | host → browser | LiveKit signaling (raw `ws://`) — **not needed** if you proxy `/livekit/` through nginx (§5c) | Only if not proxying through nginx |
| 7881 | TCP | host ↔ browser | LiveKit WebRTC-over-TCP fallback (only used if UDP fails) | Optional — open if UDP is blocked and you want TCP fallback |
| 8090 | TCP | host → admin | PocketBase admin UI (`/_/`) served by worldsim — not proxied by the container nginx | No — SSH tunnel instead (`ssh -L 8090:127.0.0.1:8090 …`) |
| 5556 | TCP | — | Dex (container-internal; proxied as `/dex/` by nginx) | No |
| 8081 | TCP | — | Pusher (container-internal; proxied as `/ws` by nginx) | No |
| 4222 | TCP | — | NATS (container-internal) | No |

### The UDP 50000-50020 warning

This is the most common reason A/V fails on a remote deploy. LiveKit's
signaling can connect fine over `wss://` (fixed in §5c), but the actual
audio/video media flows over **UDP** in this range, and it must be reachable
**both inbound and outbound** from the browser's perspective.

Symptoms when it's blocked:
- Signaling connects (no mixed-content error), but no video/audio appears.
- Browser console: ICE connection stays in `checking` then fails.
- `docker compose logs ext-av` shows joins, but `livekit` logs show no media.

Fixes, in order of preference:
1. **Open UDP 50000-50020 in the host firewall** (and any cloud security
   group). This is the simplest fix.
   ```bash
   # ufw (Ubuntu)
   sudo ufw allow 50000:50020/udp

   # iptables
   sudo iptables -A INPUT -p udp --dport 50000:50020 -j ACCEPT
   ```
2. **Run a TURN server** (e.g. `coturn`) if the host is behind NAT or UDP is
   blocked on the client side. LiveKit supports external TURN; configure it
   in `docker/livekit.yaml` under `rtc.turn`. Out of scope for this guide.
3. **Fall back to WebRTC-over-TCP** by opening `7881/tcp` — slower and not
   ideal for real-time media, but works when UDP is impossible.

> The range is configurable in `docker/livekit.yaml`
> (`rtc.port_range_start`/`port_range_end`) and must match the
> `docker-compose.yml` port mapping. Narrowing it reduces concurrent A/V
> capacity; widening it requires updating both files and the firewall rule.

---

## 6. Access the app

Open **`https://pixeleruv.example.com`** (or `https://<host-ip>` with a
self-signed cert — accept the warning once).

You'll be redirected to Dex for login.

### Login / password

| Role   | Email                       | Password     |
|--------|-----------------------------|--------------|
| Admin  | `admin@pixeleruv.local`     | `password123`|
| Player | `player@pixeleruv.local`    | `password123`|

> These are the default Dex users defined in `docker/dex/config.yaml`
> (`staticPasswords`, bcrypt cost 10). **Change them before exposing the
> host to the internet** — edit the hashes in that file and restart `dex`.
> The PocketBase superuser uses the same email/password
> (`admin@pixeleruv.local` / `password123`), set via
> `PB_ADMIN_EMAIL` / `PB_ADMIN_PASSWORD` on the `worldsim` service in
> `docker-compose.yml` (worldsim embeds PocketBase and runs the
> initial-superuser migration on first start).

After login you'll land in the world. Move with the arrow keys; each browser
tab is a player. Proximity-based audio/video engages automatically when
players are near each other or inside an `av_enabled` zone.

### Admin backends

| Service     | URL                                  | Use |
|-------------|--------------------------------------|-----|
| PocketBase  | `http://<host-ip>:8090/_/` (admin UI, served by worldsim) or `https://<host-ip>/api/` (API, proxied) | Manage `maps` and `players` collections, upload map files |
| Dex         | `https://<host-ip>/dex/` | OIDC issuer (same-origin via container nginx) |
| Audit UI    | `https://<host-ip>/audit/` (proxied) or `http://<host-ip>:8082/` (direct) | Search audit events, view world status, check service health. Basic auth (`AUDIT_AUTH_USER`/`AUDIT_AUTH_PASS`). |

> The container nginx proxies `/api/` → worldsim (so the frontend can fetch
> maps same-origin), but the **admin UI** at `/_/` is not proxied. For remote
> admin access either proxy `:8090` through a host nginx, or SSH tunnel:
> `ssh -L 8090:127.0.0.1:8090 admin@<host-ip>`. The audit UI (`/audit/`) is
> proxied same-origin through the container nginx.

---

## 7. Design and upload a map

Maps are authored in [Tiled](https://www.mapeditor.org/) and stored in
PocketBase's `maps` collection. The worldsim loads all maps from
PocketBase and hot-reloads within ~30s
when a PocketBase record changes.

> **First run is automatic.** On worldsim's first startup, if no `maps`
> record named after the configured `DEFAULT_MAP` (default `main`) exists, worldsim uploads
> `default-map.json` and the tileset PNGs referenced inside it from
> `MAP_DIR` (bundled at `/maps` in the Docker image). A fresh deploy boots
> straight into the office map with no manual upload step. The seed is
> idempotent — once a record exists, worldsim never overwrites it. The
> steps below are for **replacing** the default map or **adding** new ones.

### 7a. Author in Tiled

1. New map: **Orthogonal**, tile size **32×32**.
2. Create layers (full reference:
   [`documentation/21-map-design-guide.md`](../documentation/21-map-design-guide.md)):
   - **Decoration layers** — tile or object layers with the custom property
     `layer_type = decoration`. Altitude = layer list order (bottom first).
     Optional `sort_mode` = `static` (default) or `dynamic` (Y-sort with
     avatars).
   - **`Walls`** — tile layer, collision fallback (case-insensitive name).
   - **`Zones`** — object layer with rect/circle/polygon shapes. Set
     `zone_type` (e.g. `wall`, `meeting`, `water`); set `av_enabled = true`
     on meeting-room zones to enable room-based A/V.
   - **`Entities`** — object layer for interactive props (with `gid` +
     `entity_type` or `owner_extension`).
3. Export as **JSON** (`File → Export As… → *.json`).

A starter map and tilesets ship in `maps/` (`default-map.json`, `main.tmx`,
`Room_Builder_Office_32x32.png`, `Modern_Office_32x32.png`). The
committed `default-map.json` is the seed worldsim uploads on first run; the
frontend loads the map from PocketBase, not from static files.

### 7b. Upload to PocketBase

The `maps` collection holds one record per map. Upload the JSON file as the
record's file field, with the record's `name` set to your map name. To replace the seeded `main`
record, edit it in place (do not delete and recreate — worldsim would
re-seed the default on the next restart).

**Via the PocketBase admin UI** (easiest):

1. Open `http://<host-ip>:8090/_/` (or your tunnel/proxy).
2. Log in with `admin@pixeleruv.local` / `password123`.
3. Go to **Collections → maps**.
4. Edit the record whose `name` = `main` (or create one), attach your
   `*.json` file, and save.

**Via the API** (scriptable):

```bash
# 1. Authenticate as superuser
TOKEN=$(curl -s http://127.0.0.1:8090/api/admins/auth-with-password \
  -H 'Content-Type: application/json' \
  -d '{"identity":"admin@pixeleruv.local","password":"password123"}' \
  | jq -r .token)

# 2. Update the main record's file (replace <record-id>)
curl -s -X PATCH http://127.0.0.1:8090/api/collections/maps/records/<record-id> \
  -H "Authorization: $TOKEN" \
  -F "tiled_json=@/path/to/your-map.json"
```

Within ~30s worldsim detects the new filename, publishes `map.updated` over
NATS, and `ext-walls` / `ext-av` re-read the map and re-register triggers.
No restart needed.

> Editing the committed `maps/default-map.json` in the repo does **not** update
> the running world — the worldsim reads from PocketBase, not the
> filesystem. Always re-upload to PocketBase after editing.

---

## 8. Character spritesheets

Character spritesheets live in PocketBase's `sprite_bases` collection. Each
record has a `name` (e.g. `char_0`) and a `sheet` file field (a 768×192 PNG,
same layout as the limezu sheets — see
[`documentation/22-limezu-sprites.md`](../documentation/22-limezu-sprites.md)).

### 8a. First run (automatic)

On worldsim's first startup, if the `sprite_bases` collection is empty, it
auto-seeds from the sprites directory. No action needed — the catalog is
populated on first boot.

**Docker (`make up`):** the worldsim image bundles the sprites at `/sprites`
and sets `SPRITES_DIR=/sprites` automatically. No configuration needed.

**Native / local dev:** the authoritative sprites live in `spritesheets/` at
the repo root. `make` copies them to `frontend/public/sprites/` so the dev
server and `dist` builds have them. Set `SPRITES_DIR` before starting
worldsim:

```bash
SPRITES_DIR=frontend/public/sprites ./dist/bin/worldsim
```

For the self-contained `dist/` deployment, `make dist-*` stages spritesheets
into `dist/sprites/`, so the default `SPRITES_DIR=./sprites` works when
running from the `dist/` directory.

### 8b. Adding new spritesheets later

Drop new 768×192 PNGs into `spritesheets/` at the repo root and run the
seed-sprites CLI with `-force`:

```bash
# Native / local dev:
./dist/bin/seed-sprites -dir frontend/public/sprites -force

# Self-contained dist/ deployment:
./dist/bin/seed-sprites -dir dist/sprites -force

# Inside the worldsim Docker container:
docker compose exec worldsim seed-sprites -dir /sprites -force
```

`-force` uploads every PNG in the directory, skipping any whose `name` (filename
stem) already exists in `sprite_bases`. So existing sheets are never duplicated.

Alternatively, add sheets via the PocketBase admin UI:
1. Open `http://<host-ip>:8090/_/` (or SSH tunnel — see section 6).
2. Go to **Collections → sprite_bases**.
3. Add a record: set `name`, attach the PNG as `sheet`.

### 8c. Player selection

Logged-in users see a pre-join character picker on first visit (before entering
the world). They click a thumbnail and confirm; the choice persists in
PocketBase's `players.sprite_base` field and is restored on every reconnect.
A "Character sheet" field in the top-right Menu dropdown lets them change it
later — the avatar hot-swaps live for all connected clients.

Guests (not logged in) skip the picker and get a deterministic fallback sprite
(hash of their entity ID).

---

## 9. Day-to-day operations

```bash
cd ~/pixeleruv

docker compose ps                          # status
docker compose logs -f worldsim            # tail a service
docker compose restart ext-walls           # restart one service
docker compose down                        # stop everything
LIVEKIT_NODE_IP=<ip> docker compose up -d  # start (no --build after first time)

# Player records
curl -s http://127.0.0.1:8090/api/collections/players/records | jq

# On-demand map integrity check
docker exec -it $(docker compose ps -q nats) nats -s nats://127.0.0.1:4222 pub admin.map.integrity ""
docker compose logs worldsim 2>&1 | grep integrity
```

Persistent data lives in two Docker volumes: `pb_data` (PocketBase, mounted
into worldsim via `PB_DATA_DIR`) and `dex_data` (Dex). Back them up with
`docker run --rm -v pixeleruv_pb_data:/d -v "$PWD":/b alpine tar czf /b/pb_data.tgz /d`.

For a portable JSON export of PB collections (schema + records + file fields),
build the `pb-collections` binary (`make build` → `dist/bin/pb-collections`)
and run it against a copy of the `pb_data` directory. See
[Backup and Restore](24-backup-and-restore.md) for the full export/import
flow, trade-offs between volume snapshots and `pb-collections`, and restore
instructions.

---

## 9. Common pitfalls

- **Black screen + `crypto.subtle` error in console**: you're accessing over
  plain HTTP from a remote browser. Auth needs a secure context — use the
  HTTPS endpoint (`https://<host-ip>:4043`, section 3b) or browse from
  `localhost` on the host itself.
- **Map fails to load on a remote browser (network error to `localhost:8090`)**:
  you're running an old build where `mapLoader.ts` hardcoded `localhost:8090`.
  Rebuild the frontend — `mapLoader.ts` now derives the PocketBase URL from
  `window.location.origin` and the container nginx proxies `/api/` → PocketBase.
- **Dex rejects the redirect URI**: `PUBLIC_HOST` doesn't match the URL the
  browser used. `dex-entrypoint.sh` templates `PUBLIC_HOST` into the
  `redirectURIs` at startup — if you change `PUBLIC_HOST`, recreate the `dex`
  container (`docker compose up -d --force-recreate dex`). If you're using a
  host nginx proxy with a real domain, set `PUBLIC_HOST` to that domain.
- **`token signature is invalid` after rotating the LiveKit secret**: old
  browser tokens are signed with the old secret. Restart `livekit` and
  `ext-av`, then have browsers rejoin (refresh the page).
- **A/V connects but no media flows**: `LIVEKIT_NODE_IP` is wrong, or the
  UDP range `50000-50020` is blocked by a firewall. Check ICE candidates in
  the browser's WebRTC internals.
- **Self-signed cert not trusted for the LAN IP**: `PUBLIC_HOST` wasn't set
  when the frontend container started, so the cert's SAN doesn't include the
  LAN IP. Recreate the frontend container (`docker compose up -d
  --force-recreate frontend`) after setting `PUBLIC_HOST`.
- **Map edits don't appear**: you edited the file in the repo, not in
  PocketBase. Re-upload to the `maps` collection (section 7b).
- **`LIVEKIT_API_SECRET` mismatch**: the secret in `docker-compose.yml`
  (`ext-av`) and `docker/livekit.yaml` (`keys:`) must be identical and ≥32
  chars. The dist ships with a valid shared dev placeholder; rotate it for
  production (section 3a).

---

## 10. Audit and Observability

The stack ships with the **audit service** — it records *what happened*
(lifecycle and interaction events). For *why/how* (OpenTelemetry traces
and logs), use `make debug` with motel (dev), or add OpenObserve to the
stack on a compatible CPU (see 10b below).

### 10a. Audit UI (`/audit/`)

The audit service subscribes to `audit.event` on NATS and persists events
to its own SQLite database — independent of worldsim, so it survives
worldsim crashes and can audit the crash. Open the UI at:

```
https://<host-ip>/audit/      # proxied (same-origin)
http://<host-ip>:8082/         # direct (port exposed in compose)
```

If `AUDIT_AUTH_PASS` is set, the browser prompts for basic auth
(`AUDIT_AUTH_USER` / `AUDIT_AUTH_PASS`). `/healthz` and `/static/` are
exempt so health checks and CSS load without credentials.

**Pages:**

| Route | Purpose |
|-------|---------|
| `/audit/` | Dashboard: service health cards, event severity counts (24h), event type counts, recent events |
| `/audit/events` | Searchable event table — filter by type, severity, actor, or entity ID. HTMX partial reload on filter. |
| `/audit/events/<id>` | Event detail: full payload, actor info, trace ID (if OTel is enabled) |
| `/audit/players/<sub>` | Player timeline: all events for one player, chronological |
| `/audit/world` | World status: per-map overview (dimensions, entity/zone counts), zone table with occupancy, connected players (linked to their events), extension status (alive/dead, heartbeat age, triggers) |
| `/audit/health` | Service health detail (from pusher's `/healthz`) |

Events are retained for 30 days by default (`AUDIT_RETENTION_HOURS` env
var). The storage layer is behind an interface designed to upgrade to
ClickHouse or TimescaleDB when volume grows.

### 10b. OpenTelemetry traces (motel / OpenObserve)

All backend services (pusher, worldsim, all four extensions) are
instrumented with OpenTelemetry. Telemetry is **off by default**.

**Dev** — `make debug` starts motel (a local TUI collector at
`http://127.0.0.1:27686`). Set `OTEL_ENABLED=true` on the services you
want to instrument:

```bash
# Enable for all services
echo "OTEL_ENABLED=true" >> .env
docker compose up -d

# Or enable for a single service
docker compose up -d -e OTEL_ENABLED=true worldsim
```

**Production** — OpenObserve is not included in the Docker stack by
default because its x86 build requires AES-NI CPU instructions (not
available on older Xeons like the L3426). To add it on a compatible CPU,
add this service to `docker-compose.yml`:

```yaml
  openobserve:
    image: openobserve/openobserve:latest
    environment:
      ZO_ROOT_USER_EMAIL: "admin@pixeleruv.local"
      ZO_ROOT_USER_PASSWORD: "PixelEruv@2026!"  # requires upper+lower+digit+special
      ZO_DATA_DIR: "/data"
    volumes:
      - o2_data:/data
    ports:
      - "5080:5080"
    restart: unless-stopped
```

Then set `OTEL_EXPORTER_OTLP_ENDPOINT=http://openobserve:5080/api/default`
on the backend services and add the `/otel/` nginx proxy (see the
audit-observability design doc). On Apple Silicon, use
`openobserve/openobserve:latest-arm64` instead.

### 10c. Linking audit events to traces

Each audit event carries an optional `trace_id`. When OTel is enabled,
the audit UI's event detail view shows the trace ID — search for it in
motel or OpenObserve to jump from *what* happened to *why*.

See
[`documentation/plans/2026-07-12-audit-observability-design.md`](plans/2026-07-12-audit-observability-design.md)
for the full design, event type catalog, and storage upgrade path.
