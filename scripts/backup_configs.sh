#!/bin/bash

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

DST_DIR="${1:?usage: $0 <dst-dir>}"
if [ ! -d "$DST_DIR" ]; then
  echo "Destination directory does not exist: $DST_DIR"
  exit 1
fi

cp "${ROOT_DIR}/.env" "$DST_DIR"
cp "${ROOT_DIR}/scripts/.env" "$DST_DIR/.scripts-env"
cp "${ROOT_DIR}/services/album-exporter/config.yaml" "$DST_DIR/services-album-exporter-config.yaml"
cp "${ROOT_DIR}/services/person-album/config.yaml" "$DST_DIR/services-person-album-config.yaml"
