#!/usr/bin/env bash
#
# Compare transcoded files against their originals:
# dates, GPS, camera model, duration, resolution, file mtime.
#
#   ./verify-metadata.sh [SRC] [DST]
#
# VERBOSE=1     also print files that matched
# GPS_TOL=0.001 GPS tolerance in degrees (~100 m)
#
# Note: rotation is baked into the pixels during transcode, so a portrait
# clip stored as 1920x1080 + rotation matrix becomes a real 1080x1920.
# Dimensions are therefore compared orientation-agnostically.
#
set -uo pipefail

SRC="${1:-/Users/oleg/SyncPhones}"
DST="${2:-$HOME/transcode-out}"
SRC="${SRC%/}"
DST="${DST%/}"
VERBOSE="${VERBOSE:-0}"
GPS_TOL="${GPS_TOL:-0.0001}"

TAGS=(CreateDate MediaCreateDate TrackCreateDate Make Model)
GPS_TAGS=(GPSLatitude GPSLongitude)

n_ok=0; n_bad=0; n_missing=0

tagval() {
  exiftool -q -q -s3 -n "-$2" "$1" 2>/dev/null | head -1
}

dur() {
  ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$1" 2>/dev/null | head -1
}

# width and height as a sorted "short x long" pair, so a rotated clip
# still compares equal to its original
dims() {
  ffprobe -v error -select_streams v:0 -show_entries stream=width,height \
    -of default=nw=1:nk=1 "$1" 2>/dev/null \
  | head -2 | sort -n | tr '\n' 'x' | sed 's/x$//'
}

num_close() {
  awk -v a="$1" -v b="$2" -v t="$3" 'BEGIN{
    if (a == "" || b == "") exit 1
    d = a - b; if (d < 0) d = -d
    exit !(d <= t)
  }'
}

while IFS= read -r -d '' out; do
  rel="${out#"$DST"/}"
  src="$SRC/$rel"

  if [ ! -f "$src" ]; then
    echo "?  no original for: $rel"
    n_missing=$((n_missing + 1))
    continue
  fi

  problems=()

  for t in "${TAGS[@]}"; do
    a=$(tagval "$src" "$t")
    b=$(tagval "$out" "$t")
    if [ -n "$a" ] && [ "$a" != "$b" ]; then
      problems+=("$t: '$a' -> '${b:-empty}'")
    fi
  done

  for t in "${GPS_TAGS[@]}"; do
    a=$(tagval "$src" "$t")
    b=$(tagval "$out" "$t")
    [ -z "$a" ] && continue
    if [ -z "$b" ]; then
      problems+=("$t: '$a' -> empty")
    elif ! num_close "$a" "$b" "$GPS_TOL"; then
      problems+=("$t: '$a' -> '$b'")
    fi
  done

  da=$(dur "$src"); db=$(dur "$out")
  if [ -n "$da" ] && [ -n "$db" ] && ! num_close "$da" "$db" 0.5; then
    problems+=("duration: $da -> $db")
  fi

  ra=$(dims "$src"); rb=$(dims "$out")
  [ "$ra" != "$rb" ] && problems+=("resolution: $ra -> $rb")

  ma=$(stat -f%m "$src"); mb=$(stat -f%m "$out")
  if [ $(( ma > mb ? ma - mb : mb - ma )) -gt 2 ]; then
    problems+=("file mtime drifted")
  fi

  if [ ${#problems[@]} -eq 0 ]; then
    n_ok=$((n_ok + 1))
    [ "$VERBOSE" = "1" ] && echo "ok  $rel"
  else
    n_bad=$((n_bad + 1))
    echo "!!  $rel"
    printf '      %s\n' "${problems[@]}"
  fi
done < <(find "$DST" -type f \( -iname '*.mp4' -o -iname '*.mov' \) -print0)

echo
echo "matched: $n_ok   mismatched: $n_bad   no original: $n_missing"
