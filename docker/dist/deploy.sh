#!/bin/sh
# Upgrade a running Pixel Eruv stack with minimal disruption and no data loss.
#
# This is the safe replacement for the old "prune everything and rebuild"
# workflow. It:
#   1. Copies ../.env into the dist root if present (preserves your config).
#   2. Backs up the persistent Docker volumes (pb_data, audit_data).
#   3. Builds new images while old containers keep serving (no downtime yet).
#   4. Recreates only services whose image changed (volumes untouched).
#   5. Prunes dangling images only — NEVER volumes.
#
# Usage (from the dist root on the server, e.g. /opt/pixeleruv/):
#   ./deploy.sh
#
# To roll back a bad upgrade, use restore-volumes.sh with the backup
# produced in step 2 (./backups/).
#
# NEVER run `docker volume prune` or `docker system prune --volumes` —
# those delete the named volumes that hold your users and config.
#
# POSIX sh — no bashisms.

set -eu

# 1. Bring in the .env file if it lives one level up (common layout where
#    the dist root is /opt/pixeleruv/ and .env is at /opt/.env).
if [ -f ../.env ] && [ ! -f .env ]; then
    cp ../.env .env
    echo "==> Copied ../.env -> .env"
fi

# 2. Back up persistent volumes before touching anything.
echo "==> Backing up volumes"
./backup-volumes.sh

# 3. Build new images. Old containers keep serving during the build.
echo "==> Building images (old containers still serving)"
docker compose build

# 4. Recreate only changed services. Named volumes (pb_data, audit_data)
#    are preserved — docker compose up -d never deletes volumes.
echo "==> Recreating changed services"
docker compose up -d

# 5. Clean up dangling images only. NEVER prune volumes.
echo "==> Pruning dangling images"
docker image prune -f

echo ""
echo "==> Deploy complete."
echo "    Backups saved to ./backups/ — use restore-volumes.sh to roll back."
echo "    Check status: docker compose ps"
