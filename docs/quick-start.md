# Quick Start for Admins

This guide takes a new admin from zero to a running Pixel Eruv instance on a
remote host: copy the pre-built `dist/` directory, start the stack with
Docker Compose, put nginx in front as a TLS-terminating proxy, log in, and
design/upload a map.

It assumes the **`dist/` path** (pre-built binaries + web assets, no source
code or Go/Node toolchain needed on the host). For building `dist/` from
source on a dev machine, see the [README](../README.md) (`make dist-x86`).

---

## 1. Prerequisites on the host

- A Linux host (amd64) reachable on the public/LAN IP you want to serve from.
- Docker Engine + Docker Compose v2.
- nginx (host-level) for TLS termination.
- A domain name (recommended) or a static IP. Browsers require a
  **secure context** (HTTPS or `localhost`) for the PKCE auth flow, so plain
  HTTP only works when browsing from the host itself.

Confirm:

```bash
docker --version
docker compose version
nginx -v
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
├── web/                  # built frontend + maps/ + sprites/
├── docker/               # Dockerfiles, nginx.conf, livekit.yaml, dex/
└── pb_migrations/        # PocketBase collection schemas
```

> The compose file's build context is the dist root (`.`), so it expects
> `bin/`, `web/`, `docker/`, and `pb_migrations/` as siblings of
> `docker-compose.yml`. Run all `docker compose` commands from that
> directory.

---

## 3. Configure before first start

Three things must be set before `docker compose up` or the stack will either
fail to start or be unusable remotely.

### 3a. LiveKit API secret (REQUIRED — dist ships with a broken placeholder)

The dist compose and `dist/docker/livekit.yaml` ship with
`LIVEKIT_API_SECRET: "secret"` (6 characters). LiveKit rejects secrets
shorter than 32 characters, so A/V will not start. Generate a real secret
and replace it in **both** files (they must match — `ext-av` signs join
tokens with it, LiveKit verifies them):

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

### 3b. Public URL for remote access

The dist compose is HTTP-only (frontend on `:4080`). For browsers on other
machines you need HTTPS (see section 4). Pick your public URL now, e.g.
`https://pixeleruv.example.com`, and update two places:

**`docker/dex/config.yaml`** — set `issuer:` to the public URL and add the
matching redirect URI:

```yaml
issuer: https://pixeleruv.example.com/dex

staticClients:
  - id: pixeleruv
    redirectURIs:
      - "https://pixeleruv.example.com/auth/callback"
    name: "Pixel Eruv"
    public: true
```

**`docker-compose.yml`** — set `DEX_ISSUER` on `pusher` to the same value
(the pusher validates the token `iss` claim against this):

```yaml
pusher:
  environment:
    DEX_ISSUER: "https://pixeleruv.example.com/dex"
    DEX_JWKS_URL: "http://dex:5556/dex/keys"   # container-internal, unchanged
```

> Dex validates that incoming requests match its `issuer` URL. Behind the
> host nginx proxy it relies on `X-Forwarded-Host` / `X-Forwarded-Proto`
> (set in the nginx config below) to reconstruct the public URL.

### 3c. LiveKit node IP (for A/V over the network)

LiveKit advertises an IP in its WebRTC ICE candidates; browsers must be able
to route media back to it. Set `LIVEKIT_NODE_IP` to the host's public/LAN IP
and `LIVEKIT_PUBLIC_URL` to the WebSocket URL browsers will use:

```bash
# run from the dist root
export LIVEKIT_NODE_IP=<host-public-ip>
```

And in `docker-compose.yml` → `ext-av.environment`:

```yaml
LIVEKIT_PUBLIC_URL: "wss://pixeleruv.example.com/ws-livekit"
```

> If you proxy LiveKit's signaling WebSocket through the host nginx (see
> section 4), use `wss://pixeleruv.example.com/...`. If you expose LiveKit's
> `7880` port directly, use `ws://<host-ip>:7880` and open the port in the
> firewall. The UDP media range `50000-50020` must also be reachable by
> browsers — proxy that through nginx's `stream` block or open it in the
> firewall.

---

## 4. Start the stack

From the dist root (`~/pixeleruv/`):

```bash
LIVEKIT_NODE_IP=<host-public-ip> docker compose up --build -d
```

This starts: `nats`, `pocketbase` (:8090), `dex` (:5556), `pusher` (:8081),
`worldsim`, `frontend` (:8080 → host **:4080**), `ext-demo`, `ext-walls`,
`ext-props`, `ext-av`, `livekit` (:7880 / :7881 / UDP 50000-50020).

