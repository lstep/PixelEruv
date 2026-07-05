# Multi-stage build for both Pusher and World Sim
FROM golang:1.26-alpine AS builder

WORKDIR /build

# Copy go module files
COPY backend/go.mod backend/go.sum ./
RUN go mod download

# Copy source
COPY backend/ ./
COPY proto/ ../proto/

# Build both binaries
RUN go build -o /out/pusher ./cmd/pusher
RUN go build -o /out/worldsim ./cmd/worldsim
RUN go build -o /out/ext-demo ./cmd/ext-demo

# --- Pusher image ---
FROM alpine:3.20 AS pusher
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/pusher /usr/local/bin/pusher
ENTRYPOINT ["pusher"]

# --- World Sim image ---
FROM alpine:3.20 AS worldsim
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/worldsim /usr/local/bin/worldsim
ENTRYPOINT ["worldsim"]

# --- ext-demo image ---
FROM alpine:3.20 AS ext-demo
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/ext-demo /usr/local/bin/ext-demo
ENTRYPOINT ["ext-demo"]
