# syntax=docker/dockerfile:1

# 1. Download the pinned browser runtimes.
FROM golang:1.26.7-bookworm AS build

WORKDIR /src
ARG TARGETARCH
COPY docker/scripts/fetch-lightpanda.sh /tmp/fetch-lightpanda.sh
RUN sh /tmp/fetch-lightpanda.sh "$TARGETARCH" /out/lightpanda
COPY docker/scripts/fetch-hermes.sh /tmp/fetch-hermes.sh
RUN sh /tmp/fetch-hermes.sh "$TARGETARCH" /out/hermes /out/uv

# 2. Build the Go host.
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    sh docker/scripts/build-agent.sh

# 3. Install Hermes and prepare the runtime user.
FROM python:3.14.10-slim-bookworm
COPY --from=build /out/hermes /opt/hermes
RUN --mount=type=bind,from=build,source=/out/uv,target=/tmp/uv,readonly \
    --mount=type=bind,source=docker/scripts/setup-runtime.sh,target=/tmp/setup-runtime.sh,readonly \
    sh /tmp/setup-runtime.sh

# 4. Package the host, runtime notices and readiness check.
COPY --from=build /out/commander-browser-agent /out/lightpanda /usr/local/bin/
COPY third_party/ /usr/share/doc/
COPY internal/commander/discovery/hermes/check.py /usr/local/lib/commander-browser/hermes/check.py

# 5. Verify readiness and run as the unprivileged agent user.
ENV HOME=/home/commander \
    LIGHTPANDA_PATH=/usr/local/bin/lightpanda LIGHTPANDA_DISABLE_TELEMETRY=true \
    LIGHTPANDA_DISABLE_CORE_DUMP=true
USER 1000:1000
WORKDIR /home/commander
RUN --mount=type=bind,source=docker/scripts/check-runtime.sh,target=/tmp/check-runtime.sh,readonly \
    sh /tmp/check-runtime.sh
ENTRYPOINT ["/usr/local/bin/commander-browser-agent"]
