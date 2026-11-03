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
- The default action catalog exposes `browse`, with model-setup and in-browser
  non-secret clarification questions. Managed login, checkpoints and text-only
  screenshots are subsequent steps. Do not restore
  legacy capture/accessibility actions or their review mission. Static HTTP
  checks/crawling remain in the separate
  web-agent repository. Creating an operation does not start tasks.
  Root `objectives` is an ordered array of nonblank strings; entries apply to the
  mission and display separately. Mission steps are not supported. Future demo
  missions must import through Admin CLI after API readiness; the controller never
  loads mission files.

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

## Browser runtime

- [Lightpanda discovery](browser/lightpanda/) uses the environment abstraction.
  `LIGHTPANDA_PATH` is absolute; explicit discovery scope wins. Return fresh
  configuration, check the exact 1.0.2 version with a bounded context, and report
  structured readiness requirements without echoing subprocess output. Never
  start a CDP server during discovery.
- [browser/](browser/) owns Lightpanda process/connection lifecycle. Do not add
  Chromium as a fallback. The Dockerfile packages checksum-pinned linux/amd64 and
  linux/arm64 binaries, fails unsupported architectures, and disables telemetry
  and core dumps. Keep standalone context and `GOWORK=off`, a real UID1000 user
  and writable home.
- Fetch and verify the upstream AGPL-3.0-or-later license and embedded-font notices
  from the pinned source commit during the build. Retain them with runtime
  attribution in `/usr/share/doc/lightpanda`; do not vendor a third_party folder.
  This separate runtime dependency does not change the agent's MIT or Commander's
  AGPL-3.0-only licensing.
- The future screenshot action uses Lightpanda text-only PNGs. Identify the
  renderer in output/artifact metadata; desktop/mobile refer to viewport sizes,
  not pixel-accurate page design or full mobile-device emulation. Do not restore
  the old capture/accessibility actions to supply this capability.

## Reusable browser sessions

- [agent.go](agent.go) supplies one [session manager](browser/manager.go)
  per agent unless the caller provides a Commander sessions provider. Constructors
  and `Prepare` open no browsers. `Run` requires the exact framework-owned session
  binding and creation claim from `action.SessionFromContext`.
- Actions select `lightpanda` internally; do not require an engine input when only
  one engine is supported. The session runtime still rejects other engines. Keep
  its process, CDP connection and page for reuse. Every session has its own private
  profile; never transfer cookies or authentication between sessions or engines.
  Never reconnect a lost session and describe it as restored.
- Browser lifetime is independent of an action's cancellation after startup.
  Preserve execution context values for questions, secrets, logs and checkpoints.
  Serialize work within a session while allowing independent inspect/verify/close
  controls. Close must interrupt work and join startup/cleanup before reporting
  `closed`; uncertain cleanup and owner shutdown retain `lost` references.
- Retain closed tombstones and original creation identities for the host lifetime.
  Scope admission and release belong to the controller. Accept a new
  framework-issued creation claim independently of an old identity in the same
  scope: abandonment can authorize replacement before best-effort old cleanup.
  Never rebind or transfer the old resource. Snapshots are quick, independently
  cloned and revisioned, with coalesced change notifications. Expose engine/state/
  reference metadata, never CDP endpoints, profile paths, page URLs, cookies,
  credentials or browser output.
- Managed processes use loopback CDP, discard browser/protocol logs, and disable
  Lightpanda telemetry and core dumps. Bound startup and cleanup, terminate/reap
  owned processes, and remove private profiles. No browser process is started
  during discovery.
  Managed sessions require Unix process-group support; native Windows must report
  the Docker/WSL requirement before starting a process, until job-based process-tree
  ownership is implemented.

## Intelligence and model access

