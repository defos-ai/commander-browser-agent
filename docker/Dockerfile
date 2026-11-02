# syntax=docker/dockerfile:1
FROM golang:1.26.7-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -buildvcs=false -o /out/commander-browser-agent ./cmd

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS lightpanda
ARG TARGETARCH
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
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
# Preserve upstream notices without vendoring their text in this repository.
RUN set -eu; \
    source_commit=c5f7bb29b127040c899d4e181b5709f76fc71a6f; \
    mkdir -p /out/licenses; \
    curl --fail --location --retry 3 --connect-timeout 15 --max-time 180 \
      "https://raw.githubusercontent.com/lightpanda-io/browser/$source_commit/LICENSE" \
      --output /out/licenses/LICENSE; \
    curl --fail --location --retry 3 --connect-timeout 15 --max-time 180 \
      "https://raw.githubusercontent.com/lightpanda-io/browser/$source_commit/src/rust/render/fonts/LICENSE" \
      --output /out/licenses/FONT-LICENSE; \
    printf '%s  %s\n' \
      8486a10c4393cee1c25392769ddd3b2d6c242d6ec7928e1414efff7dfb2f07ef /out/licenses/LICENSE \
      7a083b136e64d064794c3419751e5c7dd10d2f64c108fe5ba161eae5e5958a93 /out/licenses/FONT-LICENSE | sha256sum --check; \
    printf '%s\n' \
      'Lightpanda browser 1.0.2' \
      'Copyright (C) 2023-2026 Lightpanda (Selecy SAS)' \
      'Francis Bouvier <francis@lightpanda.io>' \
      'Pierre Tachoire <pierre@lightpanda.io>' \
      'License: GNU AGPL version 3 or any later version (see LICENSE).' \
      'Bundled font notices: see FONT-LICENSE.' \
      "Upstream source: https://github.com/lightpanda-io/browser/tree/$source_commit" \
      "Source archive: https://github.com/lightpanda-io/browser/archive/$source_commit.tar.gz" \
      > /out/licenses/NOTICE

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 1000 commander \
    && useradd --uid 1000 --gid 1000 --create-home commander \
    && mkdir -p /state \
    && chown 1000:1000 /state
COPY --from=build /out/commander-browser-agent /usr/local/bin/commander-browser-agent
COPY --from=lightpanda /out/lightpanda /usr/local/bin/lightpanda
COPY --from=lightpanda /out/licenses/ /usr/share/doc/lightpanda/
# Provide a writable home for the unprivileged browser runtime.
ENV HOME=/home/commander \
    LIGHTPANDA_PATH=/usr/local/bin/lightpanda LIGHTPANDA_DISABLE_TELEMETRY=true \
    LIGHTPANDA_DISABLE_CORE_DUMP=true \
    BUS_URL=file:///state/bus STORAGE_URL=file:///state/storage
USER 1000:1000
WORKDIR /state
ENTRYPOINT ["/usr/local/bin/commander-browser-agent"]
