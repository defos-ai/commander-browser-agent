#!/bin/sh
set -eu

arch=${1:?target architecture is required}
output=${2:?output binary is required}

# Pin both the release and its architecture-specific bytes. Keep overrides out
# of the build interface so a version change also requires checksum review.
case "$arch" in
    amd64)
        asset=x86_64-linux
        checksum=ae7d01d261efb9e480e18ce3424533332883863bcf70b12a4321b5733f37e19e
        ;;
    arm64)
        asset=aarch64-linux
        checksum=ac546dfc43c533430da92b75ab36f2a29da7d6324d5d38baf606ef2e0d849345
        ;;
    *) printf 'Unsupported Lightpanda architecture: %s\n' "$arch" >&2; exit 1 ;;
esac

mkdir -p "$(dirname "$output")"
curl --fail --location --retry 3 --connect-timeout 15 --max-time 180 \
    "https://github.com/lightpanda-io/browser/releases/download/1.0.2/lightpanda-$asset" \
    --output "$output"
printf '%s  %s\n' "$checksum" "$output" | sha256sum --check
chmod 755 "$output"
