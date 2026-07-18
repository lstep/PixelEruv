# ext-rec variant of the binary-based image. Identical to backend.Dockerfile
# but adds ffmpeg, which ext-rec needs to extract audio (MP3) from the Egress
# MP4 after a recording stops. Kept separate so the other 9 backend images
# don't carry the ~130MB ffmpeg dependency.
FROM alpine:3.20
ARG BINARY
RUN apk add --no-cache ca-certificates ffmpeg
COPY bin/${BINARY} /usr/local/bin/app
COPY bin/seed-sprites /usr/local/bin/seed-sprites
COPY sprites /sprites
ENV SPRITES_DIR=/sprites
COPY maps /maps
ENV MAP_DIR=/maps
COPY geoip/ip-to-country.mmdb /opt/geoip/ip-to-country.mmdb
ENV GEOIP_DB=/opt/geoip/ip-to-country.mmdb
ENTRYPOINT ["app"]
