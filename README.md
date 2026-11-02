# Commander browser agent

Render pages with Chromium, capture desktop/mobile screenshots and check rendered
accessibility for [Commander](https://github.com/defos-ai/commander) missions.

| Action | Result |
| --- | --- |
| `capture-page` | Desktop/mobile PNGs, page title and horizontal overflow findings |
| `check-accessibility` | Bounded checks for headings, image alternatives, form labels and links |

Static HTML checks and crawling belong to the
[web agent](https://github.com/defos-ai/commander-web-agent).

## Try it locally

Requires Docker Compose. The image includes Chromium and Lightpanda `1.0.2`; the
demo uses Commander `v0.3.0` host images.

```sh
docker compose up --build --wait
docker compose run --rm --no-deps playground
```

Choose a model provider and enter its API key. Try:

> Run review-page for https://defos.ai and summarize the screenshots and accessibility findings.

Open [Admin](http://localhost:4285) to inspect results and download screenshots.
Add `http://localhost:4284`; get its credentials with:

```sh
docker compose exec api commander-api credentials
```

Use `/exit` to close Playground and `docker compose down` to stop the stack.

## Connect to Commander

Agents need the controller's bus and artifact storage, never its database.

```sh
BUS_URL=/shared/bus STORAGE_URL=/shared/storage go run ./cmd
```

FS paths must point to the same shared directories as the controller. Distributed
deployments use RabbitMQ (`BUS_URL=amqp://…` or `amqps://…`) and S3-compatible
artifact storage (`STORAGE_URL=s3://bucket`), with service credentials supplied
through the deployment environment.

Native runs need Chromium or Chrome. Set `CHROMIUM_PATH` to an absolute executable
path if discovery cannot find it. Native sandboxing stays enabled; the Docker
image sets `CHROMIUM_NO_SANDBOX=true` and runs as an unprivileged user.

For native Lightpanda support, install
[Lightpanda 1.0.2](https://github.com/lightpanda-io/browser/releases/tag/1.0.2) on
`PATH`, or set `LIGHTPANDA_PATH` to its absolute executable path. Discovery checks
the installed version before reporting readiness. Linux builds require glibc;
Windows hosts can use the Docker image or WSL. The image disables Lightpanda
telemetry and crash core dumps by default.

Lightpanda is packaged for browser automation. The current `capture-page` and
`check-accessibility` actions continue to use Chromium; packaging Lightpanda does
not add browsing, login or reusable sessions yet.

Run `go run ./cmd --help` for settings. A spawner sets
`COMMANDER_AGENT_ID_SUFFIX=0123abcd`, producing `browser-agent-0123abcd`.
`--agent-id-suffix` overrides that environment variable. Use eight lowercase hex
characters. Omit both settings or pass `--agent-id-suffix=` for a fresh ID.
Never use the same suffix for two running instances of this type.

## Develop and release

This repository follows [commander-example-agent](https://github.com/defos-ai/commander-example-agent).
The agent type lives in [agent.go](agent.go), rendering actions in [actions/](actions/),
and the demo mission in [missions/review-page.json](missions/review-page.json).
Implementation and integration-test guidance are in [AGENTS.md](AGENTS.md).

```sh
go test -race -count=1 -timeout=3m ./... ./.github/tests
go vet ./... ./.github/scripts ./.github/tests
```

Run **Actions → Make release** with `v0.2.1`.
It publishes native binaries for Linux/macOS/Windows on amd64/arm64, the existing
`ghcr.io/defos-ai/commander-browser-agent` image, and `agent.yaml` containing binary
checksums, the image digest and static responsibilities/action descriptions from
[description.go](description.go), using the framework's manifest template. The agent
depends on published Commander modules at `v0.3.0`; agent and Commander release
versions are independent. Grant this repository Actions write access to the
existing GHCR package before releasing.
Complete releases promote Docker `stable` and GitHub `Latest` when they are the
newest version.

Agent source: [MIT](LICENSE). Commander framework and provider dependencies
remain AGPL-3.0-only. Bundled Lightpanda is AGPL-3.0-or-later; its license,
attribution and font notices are in `/usr/share/doc/lightpanda` in the image.
The image's attribution links to the pinned upstream source. Dependency license
requirements apply to combined builds.
