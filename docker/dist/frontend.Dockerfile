# Binary-based frontend image: serves pre-built dist/web/ with nginx.
# Build context is the dist/ directory.
FROM nginx:alpine
# openssl is needed by the entrypoint to generate a self-signed cert for HTTPS.
RUN apk add --no-cache openssl
COPY web/ /usr/share/nginx/html
COPY docker/welcome /var/www/welcome
COPY docker/nginx.conf /etc/nginx/conf.d/default.conf
COPY docker/frontend-entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
EXPOSE 80 443
