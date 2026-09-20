#!/usr/bin/env bash
#
# Batch-transcode phone videos to HEVC using Apple's hardware encoder
# (VideoToolbox). Originals are never touched - output goes to a separate
# directory, mirroring the source tree.
#
#   ./transcode-videos.sh [SRC] [DST]
#
# Environment:
#   VT_Q=65               quality 1..100 (higher = better and bigger)
#   MIN_BITRATE=8000000   skip files below this bitrate
#   DRY_RUN=1             list what would be done, encode nothing
#
set -uo pipefail

SRC="${1:-/Users/oleg/SyncPhones}"
DST="${2:-$HOME/transcode-out}"
SRC="${SRC%/}"
DST="${DST%/}"

VT_Q="${VT_Q:-65}"
MIN_BITRATE="${MIN_BITRATE:-8000000}"
DRY_RUN="${DRY_RUN:-0}"

for cmd in ffmpeg ffprobe exiftool; do
  command -v "$cmd" >/dev/null 2>&1 || { echo "$cmd not found - brew install $cmd"; exit 1; }
done

[ -d "$SRC" ] || { echo "no such directory: $SRC"; exit 1; }
mkdir -p "$DST"

CSV="$DST/report.csv"
[ -f "$CSV" ] || echo "file,src_bytes,dst_bytes,percent,status" > "$CSV"

fsize() { stat -f%z "$1"; }

human() {
  awk -v b="$1" 'BEGIN{
    split("B KB MB GB TB", u, " "); i=1
    while (b >= 1024 && i < 5) { b /= 1024; i++ }
    printf "%.1f%s", b, u[i]
  }'
}

total_in=0; total_out=0
n_done=0; n_skip=0; n_fail=0

while IFS= read -r -d '' f; do
  rel="${f#"$SRC"/}"
  out="$DST/$rel"

  if [ -f "$out" ]; then
    echo "- already done: $rel"
    n_skip=$((n_skip + 1))
    continue
  fi

  # video stream bitrate, falling back to container bitrate
  br=$(ffprobe -v error -select_streams v:0 -show_entries stream=bit_rate \
       -of default=nw=1:nk=1 "$f" 2>/dev/null | head -1)
  case "$br" in
    ''|N/A) br=$(ffprobe -v error -show_entries format=bit_rate \
                 -of default=nw=1:nk=1 "$f" 2>/dev/null | head -1) ;;
  esac
  case "$br" in ''|N/A|*[!0-9]*) br=0 ;; esac

  if [ "$br" -lt "$MIN_BITRATE" ]; then
    echo "- low bitrate ($(( br / 1000 )) kbps), skipping: $rel"
    n_skip=$((n_skip + 1))
    continue
  fi

  in_bytes=$(fsize "$f")
  echo "> $rel  [$(human "$in_bytes"), $(( br / 1000000 )) Mbps]"

  if [ "$DRY_RUN" = "1" ]; then
    continue
  fi

  mkdir -p "$(dirname "$out")"
  # keep the real extension last - ffmpeg picks the container from it
  tmp="${out%.*}.part.${out##*.}"

  if ! ffmpeg -nostdin -v error -stats -y -i "$f" \
        -c:v hevc_videotoolbox -q:v "$VT_Q" -tag:v hvc1 \
        -fps_mode passthrough \
        -c:a copy \
        -map_metadata 0 -movflags use_metadata_tags+faststart \
        "$tmp"; then
    echo "  ENCODING FAILED"
    rm -f "$tmp"
    echo "\"$rel\",$in_bytes,0,,fail" >> "$CSV"
    n_fail=$((n_fail + 1))
    continue
  fi

  # carry over every tag from the original (dates, GPS, camera model),
  # except Rotation - ffmpeg already rotated the pixels, copying it back
  # would rotate the video twice
  if ! exiftool -q -q -TagsFromFile "$f" -all:all --Rotation -overwrite_original "$tmp"; then
    echo "  warning: exiftool did not copy metadata"
  fi

  # file mtime - Immich falls back to it when metadata has no date
  touch -r "$f" "$tmp"
  mv -f "$tmp" "$out"

  out_bytes=$(fsize "$out")
  pct=$(awk -v a="$out_bytes" -v b="$in_bytes" 'BEGIN{printf "%.0f", a*100/b}')
  status=ok
  [ "$out_bytes" -ge "$in_bytes" ] && status=bigger

  echo "  -> $(human "$out_bytes")  (${pct}% of original)"
  echo "\"$rel\",$in_bytes,$out_bytes,$pct,$status" >> "$CSV"

  total_in=$((total_in + in_bytes))
  total_out=$((total_out + out_bytes))
  n_done=$((n_done + 1))
done < <(
  find "$SRC" \
    \( -name '.stversions' -o -name '.stfolder' -o -name '@eaDir' \) -prune -o \
    -type f \( -iname '*.mp4' -o -iname '*.mov' \) -print0
)

echo
echo "done: $n_done   skipped: $n_skip   failed: $n_fail"
if [ "$total_in" -gt 0 ]; then
  echo "before: $(human "$total_in")"
  echo "after:  $(human "$total_out")"
  echo "saved:  $(human $((total_in - total_out)))"
fi
echo "report: $CSV"
