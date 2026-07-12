# Multi-stage build for both Pusher and World Sim
FROM golang:1.26-alpine AS builder

ARG VERSION=dev

WORKDIR /build

# Copy go module files
COPY backend/go.mod backend/go.sum ./
RUN go mod download

# Copy source
COPY backend/ ./
COPY proto/ ../proto/

# Build both binaries with version injected via ldflags
RUN go build -ldflags="-X github.com/lstep/pixeleruv/backend/internal/version.Version=${VERSION}" -o /out/pusher ./cmd/pusher
RUN go build -ldflags="-X github.com/lstep/pixeleruv/backend/internal/version.Version=${VERSION}" -o /out/worldsim ./cmd/worldsim
RUN go build -ldflags="-X github.com/lstep/pixeleruv/backend/internal/version.Version=${VERSION}" -o /out/seed-sprites ./cmd/seed-sprites
RUN go build -ldflags="-X github.com/lstep/pixeleruv/backend/internal/version.Version=${VERSION}" -o /out/ext-demo ./cmd/ext-demo
RUN go build -ldflags="-X github.com/lstep/pixeleruv/backend/internal/version.Version=${VERSION}" -o /out/ext-walls ./cmd/ext-walls
RUN go build -ldflags="-X github.com/lstep/pixeleruv/backend/internal/version.Version=${VERSION}" -o /out/ext-props ./cmd/ext-props
RUN go build -ldflags="-X github.com/lstep/pixeleruv/backend/internal/version.Version=${VERSION}" -o /out/ext-av ./cmd/ext-av
RUN go build -ldflags="-X github.com/lstep/pixeleruv/backend/internal/version.Version=${VERSION}" -o /out/audit ./cmd/audit

# --- Pusher image ---
FROM alpine:3.20 AS pusher
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/pusher /usr/local/bin/pusher
ENTRYPOINT ["pusher"]

# --- World Sim image ---
FROM alpine:3.20 AS worldsim
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/worldsim /usr/local/bin/worldsim
COPY --from=builder /out/seed-sprites /usr/local/bin/seed-sprites
COPY spritesheets /sprites
COPY maps /maps
ENV SPRITES_DIR=/sprites
ENV MAP_DIR=/maps
ENTRYPOINT ["worldsim"]

# --- ext-demo image ---
FROM alpine:3.20 AS ext-demo
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/ext-demo /usr/local/bin/ext-demo
ENTRYPOINT ["ext-demo"]

# --- ext-walls image ---
FROM alpine:3.20 AS ext-walls
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/ext-walls /usr/local/bin/ext-walls
ENTRYPOINT ["ext-walls"]

# --- ext-props image ---
FROM alpine:3.20 AS ext-props
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/ext-props /usr/local/bin/ext-props
ENTRYPOINT ["ext-props"]

# --- ext-av image ---
FROM alpine:3.20 AS ext-av
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/ext-av /usr/local/bin/ext-av
ENTRYPOINT ["ext-av"]

# --- audit image ---
# Templates and static files are embedded in the binary via go:embed.
FROM alpine:3.20 AS audit
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/audit /usr/local/bin/audit
ENTRYPOINT ["audit"]
