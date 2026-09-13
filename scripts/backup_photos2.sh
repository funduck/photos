#!/bin/bash
# Run everytime when you want to backup photos from host to storage2. This is additional backup

src=/Users/oleg/SyncPhones/oleg-pixel/
dst=/Volumes/Expansion/data/Oleg/Photos/oleg-pixel/

if [ ! -d "$dst" ]; then
    echo "Destination directory $dst does not exist. Exiting."
    exit 1
fi
if [ ! -d "$src" ]; then
    echo "Source directory $src does not exist. Exiting."
    exit 1
fi

rsync -av --ignore-existing --progress "$src" "$dst"

echo "Backup $src to $dst completed successfully."
