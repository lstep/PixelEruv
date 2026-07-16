# Frontend: build with Vite, serve with nginx
FROM node:22-alpine AS builder

WORKDIR /build
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
# Sync game assets (sounds, non-character sprites) from the authoritative
# assets/ directory into frontend/public/ so Vite bundles them.
# frontend/public/assets/ is gitignored, so mkdir is required before COPY.
# See Makefile sync-game-assets target.
RUN mkdir -p frontend/public/assets/sounds frontend/public/assets/sprites
COPY assets/sounds/ frontend/public/assets/sounds/
COPY assets/sprites/ frontend/public/assets/sprites/
RUN npx vite build

FROM nginx:alpine
# openssl is needed by the entrypoint to generate a self-signed cert for HTTPS.
RUN apk add --no-cache openssl
COPY --from=builder /dist/web /usr/share/nginx/html
# Static welcome page (community-customizable).
COPY docker/welcome /var/www/welcome
# Bake the build version into the welcome pages. Defaults to "dev" for local
# builds; dist builds substitute the real git tag/commit during `make dist-stage`.
ARG VERSION=dev
RUN sed -i 's/__VERSION__/${VERSION}/g' /var/www/welcome/*.html
# Proxy /ws to the pusher service
COPY docker/nginx.conf /etc/nginx/conf.d/default.conf
# Entrypoint generates a self-signed cert (SANs from $TLS_HOSTS) then starts nginx.
COPY docker/frontend-entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
EXPOSE 80 443
