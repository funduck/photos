rclone copy $STORAGE_DIR/Photos $RCLONE_REMOTE:$AWS_BUCKET/Photos \
  --s3-storage-class DEEP_ARCHIVE \
  --size-only \
  --fast-list \
  --transfers 8 \
  --checkers 16 \
  --progress
