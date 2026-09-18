#!/usr/bin/env bash
# Delete files listed in a text file (one absolute host path per line) from the
# S3 backup written by the rclone-s3-sync container. Local files are not
# touched; use scripts/delete_files_local.sh with the same list for that.
#
# Local path  $STORAGE_DIR/<rel>  maps to  $RCLONE_REMOTE:$AWS_BUCKET/<rel>
#
# S3 deletes run inside the rclone_s3_sync container so they reuse its
# generated /tmp/rclone.conf and env (RCLONE_REMOTE, AWS_BUCKET).
# The rclone IAM user is add-only; grant the temporary delete policy from
# scripts/aws_setup.sh first.
#
# Usage:
#   scripts/delete_files_s3.sh to_delete.txt             # dry run (default)
#   scripts/delete_files_s3.sh to_delete.txt --execute   # really delete
set -euo pipefail

LIST_FILE="${1:?usage: $0 <list-file> [--execute]}"
MODE="${2:---dry-run}"
CONTAINER="rclone_s3_sync"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/.env"
: "${STORAGE_DIR:?STORAGE_DIR not set in scripts/.env}"

LOCAL_ROOT="$STORAGE_DIR/"

case "$MODE" in
  --dry-run) DRY_RUN=true ;;
  --execute) DRY_RUN=false ;;
  *) echo "unknown mode: $MODE (use --dry-run or --execute)" >&2; exit 1 ;;
esac

# Build the list of paths relative to $STORAGE_DIR; refuse anything outside it.
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

# --- S3 ---------------------------------------------------------------------
# rclone delete with --files-from-raw only touches the listed objects; entries
# missing in S3 are silently skipped.
echo "=== S3 ==="
RCLONE_FLAGS=(--config /tmp/rclone.conf --files-from-raw - -v)
$DRY_RUN && RCLONE_FLAGS+=(--dry-run)
docker exec -i "$CONTAINER" sh -c 'rclone delete "$RCLONE_REMOTE:$AWS_BUCKET" "$@"' _ "${RCLONE_FLAGS[@]}" < "$REL_LIST"
