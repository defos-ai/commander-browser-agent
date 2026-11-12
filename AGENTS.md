# Working on the browser agent

## Boundaries and structure

- Follow the structure of [commander-example-agent](https://github.com/defos-ai/commander-example-agent).
  Keep README for users and implementation guidance here. Keep one root
  `.gitignore`, companion AGENTS files for every README, and credentials/runtime
  output untracked. Preserve uncommitted work; do not publish without authorization.
- Keep the root README brief and written for people: capabilities, Commander
  connection links, a minimal local start, runtime prerequisites and licensing.
  Put implementation contracts, detailed configuration, operational commands,
  limitations, testing and release instructions here instead of expanding README.
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
- The default action catalog exposes `browse`, `login` and `screenshot`, with
  model-setup and in-browser non-secret clarification questions, and execution
  checkpoints for browsing and login. Do not restore
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
- For direct development, run
  `BUS_URL=/shared/bus STORAGE_URL=/shared/storage go run ./cmd` against the
  controller's bus and artifact storage. FS providers require the same shared
  directories in both processes. Use `go run ./cmd --help` for host settings.

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
- Follow the two-stage Go build/Debian runtime Dockerfile pattern. Download
  Lightpanda with the build image's existing tools. Runtime workdir is
  `/home/commander`, with private mode-0700 `state/`; CLI owns bus/storage defaults,
  while Compose supplies its shared `/state` mounts and environment overrides.
- Keep the upstream AGPL-3.0-or-later license, embedded-font notices and runtime
  attribution in [third_party/lightpanda/](third_party/lightpanda/). Docker copies
  these files to `/usr/share/doc/lightpanda` without fetching them during builds.
  When updating Lightpanda, fetch and verify `LICENSE` and
  `src/rust/render/fonts/LICENSE` from the pinned source commit, and update the
  version and source links in `NOTICE` together with the binary checksums.
  This separate runtime dependency does not change the agent's MIT or Commander's
  AGPL-3.0-only licensing.
- The screenshot action uses Lightpanda text-only PNGs. Identify the
  renderer in output/artifact metadata; desktop/mobile refer to viewport sizes,
  not pixel-accurate page design or full mobile-device emulation. Do not restore
  the old capture/accessibility actions to supply this capability.
- The image also stages unmodified Hermes v0.23.1 at commit
  `3b421578588243f920886e6fe79a6faa87ea026e` under `/opt/hermes`, with CPython
  3.14.10 and the frozen core lockfile (browser-harness 0.1.13). This packaging
  does not activate a Hermes action backend. Keep model configuration and keys
  out of image settings. No runtime dependency downloads or lazy installs.
  [docker/fetch-hermes.sh](docker/fetch-hermes.sh) pins the archive and both uv
  binaries; preserve the license and attribution in [third_party/hermes/](third_party/hermes/).
  The offline readiness command is `/opt/hermes/.venv/bin/python -I
  /usr/local/lib/commander-browser/hermes/check.py`. It checks the packaged
  identity and imports using a temporary private home, without starting an
  agent, browser or model request. Native Go release binaries do not embed this
  Python runtime; discovery and host integration are separate migration work.
- Local validation must stay within a combined 3 CPUs and 3,000,000,000 bytes
  of RAM, with swap disabled. Use a resource-limited Docker-container Buildx
  builder and stop it before running a container with the full allowance.

## Reusable browser sessions

- [agent.go](agent.go) supplies one [session manager](browser/manager.go)
  per agent unless the caller provides a Commander sessions provider. Constructors
  and `Prepare` open no browsers. `Run` requires the exact framework-owned session
  binding and creation claim from `action.SessionFromContext`.
- `browse` and `login` declare `session_mode: open` and use
  `action.OpenSession(ctx)`; `screenshot` declares `use` and uses
  `action.UseSession(ctx)`. Business inputs contain no `session_scope` or
  `session_id`; old routing fields are rejected. Commander allocates a new root
  open session from its accepted command/task identity, with no mission session
  policy or pre-existing runtime session required.
- A direct session-aware parent continuation inherits that exact session and
  original creation claim. Child `open` actions reuse it; child `use` actions also
  require a connected resource. A stateless parent breaks this chain. Independent
  root opens create independent sessions; never infer reuse from the latest or
  idle session. Trusted Go callers may supply `ActionCall.Session` through
  Commander's dispatch preparation for explicit standalone reuse. Public operation
  assignment has no session selector in business input. Removed mission
  `policies.session` must not be restored.
- Creation claims use flat `CommandID`, `OperationID` and `TaskID`. Command ID is
  required; operation/task IDs are an optional pair and may differ from the
  creation command. Retain and compare the full original claim. Controls use
  `HasCreation()`/`Claim()` and close requires the creation identity and request ID.
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
- [browser/hermes.go](browser/hermes.go) binds a private Hermes worker to that same
  manager and exact creation claim. Its trusted absolute runtime paths are host
  configuration, never action inputs. A claim cannot switch between the Go and
  Hermes backends or change its pinned paths. `RunHermes` lends a callback-scoped
  bridge through [internal/hermes/session.go](internal/hermes/session.go); retained
  or detached contexts cannot start later work. This infrastructure does not
  change the default action catalog or move an existing Go session to Hermes.
- A ready Hermes worker remains `starting` until native automation has opened and
  verified its browser. Pin the owned Lightpanda process, harness daemon, retained
  target and original connection; health controls must not create or reconnect
  them. `use` requires a verified connection. Idle health checks keep a retained
  session alive; failed identity checks retire it as `lost`, with no replacement
  under its old claim. Target changes and unavailable execution contexts also
  conservatively lose the session, even if its processes survive. Native Python
  may still perform recovery within a batch;
  these checks do not promise universal prevention of internal retries.
- Action cancellation preserves a healthy Hermes browser only after a bounded
  receipt confirms that the model request and native work have stopped. The
  pinned OpenAI nonstreaming request path needs read-only observation of its exact
  Python thread frames to join an SDK worker that can outlive Hermes interruption.
  This is a version-specific internal check, not a public general-purpose Hermes
  cancellation API. An unfamiliar path, incomplete join or missing receipt loses
  the resource rather than allowing reuse. Native callbacks carry their original
  execution token; abandoned callbacks cannot adopt a later run. Every receipt,
  including failure paths, checks for pending native work and questions. Close
  also joins owned descendants and retains uncertain cleanup state.
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

- The approved migration uses Hermes's native Python browser tools for reasoning
  and automation, with a thin [private host bridge](internal/hermes/). This
  deliberately broadens the old typed-command policy: Python has filesystem,
  network and raw CDP capabilities. The worker is not an OS sandbox. Dedicated
  homes and exact tool routing do not isolate hostile Python from other resources
  accessible to the same OS user; deployment isolation remains a cutover gate.
  Do not add a second reasoning loop, fork upstream, parse terminal traces, or
  restore the discarded evaluation files.
- Keep the current Go actions active until the new integration passes its
  Commander-bound session, managed-secret, screenshot and checkpoint workflows.
  The private bridge is migration infrastructure, not an additional action or an
  activated fallback. Use public plugin overrides with explicit grants; refuse
  startup if required wrappers or the advertised catalog do not match. Admission
  checks belong inside the wrapper handler before native dispatch, since
  middleware exceptions can fail open. A native Python call is a batch: its
  checkpoint cannot confirm each operation or prevent every internal retry.
- Private worker messages use bounded, correlated frames over inherited pipes;
  raw stdout/stderr and returned Hermes transcripts never become protocol data.
  Questions and results must be bounded and redact known values. Private
  observation-protection messages carry bounded native JSON only to the Go
  redactor; never forward them to action handlers, logs, checkpoints or artifacts.
  Protect native results before returning them to Hermes's history. Native browser children
  may escape the worker process group; uncertain forced cleanup must retain lost
  ownership and private state, never report a confirmed close or restoration.
- [internal/hermes/model.go](internal/hermes/model.go) mediates model HTTP without
  implementing reasoning. The exact selected completion endpoint and real key
  stay in Go. Hermes receives a random credential for an execution-scoped,
  loopback-only OpenAI-compatible endpoint. Forward only bounded nonstreaming
  requests for the exact model, to the fixed selected endpoint, with the real key
  only in its authentication header. Serve SDK metadata locally; never forward
  metadata probes, SDK headers, redirects, provider errors or retries.
  Canonicalize and protect incoming provider JSON before Hermes sees it, including
  nested JSON arguments, IDs and clarification fields. Protect outgoing requests
  and native observations with the same session fingerprints. Fail closed on
  malformed/oversized data, key collisions or protected numeric values. This
  covers exact values and JSON escaping, not arbitrary website transformations.
  Public Hermes middleware isolates failures and can fail open; it is not this
  credential boundary. Do not patch Hermes, add a second reasoning loop, or
  substitute terminal-output parsing for this transport.
- Worker close owns model-transport startup and shutdown independently of the
  execution gate. Join HTTP work before clearing owned key/token bytes and before
  confirming session closure. Keep worker fingerprints as well as manager-owned
  protection so changing a trusted caller's context cannot forget earlier keys.
- [browser/intelligence/](browser/intelligence/) implements an in-process Go loop
  on the existing session context. Do not launch the native Lightpanda agent or
  attach another CDP connection for automation. Screenshot rendering has the
  isolated, unauthenticated connection described below. The pinned native agent has a fixed tool catalog,
  creates its own browser/session and exposes environment values through `getEnv`;
  a fake-model probe confirmed that path reaches model context. Its JSONL trace is
  observation-only, not a custom callback bridge. Browser Use 0.13.14 also creates
  its own connection and auto-reconnects. Neither preserves this session contract
  through a suitable public hook. Relevant pinned sources:
  [Lightpanda Agent](https://github.com/lightpanda-io/browser/blob/c5f7bb29b127040c899d4e181b5709f76fc71a6f/src/agent/Agent.zig),
  [ToolSession](https://github.com/lightpanda-io/browser/blob/c5f7bb29b127040c899d4e181b5709f76fc71a6f/src/ToolSession.zig),
  [Browser Use session](https://github.com/browser-use/browser-use/blob/2cc71890e65056b67fdb29d98bb407749c15bb08/browser_use/browser/session.py).
- Model setup follows provider → exact model ID/version → provider key.
  [internal/browsing/setup.go](internal/browsing/setup.go) uses `action.Question` for
  non-secret choices, with stable keys and at most four options. Commander owns
  operator-first waiting and model eligibility; never have the unconfigured
  browser model answer its own setup. Custom endpoints are a further non-secret
  question, never a secret request. Do not fetch a live model catalog before keys.
- [internal/browsing/hermes.go](internal/browsing/hermes.go) uses the same setup
  cache and creation claims. Its staged native path offers OpenAI and explicitly
  selected Other only; reject unverified profiles before requesting credentials.
  Keep the exact selected endpoint for both fixed transport routing and custom
  secret naming. Reuse resolves managed credentials again and honors Commander's
  delivery lease; it does not force a sync or bypass the managed-secret provider.
  The current public Go actions retain their existing provider catalog.
- Model configuration and API keys must not come from the agent's environment.
  New sessions obtain provider/model choices through questions; Other also asks
  for its exact endpoint. Do not retain removed model-environment shortcuts or
  aliases. Discovery uses the environment abstraction only for Lightpanda,
  clears stale state, and never makes model network calls, asks questions or
  requests secrets. Require HTTPS except local HTTP; reject embedded credentials,
  URL query/fragment and redirects.
- [Provider presets](browser/intelligence/provider.go) define fixed endpoints and
  provider-specific managed-secret names matching Playground. Do not import
  Commander apps. Always use `action.RequestReusableSecret` for the selected provider's
  canonical secret name. Other uses only the endpoint-bound managed name from
  the setup helper. Never send a standard provider's key to a custom endpoint or
  reuse an unscoped generic key after an endpoint change. Provider wire profiles
  must follow their primary API references; compatibility is not established by
  copying provider names alone. Keep fake-provider regressions for each profile.
- Keep valid non-secret choices in memory per full Commander creation claim;
  serialize setup per identity and reject conflicting claims.
  Never cache plaintext keys in setup or discovery state. Session reuse skips
  questions and resolves credentials again. Setup is bounded to ten minutes,
  separately from active browsing, and cannot extend the outer task deadline.
  Commander v0.4.1 supports reusable `SecretRequestOptions.Default` bytes as
  create-if-missing seeds; existing values, including expired ones, take
  precedence. It has no lookup-only or lazy-default action API. Keep first-use
  model questions until Commander can report an authoritative missing setting
  before asking them; a timeout does not establish absence. Shared model setup
  across agent instances needs that hook before question answers can seed
  reusable settings without repeating questions. Do not seed placeholder values,
  race questions against secret delivery, or add direct secret-store access.
- Enable framework managed secrets in the host. Request missing keys on demand;
  RequiredSecrets must stay empty so setup can run first. Clear owned key bytes
  after closing the model client.
  Managed browser sessions must not inherit provider keys. Send the key only in the
  model authentication header and redact known values from observations, output
  and artifacts. Suppress raw provider/browser errors and protocol output.
- The current Go loop exposes typed browser commands only, with one model command per turn and no
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
  deadline causes. In Commander v0.4.1, canceling a waiter does not retract a saved
  pending question; do not claim automatic question cleanup or action restoration.
- Keep non-action setup and active-time budget helpers under
  [internal/browsing/](internal/browsing/). The action owns one setup instance and
  delegates model/session selection to it; do not expose these helpers as actions
  or public package APIs. Preserve setup serialization and exact creation claims.

## Managed login

- [browser/login/hermes.go](browser/login/hermes.go) stages the same deterministic
  login executor on the retained Hermes session without model setup or an agent
  invocation. [internal/hermes/browser.go](internal/hermes/browser.go) grants
  callback-scoped, serialized host access to the original harness connection
  through its CDP helpers. Pin the original automation attachment using the
  version-specific daemon `connection_status` IPC metadata; the auxiliary
  attachment remains for health checks. Lightpanda emits network events on the
  original attachment. Reject a changed daemon route instead of adopting it.
  Do not open another automation WebSocket,
  record credential operations as native agent tools, or patch upstream. This
  remains private migration infrastructure; the default actions still use Go.
- The private native lease pins its original attachment, bounds RPCs and drains
  filtered events concurrently with intercepted requests. Only request routing,
  resource/frame identity, response status and completion/failure IDs cross this
  event boundary; never return headers, cookies or POST data. Reject a total
  unfiltered daemon batch of 128 events before filtering because the pinned
  daemon silently evicts at 500. An unconfirmed destructive drain or incomplete
  join loses the resource. End the lease only after stopping loading, removing
  interception and joining the event pump and request workers.
- [actions/login.go](actions/login.go) uses the host's session runner and the
  deterministic [browser/login/](browser/login/) executor; login never needs model
  setup or gives credentials or one-time codes to a model. Require an exact-origin
  `verification_url` and bounded non-secret `expected_text`. `verified` means that
  configured check matched, including existing state without new submission; do
  not infer universal authentication/account identity from HTTP 200 or a cookie.
- Bind a lowercase non-secret credential profile to the canonical login origin
  (scheme/hostname/effective port). HTTPS is required except loopback HTTP.
  Derive stable, purpose-specific username/password names from origin and profile;
  never accept arbitrary secret names from the model or action input. These names
  are a convention in Commander's shared namespace, not an access-control policy.
  Use v0.4.1 `action.RequestReusableSecret`, with empty RequiredSecrets, bounded
  waits and cancellation. Derive the transient code request's `-otp` name from
  that same origin/profile convention; Commander binds each request to its own
  execution and delivery, so a later request cannot reuse an earlier code.
- A supported one-time-code form has one visible editable text/tel/number input
  explicitly marked as a code, one native submit control and a same-origin POST
  destination. Reject extra editable fields, visible passwords, multiple forms,
  submit overrides, frames, shadow forms and CAPTCHA. Unsupported challenges
  remain `requires_challenge`, including push/security-key flows and split inputs.
  Inspect a retained same-origin challenge before verification navigation and
  handle a password response's challenge before navigating away. A visible
  challenge always takes precedence over a matching verification marker.
- Request at most one code with `action.RequestTransientSecret`, without a
  default, after inspecting the form. Pause active work time during the bounded
  wait while preserving session/action/task cancellation and deadlines. A
  request-specific expiry or withdrawal with a live caller returns
  `requires_challenge`; it does not restart the request. Register the bounded code
  for session redaction before filling and clear owned code and credential bytes
  promptly. Revalidate the saved document URL, form/control identities and form
  destination/configuration before entry and again after input/change events.
  Submit a code at most once per execution. A rejected or still-visible code form
  returns `requires_challenge`; uncertain submission returns `submission_uncertain`.
  Explicit continuation uses a new execution and fresh transient delivery.
- Support only unambiguous visible main-frame username/password POST forms with
  native submit controls. Validate form/action/method/target overrides before
  requesting values and again before filling. Guard all requests to the exact
  origin while handling credentials/codes, issue at most one password submission
  and one code submission command, and never automatically retry uncertain
  effects. Require a positive main-frame POST response or redirect receipt before
  navigating away for verification; missing response evidence stays uncertain.
  This receipt confirms a response, not authentication or exactly-once delivery.
  Native navigation must confirm its returned frame/loader before accepting
  document readiness. Lightpanda 1.0.2 can internally
  repeat a lost-response POST below CDP interception; test one owned CDP request
  and an uncertain outcome, not exactly-once website delivery. Stop loading before
  removing interception. Preserve the retained connection and independent session
  controls.
- The operator chose normal cross-site browsing after login. Do not add a
  persistent origin lock or imply that initial request interception constrains a
  trusted website's later scripts. Never transfer authenticated state to another
  browser session. Unsupported forms/challenges and uncertain verification retain
  a session reference with an honest status, not a success claim.
- One agent can retain several authenticated sessions. Use distinct session
  scopes/IDs and credential profiles for different accounts on the same origin;
  each session keeps its own cookies. A profile selects managed credential names,
  not an authenticated account or a different cookie jar. An already matching
  verification check can skip credential submission, so changing the profile
  alone does not switch the account in an existing session.
- Register submitted values with session-owned private keyed fingerprints before
  any DOM entry. Clear owned plaintext buffers on every path. Retain old
  fingerprints across rotation/failed login/code challenges/action cancellation,
  clone/isolate state by session, redact before truncating observations, and clear only after
  confirmed close. Never expose fingerprints in snapshots or store raw credential
  text for future browsing. Exact/common value redaction is bounded; do not claim
  universal protection against arbitrary website transformations.

## Protected text screenshots

- [actions/screenshot.go](actions/screenshot.go) is use-only: require the exact
  framework `use` binding with its existing session reference. Do not navigate, create
  an automation session, ask model questions or request credentials for capture.
  Keep the host's production runner and preserve explicit empty action catalogs.
- [browser/screenshot/](browser/screenshot/) reads the retained main page using a
  fixed isolated-world function. Bound title/body extraction, exclude hidden and
  form values, and apply the shared session redactor before excerpt truncation
  and before passing text to rendering. Never capture raw authenticated DOM and
  attempt to redact the PNG afterward; do not mutate/restore the live document.
  Retain model-key fingerprints in the managed session before browsing so later
  screenshots also protect those values without asking for the key again. Missing
  tracking on a custom runner does not imply that it can render screenshots.
- Lightpanda 1.0.2 permits one target/context per CDP connection. A temporary
  renderer connection to the same owned process receives only protected plain
  text in an inert blank document. Never copy cookies, storage, authentication,
  source HTML or source scripts. Keep its endpoint and runtime handle private.
  Renderer-local metrics must not change the source page's viewport/media state.
- [browser/render.go](browser/render.go) owns temporary renderer connections and
  joins target/allocator/socket cleanup, including on action cancellation and
  session close. Bind the private renderer handle to the original `Manager.Run`
  lifetime; detached contexts cannot render after that callback ends. Source
  shutdown stops admission and waits for render borrows.
  Return PNG bytes only after confirmed cleanup; uncertain cleanup retains
  ownership and marks the source runtime lost rather than silently reconnecting.
- Use native PNG output, validate its byte/pixel bounds and actual dimensions,
  and upload through ArtifactFileStore via action results. Renderer metadata
  belongs in output and `screenshot.json` beside `screenshot.png`; never include
  source text, URLs, DOM or secrets in the sidecar. `desktop`/`mobile` are text
  viewports (1280x720/390x844), not responsive-design/device emulation. The source
  excerpt truncation flag does not promise that all text fits in the image.

## Browsing checkpoints

- [internal/browsing/checkpoint.go](internal/browsing/checkpoint.go) uses the
  execution-scoped `action.LoadCheckpoint`/`SaveCheckpoint` exchange.
  The production host supplies it. Direct public-action calls without that
  provider remain usable but offer no durable checkpoints; actual provider
  failures must stop work rather than silently disabling persistence.
- Load the action's `browse_started` or `login_started` fence before setup
  questions inside the session lifetime. An existing fence refuses automatic
  replay before questions, secrets or browser work. Checkpoints belong to one
  execution, not to a transferable browser snapshot or a session-global history.
- Persist `browse_started` before initial page navigation and `login_started`
  before deterministic login, covering both password and code work. Each
  `browse_step_01` through `_30` is saved after
  model command validation and before executing that command, including question
  publication. The fact records the pending command type, current reserved usage
  and preceding completed steps; it does not prove whether the pending effect ran.
  One start, thirty command fences and one final fact fit the framework's limit
  of thirty-two immutable names. Never overwrite a named fact with new content.
- Persist only version/action/engine, exact session reference and original
  creation claim, configured limits, progress counters, fixed step outcomes and
  evidence IDs with SHA256 of already-redacted canonical evidence JSON. Do not
  persist objective text, URLs, DOM, full model commands/transcripts, question text or
  answers, provider settings, cookies, credential names/values or artifact bytes.
  Findings and evidence content remain in the final results and artifacts.
  Fact version 2 records Commander's flat creation claim. A saved start fence of
  any version still blocks replay; old facts are never rewritten or restored.
- Every checkpoint wait is bounded to five seconds and preserves the execution
  and session lifetime. The initial load belongs to setup; writes count against
  active work time. A save error or cancellation may follow a committed write;
  stop before further browser
  commands and never treat a missing receipt as permission to repeat an effect.
  Pending/failed facts require session inspection before explicit continuation.
- Save `browse_finished` or `login_finished` only after browser work/cleanup and
  output validation, while the execution and session remain active. Phase
  `work_prepared` describes prepared work, while `work_unconfirmed` records a
  failed or uncertain operation. Neither confirms artifact upload or controller
  result acceptance: `result_delivery` remains `unconfirmed` at that point.
  Failure to confirm final persistence suppresses output/artifacts and retains
  the session reference. Cancellation can leave only the last pending fence.
- Recovery is explicit: inspect the recorded session, then create a new execution
  using its exact live binding and fresh observations. Do not restore old model
  turns, questions, targets or execution allowances from checkpoints. Lost owner
  or browser state requires a new session and, when needed, a fresh login. The
  framework already prevents automatic reruns of uncertain command deliveries;
  checkpoints do not add an exactly-once guarantee.

## Demo and releases

- [compose.yaml](compose.yaml) follows the example agent's isolated FS demo:
  UID1000-owned state, one controller, loopback MCP/API/Admin ports 4283/4284/4285,
  and optional Playground using the same `COMMANDER_VERSION` (default `v0.4.1`).
  Commander host images explicitly use linux/amd64; the agent image supports
  linux/amd64 and linux/arm64. Never run demo Playground with `--all-in-one`.
- Start the local demo with `docker compose up --build --wait`, then
  `docker compose run --rm --no-deps playground`. Browser Admin is at
  `http://localhost:4285`; add API `http://localhost:4284` and obtain its login with
  `docker compose exec api commander-api credentials`. Enter model keys through
  Playground's masked setup. Use `/exit` to leave Playground and
  `docker compose down` to stop the stack.
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
  are pinned to `v0.4.1`, with no local `replace`. Agent release versions are
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
Keep the suite lean: prioritize agent behavior and meaningful regressions over
repeated schema/configuration permutations or tests of framework internals.
Shared HTTP pages belong in `tests/fixtures/`; the fake HTTP model belongs in
`tests/fakes/`.

```sh
go test -race -count=1 -timeout=3m ./... ./.github/tests
go vet ./... ./.github/scripts ./.github/tests
```

Real session/browse tests use `-tags=browserintegration` and require absolute
`LIGHTPANDA_PATH`. They exercise the production agent's actions with Lightpanda
through Commander dispatch, inventory and session controls over the FS bus, without an
external model or demo stack. A local fake model uses the actual HTTP protocol;
the harness answers setup/clarification questions and delivers synthetic managed
secrets. Retain the main login → clarification → interact/extract → screenshot →
reuse → close workflow, two-account isolation, checkpoint receipt retries and
navigation uncertainty, challenge/credential-wait cancellation, execution budgets
and grounded completion. Screenshot checks compare decoded pixels, source state
and renderer cleanup. Focused tests cover catalog/ownership, managed setup/reuse,
saved-checkpoint rejection and each supported provider's wire/secret boundaries.
Session regressions cover routing-free root allocation, exact direct-parent
continuation binding, standalone flat claims, fresh browse cookie continuity,
and rejection of unbound/foreign/closed dispatches before browser or model work.
OTP workflows cover transient delivery, password-to-code and retained-page
continuation, expiry/rejection without automatic retry, stale-response isolation,
form mutation, uncertain submission, pending close and subsequent model/screenshot
redaction.
The production manager owns browser lifecycle; fixtures own HTTP pages only.
CI compiles the race-enabled test binary in the Go bookworm image to match the
runtime's glibc, then executes it as the runtime image's UID1000 with no external
network. Use `--init` for this test container to reap browser descendants.
The `hermesintegration` tag adds permanent private-bridge scenarios using the
packaged Python/Hermes paths and a fake HTTP model: native navigation, host
admission, clarification, cookie reuse across runs, cancellation and confirmed
close. Commander dispatch scenarios also cover root allocation, direct-parent
reuse, independent cookie jars, exact-claim rejection, private callback lifetime,
inventory controls, clean cancellation with reuse and lost-target refusal.
Native managed-login scenarios also cover password/OTP delivery without model
setup, authenticated reuse by native reasoning, independent accounts on one
origin, stale/expired transient deliveries, form mutation, dropped responses,
pending-session close, and event overflow before filtering. Login values must
remain absent from model requests and public Commander messages.
CI combines both integration tags with a six-minute test-binary bound.
Clean-close Hermes scenarios check that their private profiles are removed;
intentional uncertain-loss scenarios require retained lost state and profiles.
The container must have no surviving Python or Lightpanda processes before it
is discarded, including after those uncertain cases.
The bridge currently supports the OpenAI Chat Completions wire profile for
OpenAI and explicitly selected Other. Tests include escaped provider echoes,
native observations, key rotation, exact reusable-secret requests, private worker
files/environments and redirect refusal. Other provider profiles and Commander
action activation remain separate migration work.

Future API workflow tests that invoke global cleanup must use a dedicated stack,
with Docker lifecycle owned by setup/CI outside Go tests. Keep fixtures under
`tests/` and always stop that stack on failure.
