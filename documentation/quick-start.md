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
`dist/web/`, Docker support files in `dist/docker/`, the compose file at the
dist root, and PocketBase migrations in `dist/pb_migrations/`.

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
├── docker/               # Dockerfiles, nginx.conf, livekit.yaml, dex/, entrypoint scripts
└── pb_migrations/        # PocketBase collection schemas
```

> The compose file's build context is the dist root (`.`), so it expects
> `bin/`, `web/`, `docker/`, and `pb_migrations/` as siblings of
> `docker-compose.yml`. Run all `docker compose` commands from that
> directory.

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
override `LIVEKIT_PUBLIC_URL` in `docker-compose.yml` to the proxied `wss://`
URL instead.

> The UDP media range `50000-50020` must also be reachable by browsers —
> open it in the firewall, or proxy via nginx's `stream` module.

---

## 4. Start the stack

From the dist root (`~/pixeleruv/`):

```bash
PUBLIC_HOST=192.168.1.10 LIVEKIT_NODE_IP=192.168.1.10 docker compose up --build -d
```

(Or set them in a `.env` file — compose reads it automatically.)

This starts: `nats`, `pocketbase` (:8090), `dex` (:5556), `pusher` (:8081),
`worldsim`, `frontend` (host **:4080** HTTP + **:4043** HTTPS), `ext-demo`,
`ext-walls`, `ext-props`, `ext-av`, `livekit` (:7880 / :7881 / UDP 50000-50020).

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
pocketbase same-origin. For LAN testing you can skip this section entirely
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
    # /ws → pusher, /dex/ → dex, and /api/ → pocketbase same-origin).
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

A complete single-file variant — one `server` block handling both HTTP
(redirected to HTTPS) and HTTPS, with access/error logging, Cloudflare
real-IP forwarding, WebSocket upgrade, and streaming-friendly proxy
settings (no buffering, no chunked, unlimited body size):

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
>   `/api/` → pocketbase same-origin. No need to re-implement those
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

If you'd rather not expose `:7880` directly, add a `location` for the
LiveKit WebSocket and point `LIVEKIT_PUBLIC_URL` at it:

```nginx
    location /livekit {
        proxy_pass         http://127.0.0.1:7880;
        proxy_http_version 1.1;
        proxy_set_header   Upgrade    $http_upgrade;
        proxy_set_header   Connection $connection_upgrade;
        proxy_read_timeout 86400;
    }
```

Then set `LIVEKIT_PUBLIC_URL: "wss://pixeleruv.example.com/livekit"` in
`docker-compose.yml`. The UDP media range (`50000-50020`) still needs to
reach the host directly — open it in the firewall or proxy via nginx's
`stream` module.

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
> `PB_ADMIN_EMAIL` / `PB_ADMIN_PASSWORD` in `docker-compose.yml`.

After login you'll land in the world. Move with the arrow keys; each browser
tab is a player. Proximity-based audio/video engages automatically when
players are near each other or inside an `av_enabled` zone.

### Admin backends

| Service     | URL                                  | Use |
|-------------|--------------------------------------|-----|
| PocketBase  | `http://<host-ip>:8090/_/` (admin UI) or `https://<host-ip>/api/` (API, proxied) | Manage `maps` and `players` collections, upload map files |
| Dex         | `https://<host-ip>/dex/` | OIDC issuer (same-origin via container nginx) |

> The container nginx proxies `/api/` → PocketBase (so the frontend can fetch
> maps same-origin), but the **admin UI** at `/_/` is not proxied. For remote
> admin access either proxy `:8090` through a host nginx, or SSH tunnel:
> `ssh -L 8090:127.0.0.1:8090 admin@<host-ip>`.

---

## 7. Design and upload a map

Maps are authored in [Tiled](https://www.mapeditor.org/) and stored in
PocketBase's `maps` collection. The worldsim loads the map named by
`MAP_ID` (default `map1`) and hot-reloads within ~30s when the
PocketBase record changes.

> **First run is automatic.** On worldsim's first startup, if no `maps`
> record named `MAP_ID` exists, worldsim uploads `default-map.json` and the
> tileset PNGs referenced inside it from `MAP_DIR` (bundled at `/maps` in the
> Docker image). A fresh deploy boots straight into the office map with no
> manual upload step. The seed is idempotent — once a record exists,
> worldsim never overwrites it. The steps below are for **replacing** the
> default map or **adding** new ones.

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

A starter map and tilesets ship in `maps/` (`default-map.json`, `map1.json`,
`map1.tmx`, `Room_Builder_Office_32x32.png`, `Modern_Office_32x32.png`). The
committed `default-map.json` is the seed worldsim uploads on first run; the
frontend loads the map from PocketBase, not from static files.

### 7b. Upload to PocketBase

The `maps` collection holds one record per map. Upload the JSON file as the
record's file field, with the record's `name` matching `MAP_ID`. To replace
the seeded `map1` record, edit it in place (do not delete and recreate —
worldsim would re-seed the default on the next restart).

**Via the PocketBase admin UI** (easiest):

1. Open `http://<host-ip>:8090/_/` (or your tunnel/proxy).
2. Log in with `admin@pixeleruv.local` / `password123`.
3. Go to **Collections → maps**.
4. Edit the record whose `name` = `map1` (or create one), attach your
   `*.json` file, and save.

**Via the API** (scriptable):

```bash
# 1. Authenticate as superuser
TOKEN=$(curl -s http://127.0.0.1:8090/api/admins/auth-with-password \
  -H 'Content-Type: application/json' \
  -d '{"identity":"admin@pixeleruv.local","password":"password123"}' \
  | jq -r .token)

# 2. Update the map1 record's file (replace <record-id>)
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

Persistent data lives in two Docker volumes: `pb_data` (PocketBase) and
`dex_data` (Dex). Back them up with `docker run --rm -v pixeleruv_pb_data:/d -v "$PWD":/b alpine tar czf /b/pb_data.tgz /d`.

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
