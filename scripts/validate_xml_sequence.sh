#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture_dir="$repo_dir/imports/incoming_test"

if ! find "$fixture_dir" -maxdepth 1 -type f -name 'exp_wyk_*.xml' -print -quit | grep -q .; then
  echo "Brak fixture XML w $fixture_dir" >&2
  exit 1
fi

cache_dir="${TMPDIR:-/tmp}/pcm2www-xml-go-cache"
mkdir -p "$cache_dir"

cd "$repo_dir"
echo "Waliduję każdy stan pośredni XML na izolowanej bazie SQLite w pamięci..."
PCM2WWW_IMPORT_XML_FIXTURE_TESTS=1 GOCACHE="$cache_dir" \
  go test -count=1 -run '^TestImportRealXMLSequenceIntoIsolatedDB$' -v ./internal/integrations/importer
