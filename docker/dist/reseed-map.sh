#!/bin/sh
# Delete the 'main' map record from PocketBase and restart worldsim so it
# re-seeds from the bundled dist/maps/default-map.json.
#
# Needed because worldsim's SeedMapIfMissing is idempotent: it only uploads
# the map if no 'maps' record named 'main' exists. After editing the map JSON
# and running deploy-remote, the remote still uses the old map from PB until
# you run this.
#
# Run on the remote host (the dist root, e.g. /opt/pixeleruv/):
#   ./reseed-map.sh
#
# Requires curl and jq on the host. PB admin credentials must match
# PB_ADMIN_EMAIL/PB_ADMIN_PASSWORD in docker-compose.yml.
#
# POSIX sh — no bashisms.

set -eu

PB_URL="http://localhost:8090"
PB_ADMIN_EMAIL="${PB_ADMIN_EMAIL:-admin@pixeleruv.local}"
PB_ADMIN_PASSWORD="${PB_ADMIN_PASSWORD:-password123}"

if ! command -v jq >/dev/null 2>&1; then
    echo "ERROR: jq is required but not installed." >&2
    exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
    echo "ERROR: curl is required but not installed." >&2
    exit 1
fi

echo "==> Authenticating to PocketBase as ${PB_ADMIN_EMAIL}"
TOKEN=$(curl -s -X POST "${PB_URL}/api/collections/_superusers/auth-with-password" \
    -H 'Content-Type: application/json' \
    -d "{\"identity\":\"${PB_ADMIN_EMAIL}\",\"password\":\"${PB_ADMIN_PASSWORD}\"}" \
    | jq -r .token)

if [ -z "${TOKEN}" ] || [ "${TOKEN}" = "null" ]; then
    echo "ERROR: failed to authenticate to PocketBase." >&2
    exit 1
fi

echo "==> Looking up 'main' map record"
RECORD_ID=$(curl -s "${PB_URL}/api/collections/maps/records?filter=name%3D%27main%27" \
    -H "Authorization: ${TOKEN}" \
    | jq -r '.items[0].id // empty')

if [ -z "${RECORD_ID}" ]; then
    echo "    No 'main' map record found — nothing to delete. worldsim will seed on next start."
else
    echo "==> Deleting map record: ${RECORD_ID}"
    curl -s -X DELETE "${PB_URL}/api/collections/maps/records/${RECORD_ID}" \
        -H "Authorization: ${TOKEN}" >/dev/null
    echo "    Deleted."
fi

echo "==> Restarting worldsim"
docker compose restart worldsim

echo "==> Done. worldsim will re-seed the map from dist/maps/default-map.json on start."
