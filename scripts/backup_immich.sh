#!/bin/bash

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/.env"

# Only library folder is important, it contains all assets and database dumps made by Immich
# Rest (postgres-data, redis, ml-model-cache) is not critical
src=$IMMICH_DIR/library/
dst=$IMMICH_BACKUP_DIR/library/

if [ ! -d "$dst" ]; then
    echo "Destination directory $dst does not exist. Creating."
    mkdir -p "$dst"
fi
if [ ! -d "$src" ]; then
    echo "Source directory $src does not exist. Exiting."
    exit 1
fi

rsync -av --delete --progress --dry-run "$src" "$dst"

echo "Immich backup $src to $dst completed successfully."
