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
COPY docker/fetch-hermes.sh /tmp/fetch-hermes.sh
RUN sh /tmp/fetch-hermes.sh "$TARGETARCH" /out/hermes /out/uv
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -buildvcs=false -o /out/commander-browser-agent ./cmd

FROM python:3.14.10-slim-bookworm@sha256:3fe1a0b6c2d6b2ebb572906cdabeca75aeb558c0c17db5ffefbcf8094871c32b AS hermes
ENV UV_PYTHON_DOWNLOADS=never UV_LINK_MODE=copy \
    PYTHONDONTWRITEBYTECODE=1
COPY --from=build /out/uv /tmp/uv
COPY --from=build /out/hermes /opt/hermes
WORKDIR /opt/hermes
# Frozen core only: require published wheels on both native Linux architectures.
# Keep upstream source and lockfiles intact; no browser/agent extras or lazy installs.
RUN /tmp/uv sync --frozen --no-dev --no-install-project --no-build \
    --python /usr/local/bin/python3 --no-cache

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
       libbz2-1.0 libexpat1 libffi8 libgdbm6 libgdbm-compat4 liblzma5 \
       libncursesw6 libreadline8 libsqlite3-0 libssl3 libstdc++6 libuuid1 \
       libzstd1 zlib1g \
    && rm -rf /var/lib/apt/lists/*
RUN groupadd --gid 1000 commander \
    && useradd --uid 1000 --gid 1000 --create-home commander \
    && install -d -m 700 -o 1000 -g 1000 /home/commander/state /home/commander/state/hermes
COPY --from=hermes /usr/local/ /usr/local/
COPY --from=hermes /opt/hermes /opt/hermes
RUN ldconfig
COPY --from=build /out/commander-browser-agent /usr/local/bin/commander-browser-agent
COPY --from=build /out/lightpanda /usr/local/bin/lightpanda
COPY third_party/lightpanda/ /usr/share/doc/lightpanda/
COPY third_party/hermes/ /usr/share/doc/hermes/
COPY browser/hermes/check.py /usr/local/lib/commander-browser/hermes/check.py
COPY internal/hermes/*.py /usr/local/lib/commander-browser/hermes/
COPY internal/hermes/plugin/ /usr/local/lib/commander-browser/hermes/plugin/
ENV HOME=/home/commander \
    LIGHTPANDA_PATH=/usr/local/bin/lightpanda LIGHTPANDA_DISABLE_TELEMETRY=true \
    LIGHTPANDA_DISABLE_CORE_DUMP=true \
    HERMES_HOME=/home/commander/state/hermes HERMES_INSTALL_ROOT=/opt/hermes \
    HERMES_DISABLE_LAZY_INSTALLS=1 ANONYMIZED_TELEMETRY=false \
    PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=/opt/hermes \
    PATH=/opt/hermes/.venv/bin:$PATH
USER 1000:1000
WORKDIR /home/commander
# Check the copied runtime as its real user, without model calls or browser startup.
RUN /opt/hermes/.venv/bin/python -I -S /usr/local/lib/commander-browser/hermes/check.py
ENTRYPOINT ["/usr/local/bin/commander-browser-agent"]
