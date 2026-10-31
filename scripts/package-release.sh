#!/usr/bin/env bash
set -euo pipefail

# Local packaging only: never installs, publishes, reloads or restarts a host.
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
go_bin=${GO:-go}
version=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["plugins"][0]["version"])' "$root/registry.json")
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid release version' >&2; exit 1; }
output=${1:-"$root/dist"}
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
cache=${XDG_CACHE_HOME:-"$HOME/.cache"}
mkdir -p -- "$cache"
scratch=$(mktemp -d "$cache/cloudflare-release.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
export TMPDIR="$scratch"

CGO_ENABLED=1 GOOS=linux GOARCH=amd64 "$go_bin" -C "$root" build -trimpath -buildmode=c-shared -o "$scratch/cloudflare-workers-ai.so" .
nm -D "$scratch/cloudflare-workers-ai.so" > "$scratch/symbols.txt"
grep -q ' T cliproxy_plugin_init$' "$scratch/symbols.txt"
objdump -T "$scratch/cloudflare-workers-ai.so" > "$scratch/dynamic-symbols.txt"
python3 -I - "$scratch/dynamic-symbols.txt" <<'PY'
import pathlib, re, sys
versions = {tuple(map(int, v.split('.'))) for v in re.findall(r'GLIBC_([0-9.]+)', pathlib.Path(sys.argv[1]).read_text())}
if not versions or max(versions) > (2, 34):
    sys.exit('Production library requires glibc newer than 2.34 or has no verifiable glibc contract')
print('Maximum required glibc:', '.'.join(map(str, max(versions))))
PY
archive="cloudflare-workers-ai_${version}_linux_amd64.zip"
zip -q -X -j "$scratch/$archive" "$scratch/cloudflare-workers-ai.so" "$root/LICENSE"
(cd -- "$scratch" && sha256sum "$archive") > "$scratch/checksums.txt"
cp -- "$scratch/$archive" "$scratch/checksums.txt" "$output/"
printf 'Created %s and checksums.txt\n' "$archive"
