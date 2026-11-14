#!/bin/sh
set -eu

arch=${1:?target architecture is required}
source_dir=${2:?source destination is required}
uv_path=${3:?uv destination is required}

case "$arch" in
    amd64)
        uv_asset=x86_64-unknown-linux-gnu
        uv_checksum=f68d291c75783122dad58ab77def0e13f3233f358731db5d6d7e8715603b928f
        ;;
    arm64)
        uv_asset=aarch64-unknown-linux-gnu
        uv_checksum=4bb69cf95ed13f23713bb7bc09f241896ea0c75b7c557e615a20604b82f9c403
        ;;
    *) printf 'Unsupported Hermes architecture: %s\n' "$arch" >&2; exit 1 ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
fetch() {
    curl --fail --location --retry 3 --connect-timeout 15 --max-time 180 "$1" --output "$2"
    printf '%s  %s\n' "$3" "$2" | sha256sum --check
}

# Hermes v0.23.1. Fetch the immutable source tree, not an installation script.
fetch \
    https://github.com/NousResearch/hermes-agent/archive/3b421578588243f920886e6fe79a6faa87ea026e.tar.gz \
    "$work/hermes.tar.gz" \
    2525046f165880c22f177f2214c8da3e26c58cb0aeab5bd09aad940556538897
mkdir -p "$source_dir" "$work/uv"
tar -xzf "$work/hermes.tar.gz" --strip-components=1 -C "$source_dir"
printf '%s\n' '{"release":"v0.23.1","commit":"3b421578588243f920886e6fe79a6faa87ea026e","archive_sha256":"2525046f165880c22f177f2214c8da3e26c58cb0aeab5bd09aad940556538897"}' \
    > "$source_dir/commander-runtime.json"

# Use the same uv version and architecture-specific bytes as upstream pm/lock.json.
fetch \
    "https://github.com/astral-sh/uv/releases/download/0.13.5/uv-$uv_asset.tar.gz" \
    "$work/uv.tar.gz" "$uv_checksum"
tar -xzf "$work/uv.tar.gz" --strip-components=1 -C "$work/uv"
install -m 755 "$work/uv/uv" "$uv_path"
