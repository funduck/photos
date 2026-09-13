#!/bin/bash
# Run after immitch is down

src=/Users/oleg/Immitch/
dst=/Volumes/EXTDATA/Oleg/Immitch/

if [ ! -d "$dst" ]; then
    echo "Destination directory $dst does not exist. Exiting."
    mkdir -p "$dst"
fi
if [ ! -d "$src" ]; then
    echo "Source directory $src does not exist. Exiting."
    exit 1
fi

# Backup all except database
rsync -av --ignore-existing --progress --exclude '/postgres-data' "$src" "$dst"
echo "Immitch assets backup completed successfully."

# Backup database
isDbRunning=$(docker ps -q -f name=immitch_postgres)
if [ -n "$isDbRunning" ]; then
    echo "Immitch database is running. Stopping it for backup."
    docker stop immitch_postgres
fi
rsync -av --progress "$src/postgres-data" "$dst/postgres-data"
if [ -n "$isDbRunning" ]; then
    echo "Starting Immitch database back up."
    docker start immitch_postgres
fi

echo "Immitch backup $src to $dst completed successfully."
