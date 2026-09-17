#!/usr/bin/env bash
# Delete files from a source folder (e.g. the host syncthing temp folder) that
# already have an identical copy at the same relative path in a destination
# folder (e.g. $STORAGE_DIR/Photos on the external drive).
#
# "Identical" = destination file exists, same size, and byte-for-byte equal (cmp).
# --size-only skips the cmp step (much faster, e.g. for a quick dry run).
# Files missing or different on the destination are kept and reported.
# Anything starting with "." is skipped: hidden files are ignored and hidden folders
# aren't descended into (covers syncthing's .stfolder/.stignore/.stversions/temp files).
# In-progress syncthing downloads (~syncthing~*.tmp) are skipped too.
# Only files last modified more than --min-age-days ago (default 30) are considered;
# newer files are left alone without being reported. Use --min-age-days 0 for all files.
#
# Usage:
#   scripts/delete_copied_files.sh <src-dir> <dst-dir> [--execute] [--min-age-days N] [--size-only]
#     --dry-run           list what would be deleted (default)
#     --execute           really delete
#     --min-age-days N    only files older than N days (default 30)
#     --size-only         compare by size only, skip byte-for-byte cmp
set -euo pipefail

USAGE="usage: $0 <src-dir> <dst-dir> [--dry-run|--execute] [--min-age-days N] [--size-only]"
SRC="${1:?$USAGE}"
DST="${2:?$USAGE}"
shift 2

MODE=--dry-run
MIN_AGE_DAYS=30
SIZE_ONLY=false
while (($#)); do
  case "$1" in
    --dry-run|--execute) MODE="$1"; shift ;;
    --size-only) SIZE_ONLY=true; shift ;;
    --min-age-days)
      [[ "${2:-}" =~ ^[0-9]+$ ]] || { echo "--min-age-days needs a non-negative integer" >&2; exit 1; }
      MIN_AGE_DAYS="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; echo "$USAGE" >&2; exit 1 ;;
  esac
done

case "$MODE" in
  --dry-run) DRY_RUN=true ;;
  --execute) DRY_RUN=false ;;
esac

SRC="${SRC%/}"
DST="${DST%/}"
[[ -d "$SRC" ]] || { echo "ERROR: source dir not found: $SRC" >&2; exit 1; }
# Guards against an unmounted external drive: nothing would match, but fail loudly anyway.
[[ -d "$DST" ]] || { echo "ERROR: destination dir not found: $DST" >&2; exit 1; }

echo "Mode: $MODE"
echo "Source: $SRC"
echo "Destination: $DST"
echo "Min age: $MIN_AGE_DAYS days"
echo "Compare: $($SIZE_ONLY && echo "size only" || echo "size + cmp")"
echo

deleted=0; missing=0; differ=0
while IFS= read -r -d '' path; do
  rel="${path#"$SRC"/}"
  dst_path="$DST/$rel"

  if [[ ! -f "$dst_path" ]]; then
    missing=$((missing + 1))
    echo "keep (missing on destination): $rel"
    continue
  fi
  if [[ "$(stat -c %s "$path" 2>/dev/null || stat -f %z "$path")" != \
        "$(stat -c %s "$dst_path" 2>/dev/null || stat -f %z "$dst_path")" ]] \
     || { ! $SIZE_ONLY && ! cmp -s -- "$path" "$dst_path"; }; then
    differ=$((differ + 1))
    echo "keep (differs on destination): $rel"
    continue
  fi

  if $DRY_RUN; then
    echo "would delete: $path"
  else
    rm -f -- "$path"
    echo "deleted: $path"
  fi
  deleted=$((deleted + 1))
done < <(find "$SRC" -mindepth 1 \
  -name '.*' -prune -o \
  -type f ! -name '~syncthing~*.tmp' \
  -mmin +$((MIN_AGE_DAYS * 24 * 60)) \
  -print0)

echo
echo "Kept: $missing missing on destination, $differ differ."
if $DRY_RUN; then
  echo "Dry run: $deleted files would be deleted. Re-run with --execute to delete."
else
  echo "Deleted $deleted files."
fi
