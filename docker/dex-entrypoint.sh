#!/bin/sh
# Template dex config.yaml with PUBLIC_HOST (default: localhost), then exec dex.
#
# The config template uses __PUBLIC_HOST__ as a placeholder inside the
# redirectURIs entry for remote HTTPS access (https://__PUBLIC_HOST__:4043/...).
# Set PUBLIC_HOST to your host's LAN IP or hostname at deploy time, e.g.
#   PUBLIC_HOST=192.168.1.10 docker compose up
set -e

PUBLIC_HOST="${PUBLIC_HOST:-localhost}"
TEMPLATE="/etc/dex/config.yaml.template"
OUT="/etc/dex/config.yaml"

sed "s/__PUBLIC_HOST__/${PUBLIC_HOST}/g" "$TEMPLATE" > "$OUT"

exec dex serve "$OUT"
