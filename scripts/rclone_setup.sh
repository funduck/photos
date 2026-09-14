# Just a collection of steps required to rclone with s3 remote
# Run manually one by one

rclone config

rclone lsd $RCLONE_REMOTE:$AWS_BUCKET

# dry-run
rclone copy $STORAGE_DIR/Photos $RCLONE_REMOTE:$AWS_BUCKET/Photos \
  --s3-storage-class DEEP_ARCHIVE \
  --size-only \
  --fast-list \
  --transfers 8 \
  --checkers 16 \
  --progress \
  --dry-run

# Now testing on small subset of files, to make sure everything works fine
find $STORAGE_DIR/Photos -type f | head -10 | \
  sed "s|$STORAGE_DIR/Photos||" > /tmp/test-files.txt

rclone copy $STORAGE_DIR/Photos $RCLONE_REMOTE:$AWS_BUCKET/test \
  --s3-storage-class DEEP_ARCHIVE \
  --files-from /tmp/test-files.txt \
  --progress

rclone lsl $RCLONE_REMOTE:$AWS_BUCKET/test
