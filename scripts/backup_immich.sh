#!/bin/bash
# Run after immich is down

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/.env"

src=$IMMICH_DIR/
dst=$IMMICH_BACKUP_DIR/

if [ ! -d "$dst" ]; then
    echo "Destination directory $dst does not exist. Creating."
    mkdir -p "$dst"
fi
if [ ! -d "$src" ]; then
    echo "Source directory $src does not exist. Exiting."
    exit 1
fi

# Backup all except database
rsync -av --ignore-existing --progress --exclude '/postgres-data' "$src" "$dst"
echo "Immich assets backup completed successfully."

# Backup database
isDbRunning=$(docker ps -q -f name=immich_postgres)
if [ -n "$isDbRunning" ]; then
    echo "Immich database is running. Stopping it for backup."
    docker stop immich_postgres
fi
rsync -av --progress "$src/postgres-data" "$dst/postgres-data"
if [ -n "$isDbRunning" ]; then
    echo "Starting Immich database back up."
    docker start immich_postgres
fi

echo "Immich backup $src to $dst completed successfully."
