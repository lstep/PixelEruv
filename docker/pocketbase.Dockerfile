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

EXPOSE 8090

# start PocketBase — default CORS allows all origins (stateless, no cookies).
CMD ["/pb/pocketbase", "serve", "--http=0.0.0.0:8090", "--dir=/pb/pb_data"]
