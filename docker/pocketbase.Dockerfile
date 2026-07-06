# PocketBase — standalone binary serving the `maps` collection.
# Based on the official example at
# https://pocketbase.io/docs/going-to-production/#using-docker
FROM alpine:latest

ARG PB_VERSION=0.39.5

RUN apk add --no-cache \
  unzip \
  ca-certificates

# download and unzip PocketBase
ADD https://github.com/pocketbase/pocketbase/releases/download/v${PB_VERSION}/pocketbase_${PB_VERSION}_linux_amd64.zip /tmp/pb.zip
RUN unzip /tmp/pb.zip -d /pb/

# copy migrations (collection schema definitions)
COPY pb_migrations /pb/pb_migrations

# entrypoint creates the superuser (idempotent) then starts PocketBase.
COPY docker/pocketbase-entrypoint.sh /pb/entrypoint.sh
RUN chmod +x /pb/entrypoint.sh

EXPOSE 8090

ENTRYPOINT ["/pb/entrypoint.sh"]
