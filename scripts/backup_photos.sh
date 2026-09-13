#!/bin/bash
# Run everytime when you want to backup photos from host to storage

src=$PHONE_TMP_DIR/
dst=$PHONE_STORAGE_DIR/

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