Check it came up:

```bash
docker compose ps
docker compose logs -f worldsim     # should see "worldsim ready" + map load
curl -s http://127.0.0.1:4080/ | head   # frontend HTML
```

The frontend is now reachable on `http://<host-ip>:4080` — but **only from
the host itself** (or over plain HTTP, which breaks auth on remote
browsers). Put nginx in front for real access.

---

## 5. Host nginx as a TLS proxy to :4080

The in-container nginx already proxies `/ws` → pusher and `/dex/` → dex
same-origin. The host nginx only needs to terminate TLS and forward
everything to `127.0.0.1:4080`.

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
    # /ws → pusher and /dex/ → dex same-origin).
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

Reload and verify:

```bash
sudo nginx -t && sudo systemctl reload nginx
curl -sk https://pixeleruv.example.com/ | head
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
| PocketBase  | `https://pixeleruv.example.com/pb/_/` (if proxied) or `http://<host-ip>:8090/_/` | Manage `maps` and `players` collections, upload map files |
| Dex         | `https://pixeleruv.example.com/dex/` | OIDC issuer (same-origin via container nginx) |

> PocketBase is **not** proxied by the container nginx by default. For
> remote admin access either proxy `:8090` through the host nginx, or SSH
> tunnel: `ssh -L 8090:127.0.0.1:8090 admin@<host-ip>`.

---

## 7. Design and upload a map

Maps are authored in [Tiled](https://www.mapeditor.org/) and stored in
PocketBase's `maps` collection. The worldsim loads the map named by
`MAP_ID` (default `test-map`) and hot-reloads within ~30s when the
PocketBase record changes.

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

A starter map and tileset ship in `web/maps/` (`test-map.json`,
`tileset.json`, `tileset.png`) — use them as a template.

### 7b. Upload to PocketBase

The `maps` collection holds one record per map. Upload the JSON file as the
record's file field, with the record's `name` matching `MAP_ID`.

**Via the PocketBase admin UI** (easiest):

1. Open `http://<host-ip>:8090/_/` (or your tunnel/proxy).
2. Log in with `admin@pixeleruv.local` / `password123`.
3. Go to **Collections → maps**.
4. Edit the record whose `name` = `test-map` (or create one), attach your
   `*.json` file, and save.

**Via the API** (scriptable):

```bash
# 1. Authenticate as superuser
TOKEN=$(curl -s http://127.0.0.1:8090/api/admins/auth-with-password \
  -H 'Content-Type: application/json' \
  -d '{"identity":"admin@pixeleruv.local","password":"password123"}' \
  | jq -r .token)

# 2. Update the test-map record's file (replace <record-id>)
curl -s -X PATCH http://127.0.0.1:8090/api/collections/maps/records/<record-id> \
  -H "Authorization: $TOKEN" \
  -F "file=@/path/to/your-map.json"
```

Within ~30s worldsim detects the new filename, publishes `map.updated` over
NATS, and `ext-walls` / `ext-av` re-read the map and re-register triggers.
No restart needed.

> Editing the committed `assets/map1.json` in the repo does **not** update
> the running world — the worldsim reads from PocketBase, not the
> filesystem. Always re-upload to PocketBase after editing.

---

## 8. Day-to-day operations

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
  plain HTTP from a remote browser. Auth needs a secure context — use HTTPS
  (section 5) or browse from `localhost` on the host itself.
- **`token signature is invalid` after rotating the LiveKit secret**: old
  browser tokens are signed with the old secret. Restart `livekit` and
  `ext-av`, then have browsers rejoin (refresh the page).
- **A/V connects but no media flows**: `LIVEKIT_NODE_IP` is wrong, or the
  UDP range `50000-50020` is blocked by a firewall. Check ICE candidates in
  the browser's WebRTC internals.
- **Dex returns an error on login**: the `issuer:` in `docker/dex/config.yaml`
  doesn't match the URL the browser used, or nginx isn't forwarding
  `X-Forwarded-Host` / `X-Forwarded-Proto`. Both must match the public URL.
- **Map edits don't appear**: you edited the file in the repo, not in
  PocketBase. Re-upload to the `maps` collection (section 7b).
- **`LIVEKIT_API_SECRET` mismatch**: the secret in `docker-compose.yml`
  (`ext-av`) and `docker/livekit.yaml` (`keys:`) must be identical and ≥32
  chars. The dist ships with a valid shared dev placeholder; rotate it for
  production (section 3a).
