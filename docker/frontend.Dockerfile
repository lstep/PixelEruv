# Frontend: build with Vite, serve with nginx
FROM node:22-alpine AS builder

WORKDIR /build
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npx vite build

FROM nginx:alpine
# openssl is needed by the entrypoint to generate a self-signed cert for HTTPS.
RUN apk add --no-cache openssl
COPY --from=builder /dist/web /usr/share/nginx/html
# Proxy /ws to the pusher service
COPY docker/nginx.conf /etc/nginx/conf.d/default.conf
# Entrypoint generates a self-signed cert (SANs from $TLS_HOSTS) then starts nginx.
COPY docker/frontend-entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
EXPOSE 80 443
