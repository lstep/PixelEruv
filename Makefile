.PHONY: proto build sync-assets sync-maps sync-sprites web dist dist-x86 dist-macos dist-stage up down logs debug debug-frontend pb-collections geoip deploy-remote

PROTO_DIR := proto
GO_OUT := backend/internal/pb
TS_OUT := frontend/src/proto
COMPOSE_FILE := docker/docker-compose.yml
DIST_BIN := dist/bin
DIST_DIR := dist
DIST_COMPOSE := $(DIST_DIR)/docker-compose.yml

# Build version: git tag if HEAD is exactly on a tag, otherwise short commit
# hash. Falls back to "dev" if git is unavailable (e.g. extracted tarball).
VERSION := $(shell git describe --tags --exact-match 2>/dev/null || git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS := -X github.com/lstep/pixeleruv/backend/internal/version.Version=$(VERSION)

# Cross-compile defaults — native platform. Override per-target.
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

# OpenTelemetry / motel debug configuration
OTEL_ENDPOINT := http://127.0.0.1:27686
NATS_CONTAINER := pixeleruv-debug-nats
NATS_PORT := 4222

# Go plugin
GO_PROTOC := --go_out=$(GO_OUT) --go_opt=module=github.com/lstep/pixeleruv/backend/internal/pb
GO_GRPC_PROTOC := --go-grpc_out=$(GO_OUT) --go-grpc_opt=module=github.com/lstep/pixeleruv/backend/internal/pb

# TS plugin (bufbuild/es)
TS_PROTOC := --es_out=$(TS_OUT) --es_opt=target=ts

proto:
	@mkdir -p $(GO_OUT) $(TS_OUT)
	protoc $(GO_PROTOC) -I $(PROTO_DIR) $(PROTO_DIR)/*.proto
	protoc $(TS_PROTOC) -I $(PROTO_DIR) $(PROTO_DIR)/*.proto

# Build Go binaries into dist/bin/ for the target GOOS/GOARCH.
# Defaults to native; overridden by dist-x86 / dist-macos.
build:
	@mkdir -p $(DIST_BIN)
	cd backend && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../$(DIST_BIN)/pusher ./cmd/pusher
	cd backend && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../$(DIST_BIN)/worldsim ./cmd/worldsim
	cd backend && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../$(DIST_BIN)/ext-demo ./cmd/ext-demo
	cd backend && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../$(DIST_BIN)/ext-walls ./cmd/ext-walls
	cd backend && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../$(DIST_BIN)/ext-props ./cmd/ext-props
	cd backend && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../$(DIST_BIN)/ext-av ./cmd/ext-av
	cd backend && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../$(DIST_BIN)/audit ./cmd/audit
	cd backend && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../$(DIST_BIN)/admin ./cmd/admin
	cd backend && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../$(DIST_BIN)/seed-sprites ./cmd/seed-sprites
	cd backend && GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ../$(DIST_BIN)/pb-collections ./cmd/pb-collections

# Sync root assets into frontend/public/ so Vite serves them in dev and bundles
# them into dist/web/. The root maps/ and spritesheets/ directories are the
# authoritative sources; frontend/public/assets/maps and frontend/public/sprites
# are generated copies.
sync-assets: sync-maps sync-sprites sync-icon sync-game-assets

sync-maps:
	@mkdir -p frontend/public/assets/maps
	cp -R maps/. frontend/public/assets/maps/

sync-sprites:
	@mkdir -p frontend/public/sprites
	cp -R spritesheets/. frontend/public/sprites/

sync-icon:
	@mkdir -p frontend/public/assets
	cp assets/pixel-eruv-icon.svg frontend/public/assets/pixel-eruv-icon.svg

# sync-game-assets copies sounds and non-character sprites (e.g.
# light-glow.png) from the authoritative assets/ directory.
sync-game-assets:
	@mkdir -p frontend/public/assets/sounds frontend/public/assets/sprites
	cp -R assets/sounds/. frontend/public/assets/sounds/
	cp -R assets/sprites/. frontend/public/assets/sprites/

# Build frontend static assets into dist/web/
web: sync-assets
	cd frontend && npx vite build

# Stage Docker support files, compose, and migrations into dist/.
# Called after build + web by the dist-* targets.
dist-stage:
	@# --- remove stale config from the old dist layout ---
	@rm -rf $(DIST_DIR)/config
	@# --- stage Docker support files into dist/docker/ ---
	@mkdir -p $(DIST_DIR)/docker
	cp docker/dist/backend.Dockerfile   $(DIST_DIR)/docker/backend.Dockerfile
	cp docker/dist/frontend.Dockerfile  $(DIST_DIR)/docker/frontend.Dockerfile
	cp docker/nginx.conf                $(DIST_DIR)/docker/nginx.conf
	cp docker/dist/example.nginx.conf   $(DIST_DIR)/example.nginx.conf
	cp docker/livekit.yaml              $(DIST_DIR)/docker/livekit.yaml
	cp docker/frontend-entrypoint.sh    $(DIST_DIR)/docker/frontend-entrypoint.sh
	@# --- stage static welcome page (copy entire directory) ---
	@mkdir -p $(DIST_DIR)/docker/welcome
	cp -R docker/welcome/.              $(DIST_DIR)/docker/welcome/
	@# --- bake the build version into the staged welcome pages ---
	@# Uses sed -i.bak + rm for portability across macOS (BSD sed) and Linux (GNU sed).
	@sed -i.bak 's/__VERSION__/$(VERSION)/g' $(DIST_DIR)/docker/welcome/*.html
	@rm -f $(DIST_DIR)/docker/welcome/*.bak
	@# --- stage compose ---
	cp docker/dist/docker-compose.yml   $(DIST_COMPOSE)
	@# --- stage character spritesheets for worldsim auto-seed ---
	@mkdir -p $(DIST_DIR)/sprites
	cp -R frontend/public/sprites/.      $(DIST_DIR)/sprites/
	@# --- stage default map (Tiled JSON + tileset PNGs) for worldsim auto-seed ---
	@mkdir -p $(DIST_DIR)/maps
	cp -R maps/.                         $(DIST_DIR)/maps/
	@# --- stage GeoIP MMDB for audit service country flags ---
	@mkdir -p $(DIST_DIR)/geoip
	cp backend/cmd/audit/data/ip-to-country.mmdb $(DIST_DIR)/geoip/
	@# --- stage server management scripts (backup/restore/deploy) ---
	cp docker/dist/backup-volumes.sh    $(DIST_DIR)/backup-volumes.sh
	cp docker/dist/restore-volumes.sh   $(DIST_DIR)/restore-volumes.sh
	cp docker/dist/deploy.sh            $(DIST_DIR)/deploy.sh
	@chmod +x $(DIST_DIR)/backup-volumes.sh $(DIST_DIR)/restore-volumes.sh $(DIST_DIR)/deploy.sh

# dist: native platform (convenience alias).
dist: build web dist-stage
	@echo "==> dist/ built for $(GOOS)/$(GOARCH). Run with:"
	@echo "    docker compose -f $(DIST_COMPOSE) up --build"

# dist-x86: Linux Intel (amd64) — for Docker deployment on Intel servers.
dist-x86: GOOS := linux
dist-x86: GOARCH := amd64
dist-x86: build web dist-stage
	@echo "==> dist/ built for linux/amd64. Run with:"
	@echo "    docker compose -f $(DIST_COMPOSE) up --build"

# dist-macos: macOS native (arm64 on Apple Silicon) — for local host execution.
dist-macos: GOOS := darwin
dist-macos: GOARCH := arm64
dist-macos: build web dist-stage
	@echo "==> dist/ built for darwin/arm64. Binaries run natively on macOS."
	@echo "    Run Go services directly from dist/bin/; use Docker for nats/mailhog/livekit."

# deploy-remote: build for linux/amd64, rsync dist/ to the server, and run
# deploy.sh on the server. This is the one-command upgrade path — it backs
# up volumes, builds images, and recreates only changed services without
# touching persistent data.
#
# Override REMOTE_HOST and REMOTE_PATH for your server:
#   make deploy-remote REMOTE_HOST=user@myserver REMOTE_PATH=/opt/pixeleruv
REMOTE_HOST ?= ellipsis
REMOTE_PATH ?= /opt/pixeleruv

deploy-remote: dist-x86
	@echo "==> Rsyncing dist/ to $(REMOTE_HOST):$(REMOTE_PATH)/"
	rsync -avz --delete dist/ "$(REMOTE_HOST):$(REMOTE_PATH)/"
	@echo "==> Running deploy.sh on $(REMOTE_HOST)"
	ssh "$(REMOTE_HOST)" "cd '$(REMOTE_PATH)' && ./deploy.sh"

up: sync-assets
	docker compose -f $(COMPOSE_FILE) up --build

down:
	docker compose -f $(COMPOSE_FILE) down
	@docker rm -f $(NATS_CONTAINER) >/dev/null 2>&1 || true

logs:
	docker compose -f $(COMPOSE_FILE) logs -f

# --- Debug with OpenTelemetry + motel ---
# Starts motel (if not running), a standalone NATS container,
# and the two Go services with OTEL_ENABLED=true so traces/logs ship to motel.
# PocketBase is embedded in worldsim — no separate container needed.
# Frontend is started separately via `make debug-frontend`.
# Stop everything with `make debug-stop`.
debug: debug-nats
	@command -v motel >/dev/null 2>&1 || { echo "motel not found — install from https://github.com/kitlangton/motel"; exit 1; }
	@motel start >/dev/null 2>&1 || true
	@echo "==> motel: $(OTEL_ENDPOINT) (TUI at http://127.0.0.1:27686)"
	@echo "==> starting worldsim + pusher with OTel enabled (Ctrl-C to stop)"
	@OTEL_ENABLED=true OTEL_EXPORTER_OTLP_ENDPOINT=$(OTEL_ENDPOINT) \
		NATS_URL=nats://127.0.0.1:$(NATS_PORT) TICK_HZ=10 \
		PB_DATA_DIR=./pb_data PB_HTTP_ADDR=127.0.0.1:8090 \
		PB_ADMIN_EMAIL=admin@pixeleruv.local PB_ADMIN_PASSWORD=password123 \
		./$(DIST_BIN)/worldsim &
	@OTEL_ENABLED=true OTEL_EXPORTER_OTLP_ENDPOINT=$(OTEL_ENDPOINT) \
		NATS_URL=nats://127.0.0.1:$(NATS_PORT) WS_ADDR=:8081 \
		PB_API_URL=http://127.0.0.1:8090/api \
		./$(DIST_BIN)/pusher
	@$(MAKE) debug-stop

# Start a standalone NATS container for the debug session.
debug-nats:
	@docker rm -f $(NATS_CONTAINER) >/dev/null 2>&1 || true
	@docker run -d --name $(NATS_CONTAINER) -p $(NATS_PORT):4222 nats:2.10-alpine -js >/dev/null
	@echo "==> NATS running on nats://127.0.0.1:$(NATS_PORT) (container: $(NATS_CONTAINER))"

# Stop the debug NATS container. Go services exit on Ctrl-C.
debug-stop:
	@docker rm -f $(NATS_CONTAINER) >/dev/null 2>&1 || true
	@echo "==> debug session stopped"

# Start the Vite dev server with frontend OTel enabled.
# Traces go to /v1/traces (proxied to motel by Vite) to avoid CORS.
debug-frontend: sync-assets
	@echo "==> frontend at http://localhost:5173 (OTel enabled, traces proxied to $(OTEL_ENDPOINT))"
	cd frontend && VITE_OTEL_ENABLED=true VITE_OTEL_ENDPOINT=/v1/traces npx vite

# Refresh the GeoIP MMDB used by the audit service for country flag lookups.
# Downloads the latest ip-to-country.mmdb from iplocate/ip-address-databases
# (free, no registration, no attribution required) into the repo so it's
# available for Docker builds and local dev. Commit the updated file.
GEOIP_DB_PATH := backend/cmd/audit/data/ip-to-country.mmdb
geoip:
	@echo "==> Downloading latest ip-to-country.mmdb from iplocate..."
	curl -fL -o $(GEOIP_DB_PATH) \
		https://github.com/iplocate/ip-address-databases/raw/main/ip-to-country/ip-to-country.mmdb
	@echo "==> Done. Restart the audit service to pick up the new database."
	@echo "    Commit the updated file: git add $(GEOIP_DB_PATH)"
