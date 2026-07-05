.PHONY: proto build web up down logs debug debug-frontend debug-pocketbase

PROTO_DIR := proto
GO_OUT := backend/internal/pb
TS_OUT := frontend/src/proto
COMPOSE_FILE := dist/config/docker-compose.yml
DIST_BIN := dist/bin

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

# Build native Go binaries into dist/bin/
build:
	@mkdir -p $(DIST_BIN)
	cd backend && go build -o ../$(DIST_BIN)/pusher ./cmd/pusher
	cd backend && go build -o ../$(DIST_BIN)/worldsim ./cmd/worldsim

# Build frontend static assets into dist/web/
web:
	cd frontend && npx vite build

up:
	docker compose -f $(COMPOSE_FILE) up --build

down:
	docker compose -f $(COMPOSE_FILE) down

logs:
	docker compose -f $(COMPOSE_FILE) logs -f

# --- Debug with OpenTelemetry + motel ---
# Starts motel (if not running), a standalone NATS container, PocketBase,
# and the two Go services with OTEL_ENABLED=true so traces/logs ship to motel.
# Frontend is started separately via `make debug-frontend`.
# Stop everything with `make debug-stop`.
debug: debug-nats debug-pocketbase
	@command -v motel >/dev/null 2>&1 || { echo "motel not found — install from https://github.com/kitlangton/motel"; exit 1; }
	@motel start >/dev/null 2>&1 || true
	@echo "==> motel: $(OTEL_ENDPOINT) (TUI at http://127.0.0.1:27686)"
	@echo "==> starting worldsim + pusher with OTel enabled (Ctrl-C to stop)"
	@OTEL_ENABLED=true OTEL_EXPORTER_OTLP_ENDPOINT=$(OTEL_ENDPOINT) \
		NATS_URL=nats://127.0.0.1:$(NATS_PORT) TICK_HZ=10 \
		./$(DIST_BIN)/worldsim &
	@OTEL_ENABLED=true OTEL_EXPORTER_OTLP_ENDPOINT=$(OTEL_ENDPOINT) \
		NATS_URL=nats://127.0.0.1:$(NATS_PORT) WS_ADDR=:8081 \
		./$(DIST_BIN)/pusher
	@$(MAKE) debug-stop

# Start a standalone NATS container for the debug session.
debug-nats:
	@docker rm -f $(NATS_CONTAINER) >/dev/null 2>&1 || true
	@docker run -d --name $(NATS_CONTAINER) -p $(NATS_PORT):4222 nats:2.10-alpine -js >/dev/null
	@echo "==> NATS running on nats://127.0.0.1:$(NATS_PORT) (container: $(NATS_CONTAINER))"

# Start the PocketBase container for the debug session (port 8090).
debug-pocketbase:
	@docker compose -f $(COMPOSE_FILE) up -d --build pocketbase
	@echo "==> PocketBase on http://127.0.0.1:8090 (admin UI at /_/)"

# Stop the debug NATS container and PocketBase. Go services exit on Ctrl-C.
debug-stop:
	@docker rm -f $(NATS_CONTAINER) >/dev/null 2>&1 || true
	@docker compose -f $(COMPOSE_FILE) stop pocketbase >/dev/null 2>&1 || true
	@echo "==> debug session stopped"

# Start the Vite dev server with frontend OTel enabled.
# Traces go to /v1/traces (proxied to motel by Vite) to avoid CORS.
debug-frontend:
	@echo "==> frontend at http://localhost:5173 (OTel enabled, traces proxied to $(OTEL_ENDPOINT))"
	cd frontend && VITE_OTEL_ENABLED=true VITE_OTEL_ENDPOINT=/v1/traces npx vite
