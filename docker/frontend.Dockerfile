# Frontend: build with Vite, serve with nginx
FROM node:22-alpine AS builder

WORKDIR /build
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npx vite build

FROM nginx:alpine
COPY --from=builder /dist/web /usr/share/nginx/html
# Proxy /ws to the pusher service
COPY docker/nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
