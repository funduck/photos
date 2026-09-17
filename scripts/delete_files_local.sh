#!/usr/bin/env bash
# Delete files listed in a text file (one absolute host path per line) both
# locally and from the S3 backup written by the rclone-s3-sync container.
#
# Local path  $STORAGE_DIR/$SUBDIR/<rel>  maps to  $RCLONE_REMOTE:$AWS_BUCKET/$SUBDIR/<rel>
#
# S3 deletes run inside the rclone_s3_sync container so they reuse its
# generated /tmp/rclone.conf and env (RCLONE_REMOTE, AWS_BUCKET, SYNC_NAME).
# Run this on the host (macOS), where the /Volumes/... paths exist.
#
# Usage:
#   scripts/delete_files.sh to_delete.txt             # dry run (default)
#   scripts/delete_files.sh to_delete.txt --execute   # really delete
set -euo pipefail

LIST_FILE="${1:?usage: $0 <list-file> [--execute]}"
MODE="${2:---dry-run}"
CONTAINER="rclone_s3_sync"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/.env"
: "${STORAGE_DIR:?STORAGE_DIR not set in scripts/.env}"

# Must match RCLONE_SYNC_SUBDIR of the container we exec into.
SUBDIR="$(docker exec "$CONTAINER" sh -c 'printf %s "$SYNC_NAME"')"
LOCAL_ROOT="$STORAGE_DIR/$SUBDIR/"

case "$MODE" in
  --dry-run) DRY_RUN=true ;;
  --execute) DRY_RUN=false ;;
  *) echo "unknown mode: $MODE (use --dry-run or --execute)" >&2; exit 1 ;;
esac

# Build the list of paths relative to the synced folder; refuse anything outside it.
REL_LIST="$(mktemp)"
trap 'rm -f "$REL_LIST"' EXIT
while IFS= read -r path || [[ -n "$path" ]]; do
  [[ -z "$path" ]] && continue
  if [[ "$path" != "$LOCAL_ROOT"* ]]; then
    echo "ERROR: not under $LOCAL_ROOT: $path" >&2
    exit 1
  fi
  printf '%s\n' "${path#"$LOCAL_ROOT"}" >> "$REL_LIST"
done < "$LIST_FILE"

echo "Mode: $MODE"
echo "Entries: $(wc -l < "$REL_LIST" | tr -d ' ')"
echo "Local root: $LOCAL_ROOT"
echo

# --- Local ------------------------------------------------------------------
# Runs after S3 so a failed S3 step (set -e) leaves local files untouched.
echo
echo "=== Local ==="
deleted=0; missing=0
while IFS= read -r rel; do
  path="$LOCAL_ROOT$rel"
  if [[ ! -e "$path" && ! -L "$path" ]]; then
    missing=$((missing + 1))
    continue
  fi
  if $DRY_RUN; then
    echo "would delete: $path"
  else
    rm -f -- "$path"
  fi
  deleted=$((deleted + 1))
done < "$REL_LIST"

echo
if $DRY_RUN; then
  echo "Dry run: $deleted local files would be deleted, $missing already missing."
  echo "Re-run with --execute to delete."
else
  echo "Deleted $deleted local files, $missing already missing."
fi
