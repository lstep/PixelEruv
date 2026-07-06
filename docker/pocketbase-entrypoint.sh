#!/bin/sh
set -e

# Create the superuser if it doesn't exist (idempotent via upsert).
# The worldsim UserStore needs this to authenticate and persist player
# entity IDs / positions. Without it, entity IDs change on every reconnect,
# which breaks LiveKit A/V (participant identity != replication entity ID).
/pb/pocketbase superuser upsert "${PB_ADMIN_EMAIL:-admin@pixeleruv.local}" "${PB_ADMIN_PASSWORD:-password123}" 2>/dev/null || true

exec /pb/pocketbase serve --http=0.0.0.0:8090 --dir=/pb/pb_data
