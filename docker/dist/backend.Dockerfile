# Binary-based image: copies a pre-built binary from dist/bin/ instead of
# compiling from source. Used by dist/docker-compose.yml with:
#   args:
#     BINARY: pusher | worldsim | ext-demo | ext-walls | ext-props | ext-av
FROM alpine:3.20
ARG BINARY
RUN apk add --no-cache ca-certificates
COPY bin/${BINARY} /usr/local/bin/app
# Bundle the character spritesheets so worldsim can seed sprite_bases on the
# first run. Other services ignore SPRITES_DIR.
COPY sprites /sprites
ENV SPRITES_DIR=/sprites
ENTRYPOINT ["app"]
