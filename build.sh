#!/usr/bin/env bash
set -euo pipefail

ver="${1:-1.0.0}"        # wersja z parametru albo 1.0.0
pkg="./"
out="bin/pcm2www-sync.exe"

mkdir -p bin

GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-H=windowsgui -s -w -X main.ver=${ver}" \
  -o "${out}" "${pkg}"

echo "Built ${out} (version ${ver})"
