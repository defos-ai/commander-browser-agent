# syntax=docker/dockerfile:1
FROM golang:1.26.7-bookworm AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off
ARG TARGETARCH
# Pin both the release and its architecture-specific bytes. Keep overrides out
# of the build interface so a version change also requires checksum review.
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) asset=x86_64-linux; checksum=ae7d01d261efb9e480e18ce3424533332883863bcf70b12a4321b5733f37e19e ;; \
      arm64) asset=aarch64-linux; checksum=ac546dfc43c533430da92b75ab36f2a29da7d6324d5d38baf606ef2e0d849345 ;; \
      *) printf 'Unsupported Lightpanda architecture: %s\n' "$TARGETARCH" >&2; exit 1 ;; \
    esac; \
    mkdir -p /out; \
    curl --fail --location --retry 3 --connect-timeout 15 --max-time 180 \
      "https://github.com/lightpanda-io/browser/releases/download/1.0.2/lightpanda-$asset" \
      --output /out/lightpanda; \
    printf '%s  %s\n' "$checksum" /out/lightpanda | sha256sum --check; \
    chmod 755 /out/lightpanda
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -buildvcs=false -o /out/commander-browser-agent ./cmd

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN groupadd --gid 1000 commander \
    && useradd --uid 1000 --gid 1000 --create-home commander \
    && install -d -m 700 -o 1000 -g 1000 /home/commander/state
COPY --from=build /out/commander-browser-agent /usr/local/bin/commander-browser-agent
COPY --from=build /out/lightpanda /usr/local/bin/lightpanda
COPY third_party/lightpanda/ /usr/share/doc/lightpanda/
ENV HOME=/home/commander \
    LIGHTPANDA_PATH=/usr/local/bin/lightpanda LIGHTPANDA_DISABLE_TELEMETRY=true \
    LIGHTPANDA_DISABLE_CORE_DUMP=true
USER 1000:1000
WORKDIR /home/commander
ENTRYPOINT ["/usr/local/bin/commander-browser-agent"]
