# Binary-based image: copies a pre-built binary from dist/bin/ instead of
# compiling from source. Used by dist/docker-compose.yml with:
#   args:
#     BINARY: pusher | worldsim | ext-demo | ext-walls | ext-props | ext-av | audit
FROM alpine:3.20
ARG BINARY
RUN apk add --no-cache ca-certificates
COPY bin/${BINARY} /usr/local/bin/app
# Include the seed-sprites CLI so admins can add sheets to a running worldsim
# container (docker compose exec worldsim seed-sprites -dir /sprites -force).
# Other services ignore it.
COPY bin/seed-sprites /usr/local/bin/seed-sprites
# Bundle the character spritesheets so worldsim can seed sprite_bases on the
# first run. Other services ignore SPRITES_DIR.
COPY sprites /sprites
ENV SPRITES_DIR=/sprites
# Bundle the default map (Tiled JSON + tileset PNGs) so worldsim can seed the
# maps collection on first run. Other services ignore MAP_DIR.
COPY maps /maps
ENV MAP_DIR=/maps
ENTRYPOINT ["app"]
