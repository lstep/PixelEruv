#!/bin/sh
# Restore persistent Docker volumes (pb_data, audit_data) from a backup dir.
#
# Stops the stack, replaces the contents of each named volume from the
# latest matching tarball in the backup dir, then restarts the stack.
# Use this when something went wrong and you need to roll back to a
# previous backup.
#
# Usage:
#   ./restore-volumes.sh <backup-dir>
#
# The backup dir should contain files named like:
#   pb_data-YYYYMMDD-HHMMSS.tar.gz
#   audit_data-YYYYMMDD-HHMMSS.tar.gz
#
# If multiple backups exist for a volume, the latest (by filename sort)
# is used. Volumes without a matching tarball are left untouched.
#
# WARNING: this stops all services and overwrites volume contents.
# Connected clients will drop. Run this only when recovering from a
# failed upgrade or data corruption.
#
# POSIX sh — no bashisms.

set -eu

COMPOSE_PROJECT="pixeleruv"
VOLUMES="pb_data audit_data"

if [ $# -ne 1 ]; then
    echo "Usage: $0 <backup-dir>" >&2
    exit 1
fi

BACKUP_DIR="$1"

if [ ! -d "$BACKUP_DIR" ]; then
    echo "Error: backup dir not found: $BACKUP_DIR" >&2
    exit 1
fi

# Resolve to an absolute path so the docker bind mount matches the tarball paths.
BACKUP_DIR=$(cd "$BACKUP_DIR" && pwd)

echo "==> Stopping the stack before restore"
docker compose down

for vol in $VOLUMES; do
    full_name="${COMPOSE_PROJECT}_${vol}"

    # Find the latest tarball for this volume (sort -r picks newest timestamp).
    tarball=$(ls -1 "${BACKUP_DIR}/${vol}-"*.tar.gz 2>/dev/null | sort -r | head -1)

    if [ -z "$tarball" ]; then
        echo "  skip: no backup tarball for ${vol} in ${BACKUP_DIR}"
        continue
    fi

    echo "  restoring $full_name from $tarball"

    # Create the volume if it doesn't exist (e.g. after a full wipe).
    docker volume inspect "$full_name" >/dev/null 2>&1 || docker volume create "$full_name" >/dev/null

    # Clear existing contents, then extract the tarball into the volume.
    docker run --rm -v "${full_name}:/data" -v "${BACKUP_DIR}:/backup" alpine \
        sh -c "rm -rf /data/* /data/.[!.]* 2>/dev/null; tar xzf \"/backup/$(basename "$tarball")\" -C /data"
done

echo "==> Restarting the stack"
docker compose up -d

echo "Done. Restore complete."
