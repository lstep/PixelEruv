.PHONY: proto build web up down logs

PROTO_DIR := proto
GO_OUT := backend/internal/pb
TS_OUT := frontend/src/proto
COMPOSE_FILE := dist/config/docker-compose.yml
DIST_BIN := dist/bin

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
