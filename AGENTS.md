# Working on the browser agent

## Boundaries and structure

- Follow the structure of [commander-example-agent](https://github.com/defos-ai/commander-example-agent).
  Keep README for users and implementation guidance here. Keep one root
  `.gitignore`, companion AGENTS files for every README, and credentials/runtime
  output untracked. Preserve uncommitted work; do not publish without authorization.
- The module imports Commander framework and providers only. Do not import
  Commander apps or add a controller inside the agent. Agent source is
  [MIT](LICENSE); Commander framework and provider dependencies remain
  AGPL-3.0-only. Preserve third-party license notices and the README distinction
  between source and dependency licenses for combined builds.
- [agent.go](agent.go) declares the literal `AgentType = "browser-agent"` and
  `NewAgent`. [actions.go](actions.go) creates fresh action instances. Keep an
  explicitly empty action catalog empty; discovery state belongs to one host.
- Commander → Controller → Agent: actions return results/proposals, the controller
  chooses agents and mission rules control transitions. Upload bytes through
  `ArtifactFileStore`; the controller accepts metadata. Do not claim exactly-once
  execution or confuse broker ACKs with result acceptance.
- The demo [mission](missions/review-page.json) uses only `capture-page` and
  `check-accessibility`. Creating an operation does not start tasks. Static HTTP
  checks and crawling remain in the separate web-agent repository.
  Root `objectives` is an ordered array of nonblank strings. All entries apply
  to the mission and are displayed separately; step `objective` stays singular.
  These mission definitions require Commander v0.2.0 or newer hosts.

## Host and providers

- [cli/cli.go](cli/cli.go) owns reusable startup/shutdown; `Main` owns signal
  handling. Cancel serving before closing dependencies; preserve joined transport
  and persistence errors on shutdown. [cmd/main.go](cmd/main.go) translates errors
  to exit status.
- [cli/config.go](cli/config.go) owns flags/environment and validates identity
  before opening providers. `--agent-id-suffix` overrides
  `COMMANDER_AGENT_ID_SUFFIX`, including an explicitly empty flag. A nonempty
  suffix must be exactly eight lowercase hex characters. Compose the full ID from
  fixed `AgentType` using framework `id.AgentIDFromSuffix`. No prefix or full ID
  input or public-configuration field is needed.
  Unset/empty generates a fresh ID. Never launch two instances with the same ID;
  the suffix provides correlation, not authorization or duplicate prevention.
- [cli/factory.go](cli/factory.go) selects FS/RabbitMQ bus and FS/S3 artifact
  providers. Unsupported schemes fail explicitly. Agents never connect to MongoDB.
  The browser needs no managed-secret decryption key.

## Rendering

- [actions/url.go](actions/url.go) validates HTTP(S) URLs and exact hostname scope.
  Ports may differ; subdomains and `www` variants are different hosts.
- Discovery in [actions/browser.go](actions/browser.go) uses the environment
  abstraction. `CHROMIUM_PATH` is absolute; explicit discovery scope wins.
  Rediscovery clears stale configuration. Missing executables/configuration make
  rendering actions unavailable with actionable requirements.
- Each execution uses a fresh browser profile and bounded context. Keep GET/HEAD
  restrictions and document navigation on the requested hostname while permitting
  external page resources. CDP listeners must not issue synchronous commands to
  their own target.
- Native sandboxing stays enabled unless `CHROMIUM_NO_SANDBOX` is explicit. The
  [Dockerfile](docker/Dockerfile) installs Chromium/fonts, sets container defaults,
  and provides a real UID1000 user with a writable home for Chromium's crash
  handler. Docker builds use standalone context and `GOWORK=off`.
- Group desktop/mobile PNGs in one artifact with descriptive metadata.
  Accessibility results are bounded checks, not a complete compliance audit.

## Demo and releases

- [compose.yaml](compose.yaml) follows the example agent's isolated FS demo:
  UID1000-owned state, one controller, loopback MCP/API/Admin ports 4283/4284/4285,
  and optional Playground using the same `COMMANDER_VERSION` (default `v0.2.0`).
  Commander host images explicitly use linux/amd64; the agent image supports
  linux/amd64 and linux/arm64. Never run demo Playground with `--all-in-one`.
- `mcp-credentials` is the demo's sole credential-file exception. Retain tokens
  across restarts, mount read-only into MCP/Playground, export before exec, preserve
  configured overrides and never log secrets. API/MCP stay at PID1 for lookup.
- The demo fixes API `ENV` to `development`.
  `.github/composes/compose.test.yaml` enables test-only API cleanup and adds a
  static HTTP fixture at `http://fixture/`, from
  [tests/fixtures/site](tests/fixtures/site/). Keep fixtures and test-only services
  in that override, not the demo. No public target/model is needed.
- `.github/scripts/release.go` reads the literal `AgentType` from `agent.go` to
  derive names. Keep the existing GHCR package `defos-ai/commander-browser-agent`.
  Grant the new repository write access before publishing. Do not let the old
  monorepo workflow keep publishing the same version/stable tags.
- The next standalone release is `v0.2.1`. Published Commander framework/providers
  are pinned to `v0.2.0`, with no local `replace`. Agent release versions are
  independent of Commander versions. An ignored development workspace may map
  dependencies to local checkouts.
- Manifest generation uses `framework/agent/manifest.Render` and its embedded
  template. Do not add a repository-local template or a `--template` release flag.
- Make release chains tests → six native builds → Docker image → digest-pinned
  `agent.yaml` and release assets from one tested revision. Keep build/push
  workflows reusable, serialize releases, preserve existing tags/assets and retry
  failed jobs in the same run. After complete publication, promote Docker `stable`
  and GitHub `Latest` without moving backward to an older version. The manifest
  describes delivery assets and static responsibilities/action descriptions, never
  host commands, networks, privileges or secrets.

## Static package description

- [description.go](description.go) provides pure `Responsibilities()` and
  `ActionDescriptions()` functions returning fresh slices. Runtime defaults and
  release manifests use the same metadata; constructors share description
  constants from `actions/`. Update both the registered and described action lists
  when capabilities change. Do not create agents/providers, run discovery, read
  environment variables or perform I/O in these functions.
- Manifest generation imports this module using the path in `go.mod` and calls
  those functions through a temporary exporter. Keep its output bounded and its
  Go environment free of release/model tokens. The four-field `metadata` command
  remains independent of these functions. Descriptions are capability metadata,
  never action readiness or permission to launch a package.

## Validation

Keep tests/fixtures under `tests/`, with external packages testing public action
and host APIs. Prefer API scenarios for registration, real rendering and artifact
acceptance; retain focused discovery/identity failure tests. Do not add production
configuration or exported injection solely for tests.

```sh
go test -race -count=1 -timeout=3m ./... ./.github/tests
go vet ./... ./.github/scripts ./.github/tests
```

The API integration test invokes global test cleanup. Use only a dedicated stack;
Docker lifecycle belongs in setup/CI, outside Go tests. Ports 4283/4284/4285 must be free:

```sh
export COMPOSE_PROJECT_NAME=commander-browser-test API_USERNAME=commander
export API_PASSWORD="$(openssl rand -hex 32)"
docker compose -f compose.yaml -f .github/composes/compose.test.yaml up --build --wait --wait-timeout 120
docker compose -f compose.yaml -f .github/composes/compose.test.yaml run --rm --no-deps -T playground --help
COMMANDER_API_URL=http://127.0.0.1:4284 go test -tags=integration -race -count=1 -timeout=3m -run '^TestAgentThroughAPI$' -v ./tests/...
docker compose -f compose.yaml -f .github/composes/compose.test.yaml down --volumes --remove-orphans
unset API_PASSWORD API_USERNAME COMPOSE_PROJECT_NAME
```

Always stop the test stack on failure. The test waits for registration, executes
both rendering actions, downloads and decodes both PNGs, checks dimensions and
hashes, completes the operation/report, then settles work and cleans state.