- [browser/intelligence/](browser/intelligence/) implements an in-process Go loop
  on the existing session context. Do not launch the native Lightpanda agent or
  attach another CDP connection. The pinned native agent has a fixed tool catalog,
  creates its own browser/session and exposes environment values through `getEnv`;
  a fake-model probe confirmed that path reaches model context. Its JSONL trace is
  observation-only, not a custom callback bridge. Browser Use 0.13.14 also creates
  its own connection and auto-reconnects. Neither preserves this session contract
  through a suitable public hook. Relevant pinned sources:
  [Lightpanda Agent](https://github.com/lightpanda-io/browser/blob/c5f7bb29b127040c899d4e181b5709f76fc71a6f/src/agent/Agent.zig),
  [ToolSession](https://github.com/lightpanda-io/browser/blob/c5f7bb29b127040c899d4e181b5709f76fc71a6f/src/ToolSession.zig),
  [Browser Use session](https://github.com/browser-use/browser-use/blob/2cc71890e65056b67fdb29d98bb407749c15bb08/browser_use/browser/session.py).
- Model setup follows provider → exact model ID/version → provider key.
  [actions/setup.go](actions/setup.go) uses `action.Question` for
  non-secret choices, with stable keys and at most four options. Commander owns
  operator-first waiting and model eligibility; never have the unconfigured
  browser model answer its own setup. Custom endpoints are a further non-secret
  question, never a secret request. Do not fetch a live model catalog before keys.
- Model configuration and API keys must not come from the agent's environment.
  New sessions obtain provider/model choices through questions; Other also asks
  for its exact endpoint. Do not retain removed model-environment shortcuts or
  aliases. Discovery uses the environment abstraction only for Lightpanda,
  clears stale state, and never makes model network calls, asks questions or
  requests secrets. Require HTTPS except local HTTP; reject embedded credentials,
  URL query/fragment and redirects.
- [Provider presets](browser/intelligence/provider.go) define fixed endpoints and
  provider-specific managed-secret names matching Playground. Do not import
  Commander apps. Always use `action.RequestSecret` for the selected provider's
  canonical secret name. Other uses only the endpoint-bound managed name from
  `ModelSecretName`. Never send a standard provider's key to a custom endpoint or
  reuse an unscoped generic key after an endpoint change. Provider wire profiles
  must follow their primary API references; compatibility is not established by
  copying provider names alone. Keep fake-provider regressions for each profile.
- Keep valid non-secret choices in memory per full Commander creation claim;
  serialize setup per identity and reject conflicting claims.
  Never cache plaintext keys in setup or discovery state. Session reuse skips
  questions and resolves credentials again. Setup is bounded to ten minutes,
  separately from active browsing, and cannot extend the outer task deadline.
  Stay on Commander v0.3.0: do not invent v0.4.0 RequestSecret default arguments.
- Enable framework managed secrets in the host. Request missing keys on demand;
  RequiredSecrets must stay empty so setup can run first. Clear owned key bytes
  after closing the model client.
  Managed browser sessions must not inherit provider keys. Send the key only in the
  model authentication header and redact known values from observations, output
  and artifacts. Suppress raw provider/browser errors and protocol output.
- Expose typed browser commands only, with one model command per turn and no
  model-supplied JavaScript, selectors, credentials or raw CDP. Keep safe bounded
  observations, evidence references and static execution logs. Document navigation
  allows all HTTP(S) hostnames by default: omitted/empty `allowed_hosts` normalizes
  to `["*"]`. Explicit hostname lists restrict documents to those exact hosts and
  must include the starting host. The wildcard must stand alone; partial wildcards
  are unsupported. Keep URL credential/scheme checks in both modes.
  Reject malformed/multiple commands before side effects. Stop on uncertain
  browser operations instead of automatically retrying them. Bound wall time,
  steps, completion allowances, observations, responses and evidence. Do not claim
  a provider cost/generation-token limit or transparent recovery from lost state.
  Token allowance accounting differs by provider (including reasoning/tool tokens).
- The action factory receives the same production session runner as the host.
  Preserve explicitly empty catalogs; do not create a second manager inside an
  action. Return artifact bytes through action results for ArtifactFileStore
  upload; never write raw DOM, model transcripts or credentials to artifacts.
- The typed `ask` command bridges to `action.Question` using execution-local keys
  `browser_clarification_1` through `_4`. Commander owns operator waiting, fallback
  eligibility and saved answers; do not ask the browser model to answer itself or
  retry browser operations while a question is pending. Validate bounded nonblank
  UTF-8 question/options before publication, retain at most four answers of at most
  2,000 bytes with their question/options in memory for the current run, and keep
  transcripts out of results, logs and artifacts. Answers cannot authorize scope,
  tool or budget changes, secret entry or new external side effects.
  Clarification waits pause the active browsing timer, are individually bounded to
  three minutes, and never extend the task deadline. Preserve cancellation and
  deadline causes. In Commander v0.3.0, canceling a waiter does not retract a saved
  pending question; do not claim automatic question cleanup or action restoration.

## Demo and releases

- [compose.yaml](compose.yaml) follows the example agent's isolated FS demo:
  UID1000-owned state, one controller, loopback MCP/API/Admin ports 4283/4284/4285,
  and optional Playground using the same `COMMANDER_VERSION` (default `v0.3.0`).
  Commander host images explicitly use linux/amd64; the agent image supports
  linux/amd64 and linux/arm64. Never run demo Playground with `--all-in-one`.
- Do not pass model settings or keys into the demo agent: use questions and
  managed secrets. Do not automatically pass host provider keys into Playground.
  It accepts masked key entry or one explicitly forwarded provider key and shares
  the selected key; the agent requests its managed secret.
- Separate `api-credentials` and `mcp-credentials` volumes retain the demo logins.
  Initializers validate saved values and preserve explicit overrides. API and
  Playground mount the API password read-only; MCP and Playground
  mount the token read-only. Export before exec and never log secrets. API/MCP
  stay at PID1 for credential lookup. Browser Admin signs in independently.
- The demo fixes API `ENV` to `development`. Keep any future HTTP fixtures and
  test-only API configuration in a dedicated test override, not the demo.
- `.github/scripts/release.go` reads the literal `AgentType` from `agent.go` to
  derive names. Keep the existing GHCR package `defos-ai/commander-browser-agent`.
  Grant the new repository write access before publishing. Do not let the old
  monorepo workflow keep publishing the same version/stable tags.
- The next standalone release is `v0.2.1`. Published Commander framework/providers
  are pinned to `v0.3.0`, with no local `replace`. Agent release versions are
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

Real session/browse tests use `-tags=browserintegration` and require absolute
`LIGHTPANDA_PATH`. They exercise Lightpanda and a test-only action through
Commander dispatch, inventory and session controls over the FS bus, without an
external model or demo stack. Browse tests use a local fake model over its actual
HTTP protocol, answer framework setup questions and deliver synthetic keys through
the framework secret exchange. Cover pending-question retries, saved answers,
ignored model environment variables, managed credentials, cancellation and reuse
without new questions.
The production manager owns browser lifecycle; fixtures own HTTP pages only.
CI compiles the race-enabled test binary in the Go bookworm image to match the
runtime's glibc, then executes it as the runtime image's UID1000 with no external
network. Use `--init` for this test container to reap browser descendants.

Future API workflow tests that invoke global cleanup must use a dedicated stack,
with Docker lifecycle owned by setup/CI outside Go tests. Keep fixtures under
`tests/` and always stop that stack on failure.
