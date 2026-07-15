#!/bin/sh
# Back up the persistent Docker volumes (pb_data, audit_data) to tarballs.
#
# Uses a throwaway alpine container to read each named volume without
# stopping the stack. Safe to run while the app is live (SQLite WAL handles
# concurrent readers; the tar captures a consistent-enough snapshot for
# routine pre-upgrade backups — for point-in-time consistency, stop worldsim
# first).
#
# Usage:
#   ./backup-volumes.sh [output-dir]
#
# Default output dir is ./backups (relative to the dist root).
# Tarballs are named pb_data-YYYYMMDD-HHMMSS.tar.gz and
# audit_data-YYYYMMDD-HHMMSS.tar.gz.
#
# Volumes that don't exist yet (e.g. audit_data on a fresh deploy) are
# skipped with a notice, not an error.
#
# POSIX sh — no bashisms.

set -eu

COMPOSE_PROJECT="pixeleruv"
OUTPUT_DIR="${1:-./backups}"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)

# Volumes declared in docker-compose.yml.
VOLUMES="pb_data audit_data"

mkdir -p "$OUTPUT_DIR"

for vol in $VOLUMES; do
    full_name="${COMPOSE_PROJECT}_${vol}"

    # Check the volume exists before trying to back it up.
    if ! docker volume inspect "$full_name" >/dev/null 2>&1; then
        echo "  skip: volume $full_name does not exist yet"
        continue
    fi

    outfile="${OUTPUT_DIR}/${vol}-${TIMESTAMP}.tar.gz"
    echo "  backing up $full_name -> $outfile"
    docker run --rm -v "${full_name}:/data:ro" -v "$PWD:/backup" alpine \
        tar czf "/backup/${vol}-${TIMESTAMP}.tar.gz" -C /data .
done

echo "Done. Backups in ${OUTPUT_DIR}/"
