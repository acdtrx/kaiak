# Tech Stack

> Decisions and the reasoning behind them. Specs reference this file for anything
> deployment- or stack-concrete. When the project deviates, edit the section in place
> and date the ruling (`(settled YYYY-MM-DD)`, see `AGENTS.md`, Documentation) — this
> file describes the project's actual stack, not a menu.
>
> This is the **only** doc that names libraries and frameworks — no spec file names a
> specific technology unless it is inherent to the domain (the OpenAI API, SSE, the
> Prometheus text format are). As dependencies accumulate, grow the dependency inventory
> at the end: every dependency, what it is for, why it was chosen.

## Repo layout

- `gateway/` — Go module. `cmd/kaiak` is the binary; subsystems are packages under
  `internal/`, a flat list (settled in `docs/ARCHITECTURE.md` as the project takes
  shape). A subsystem that needs private sub-packages nests them under its own
  `internal/`, so the compiler enforces entry-only access.
- `control/` — npm workspaces: `control/kaiak-control` (package of the same name) and
  `control/sample`.
- `protocol/` — JSON Schemas for the config document and every protocol message, plus
  shared fixtures (valid and invalid configs, usage batches, stream sequences, each
  backend type's models-list request, what a usage record counts toward each limit
  type). Both halves run the fixtures in their test suites.

## Gateway: Go (settled 2026-09-24)

- **Why Go**: the gateway is a proxy on the hot path holding many long-lived streams. Go
  gives one static binary (~18 MB image on a static distroless base), millisecond
  startup, cheap concurrency (a goroutine per stream, `context` for cancellation and
  back-pressure) and a standard library that covers HTTP server and client, SSE, JSON,
  crypto and TLS. Rejected: Node (the control plane's language — viable for an
  I/O-bound proxy, but a ~150 MB image and no path to zero dependencies with a real
  HTTP framework); Rust (more effort than the problem needs).
- **Dependencies: minimal, maintained, clearly worth what they cost** (settled
  2026-10-08). The self-contained requirement is the product: the binary stays
  usable standalone — one static binary, depending on no service but the control
  plane (and that only in control-plane mode) — and storage and management stay
  with the app built on `kaiak-control`. Within that, a third-party module may
  enter when it is small (few transitive modules), maintained (CODING-RULES §3),
  and clearly worth its cost — weighed in binary size, the modules it brings, and
  whether it keeps the gateway's own rules (no remote text in logs, the delivery
  and drop rules, malformed settings failing the start). Each one is a dated ruling
  here, asked for first, and listed in the inventory below. What the standard
  library covers in a few hundred lines stays in-repo (SSE parsing, the Prometheus
  text exposition, OTLP/HTTP JSON export). Rejected: zero third-party
  dependencies as a hard rule (the 2026-09-24 ruling) — the self-contained
  requirement is about what the binary needs to run, not about who wrote its
  code. Allowed by this rule, not decided here: the OpenTelemetry trace SDK
  core (`go.opentelemetry.io/otel`, `sdk/trace`, the TraceContext propagator)
  links four small third-party modules (`xxhash`, `logr`, `stdr`, `uuid`) and no
  gRPC or protobuf — the traces work's to take or leave, with its own ruling.
- **HTTP**: `net/http` server and client. Two listeners: the API port (client traffic)
  and the admin port (`/metrics`, `/healthz`, `/readyz`) — the admin port is never
  exposed through an ingress.
- **Config format: JSON** (settled 2026-09-24). Standard library, same encoding as the
  protocol, one JSON Schema serves the gateway, `kaiak-control` and any UI. YAML rejected: it
  needs a dependency, and hand-editing comfort is the only gain.
- **Validation**: Go has no built-in JSON Schema validator, so the gateway decodes into
  typed structs with unknown fields rejected (`DisallowUnknownFields`) and runs
  explicit validation functions after decoding. The shared fixtures in `protocol/` keep
  this in agreement with the schemas `kaiak-control` validates against.
- **Logging**: `log/slog` — JSON in production, text handler in development; one line
  per request with request ID, key ID, model, backend, status, latency, tokens;
  attribute names follow OpenTelemetry's semantic conventions where they fit
  (`docs/specs/GATEWAY.md`, Observability: Logs).
- **OTLP export: hand-written OTLP/HTTP with JSON encoding** (logs settled
  2026-10-05, metrics 2026-10-08). Logs: an `slog` handler beside the stderr one
  queues each record; a background sender posts batches as OTLP JSON
  (`resourceLogs` → `scopeLogs` → `logRecords`). Metrics: a periodic reader
  collects the registry and posts `resourceMetrics` → `scopeMetrics` → `metrics`.
  Both with `net/http` and `encoding/json`, over one shared connection layer.
  OTLP/HTTP JSON is a documented encoding that every collector's OTLP HTTP receiver
  accepts. Rejected: protobuf encoding (`http/protobuf`, the specification's
  default) — it needs the protobuf runtime or a hand-written protobuf encoder;
  gRPC — a dependency, and the target collectors take OTLP/HTTP.
  - **The OpenTelemetry Go SDK's exporters rejected** (settled 2026-10-08, after a
    spike on SDK v1.47.0, `otlploghttp` v0.23.0, `otelslog` v0.21.0): the OTLP
    exporters link the gRPC client stack, protobuf, grpc-gateway and genproto even
    for the HTTP exporters — about 13.5 MB over a 12.7 MB binary — and the metric
    and log HTTP exporters write protobuf only. They also break settled rules
    (`docs/specs/GATEWAY.md` → Observability): export errors are strings embedding
    the collector's response body or partial-success message, and a malformed
    headers variable prints the header value, so no remote text cannot be kept; a
    `200` login page and a JSON partial success count as delivered; a refused
    connection is not retried; redirects are followed with the credential headers
    unless a custom client is passed, which then ignores the timeout option;
    malformed `OTEL_*` values are ignored silently, and options left unset are
    filled from the environment; a histogram series cannot exist before its first
    observation (series at 0); the log batch processor drops the oldest records and
    does not expose its drops; the `slog` bridge nests groups as maps, writes times
    as integer nanoseconds and drops the error attribute's key. Our exporters cost a
    few hundred lines each and keep every rule.
- **The telemetry tree** (settled 2026-10-08): the OpenTelemetry code lives under
  `gateway/internal/telemetry/` — `otlp` (the OTLP/HTTP connection every signal
  shares: the `OTEL_*` settings per signal, the resource, one export request and
  its delivery rules), `otlplog` (the log exporter), `metric` (instruments, the
  collect step, the Prometheus text writer with the OpenTelemetry → Prometheus
  name translation), `otlpmetric` (the periodic metric exporter). It imports only
  the standard library and `kaiak/internal/netfail` (itself standard library only),
  which `scripts/check-gateway.sh` checks, and its tests use no kaiak fixtures: it
  is built to be extracted into a module of its own when a second project needs it,
  and stays in the repo until then. It keeps OpenTelemetry's model — instruments
  defined by name, unit, description, kind and attributes; the resource and scope
  apart; aggregation, collection and export separate, each reader with its own
  temporality; exporters with `ForceFlush` and `Shutdown` — not the SDK's API
  shape.
- **Persistence: none in the gateway** (settled 2026-10-07, `docs/specs/GATEWAY.md` →
  Configuration sources): it writes nothing to disk. Config, usage totals and usage
  records live in the control plane, whose host app owns the store
  (`control/kaiak-control/GUIDE.md`).
- **Metrics**: one registry of OpenTelemetry instruments (`telemetry/metric`),
  written as the Prometheus text exposition on the admin port and pushed by the
  OTLP metric exporter — both by hand (settled 2026-10-08).
- **Build & image**: `CGO_ENABLED=0` static build; multi-stage container build ending on
  a static distroless non-root base holding only the binary. Kubernetes manifests are
  out of scope (settled 2026-09-24 — operators own their cluster setup).
- **Container images** (settled 2026-09-25):
  - Gateway (`gateway/Dockerfile`, context `gateway/`): builder `golang:<go.mod
    version>` (the build script passes go.mod's `go` line), `go build -trimpath
    -ldflags "-s -w -X main.version=<version>"`; final
    `gcr.io/distroless/static-debian13:nonroot` holding only `/kaiak`, `USER
    65532:65532`, ports 8080 and 9090, no config and no volume: the gateway writes
    nothing, so it runs on a read-only root filesystem (the smoke test runs it
    `--read-only`). About 19 MB unpacked.
  - **Numeric users** (settled 2026-09-25, the follow-up audit's N-O2): `USER
    65532:65532` (gateway, the base image's `nonroot`) and `USER 1000:1000` (sample,
    `node:26-slim`'s `node`). Kubernetes checks `runAsNonRoot` against the image's
    user only when it is numeric; a name fails the restricted Pod Security Standard
    unless every pod sets `runAsUser`.
  - **Version stamping** (settled 2026-09-25, N-O7): `kaiak.build.info`'s `service.version`
    reports the `git describe` version `scripts/build-images.sh` passes as the
    `VERSION` build argument, linked into a package-level `version` string in
    `cmd/kaiak` (`main`, which passes it to the metrics and the OTLP resource) with
    `-ldflags -X`; unstamped builds fall back to Go's module
    version (a VCS pseudo-version for `go build` in a checkout), else `(devel)`
    (`go run`). This does not break "no package-level mutable state": the linker
    sets the variable before the program starts and no code assigns it — a build
    constant, like the Go version. Rejected: Go's own VCS stamping alone — the build
    context has no `.git`, so the image reported `(devel)`; a constant generated
    into the source — a generated file to keep out of commits for one string.
  - Sample control plane (`control/sample/Dockerfile`, context the repo root):
    `node:26-slim` (major pinned, patches follow), `npm ci --omit=dev` for the
    workspace, sources run by Node's type stripping, `USER 1000:1000` (runs with a
    read-only root filesystem too),
    `KAIAK_SAMPLE_LISTEN=0.0.0.0:8090`; `kaiak-control` brings its own schema copy.
    Logs are JSON only: pino-pretty is a development dependency and stays out of the
    image. About 370 MB unpacked.
  - **Releases are multi-arch** (`linux/amd64`, `linux/arm64`; settled 2026-09-28):
    the `release` workflow (`.github/workflows/release.yml`), on a `vX.Y.Z` tag,
    builds each architecture natively on its own GitHub runner, smoke-tests it there,
    pushes `<version>-<arch>`, then joins both into `<version>` (and `latest` for a
    plain `X.Y.Z`; a suffixed tag is a pre-release and leaves `latest`) on GHCR, and
    creates the GitHub Release. Rejected: emulated arm64 builds (QEMU) — the Node
    image builds slowly under emulation, and native runners let the smoke test run
    on the architecture it tests.
  - Local builds (`scripts/build-images.sh`) are `linux/amd64` only, on any Docker
    context, local or remote (the current context and its default builder unless
    set).
  - Registry naming: `<registry>/<namespace>/kaiak` and `…/kaiak-sample` (the
    registry and namespace are always given: `--repo` or `KAIAK_IMAGE_REPO`; releases
    publish to `ghcr.io/acdtrx`), tagged from `git describe --tags` without a release
    tag's leading `v` — the tag itself on a tagged commit, otherwise `<tag>-<short sha>` and `<short sha>` — plus
    `latest`. A push runs `scripts/smoke-images.sh` first (both images on a test
    network with the fake backend, file mode and control-plane mode, the gateways
    read-only with no volume, the reported version checked) and pushes only if it
    passes; the fake backend is a Dockerfile target for that test, never pushed.
- **CI** (settled 2026-09-28): GitHub Actions. `checks` runs `scripts/check-all.sh` on
  pushes to `main` and on pull requests; `release` publishes images on `vX.Y.Z` tags
  (Container images). Actions are GitHub's own (`actions/checkout`, `setup-go`,
  `setup-node`), pinned to a commit hash with the version in a comment; builds,
  pushes and releases use the runner's `docker` and `gh` directly — no third-party
  actions. `actionlint` checks the workflows (run with `go run`, never in `go.mod`).
- **Testing**: `go test -race`; `net/http/httptest` for servers; an in-repo **fake
  backend** speaking the OpenAI API, Messages, Responses and rerank (in vLLM's answer
  shape or llama-server's) that can stream, stall, fail, hang, send error events and
  omit usage on demand — the tool for retries, fallbacks, circuit breaking, draining
  and accounting tests. Integration runs against a real llama-server/vLLM are opt-in,
  never required for green.
- **Cross-half e2e** (settled 2026-09-24): `TestAcrossHalves` in `gateway/e2e`
  behind the build tag `crosshalf` — the Go harness (building `kaiak`, log waits, the
  in-process fake backend) drives the real sample control plane as a Node process.
  The tag keeps Node out of `go test ./...` and `scripts/check-gateway.sh` (whose vet
  and staticcheck still cover the tagged files); `scripts/check-all.sh` runs it, and
  it fails, never skips, without Node or `control/`'s dependencies. Rejected: a Node
  test driving Go binaries (it would rebuild the harness and the fake backend) and a
  third module (the fake backend is `internal` to the gateway).
- **Lint**: `gofmt`, `go vet` and **staticcheck** (settled 2026-09-24). staticcheck runs
  as `go run honnef.co/go/tools/cmd/staticcheck@<pinned version>` from a script: fetched
  once into the module cache, never listed in `go.mod`, never in the binary — a
  tool, not a dependency.

### Go rules (apply in `gateway/`)

- `gofmt`-clean, `go vet`-clean.
- Errors are values: wrap with `%w` and context (`fmt.Errorf("load config %s: %w", …)`),
  inspect with `errors.Is` / `errors.As`. Errors that cross the API boundary are typed,
  carrying a stable code and a message (CODING-RULES §5).
- `context.Context` is the first parameter of anything that does I/O or may block, and
  is the cancellation primitive (CODING-RULES §6) — a client disconnect cancels the
  upstream call through it.
- Every goroutine has an owner that can stop it and waits for it; no fire-and-forget
  goroutines.
- No package-level mutable state; dependencies are passed in (`cmd/kaiak` builds the
  graph). A string set only by the linker (`-ldflags -X`, the build version) is a
  build constant, not state (Container images: version stamping).
- The live config is an immutable snapshot swapped atomically; a request holds the
  snapshot it started with.

## Control plane: `kaiak-control` and the sample (settled 2026-09-24)

- **Why Node**: the real control plane (with the UI) will be a Node project; `kaiak-control`
  is the reusable core it imports, so the protocol logic — config serving, usage intake
  with idempotency, aggregation per group and global, budget decisions, gateway
  status — is written once. The sample is a thin app on it.
- **Node.js current stable, ESM, native TypeScript** — type stripping runs the source
  directly; erasable syntax only (no enums, namespaces, parameter properties);
  `tsc --noEmit` does the type checking.
- **The `kaiak-control` core is HTTP-framework-agnostic**; storage sits behind an interface (the sample
  uses an in-memory implementation; the real control plane plugs in its database). The
  library ships a **Fastify plugin** as its HTTP adapter, matching the default Node stack
  the real control plane will start from.
- **Validation: JSON Schema via ajv** against the schemas in `protocol/` — the same
  files the gateway's fixtures are checked against. No zod: one schema language.
- **`kaiak-control` carries a copy of `protocol/schema/`** in `kaiak-control/schema/`
  and reads only that (settled 2026-09-25): the package and the sample image work
  without the repository around them. `protocol/schema/` stays the source of truth;
  `npm run sync-schemas` refreshes the copy and `npm test` fails while a byte differs.
  Rejected: reading the repo-relative path (breaks any extracted package or image
  layout); generating the copy only at pack time (the repo would then need a second
  read path).
- **Sample control plane**: Fastify app using the `kaiak-control` plugin. Reads a config file,
  watches it and pushes changes; holds usage and status in memory; serves one
  read-only page — server-rendered HTML updated live over SSE, no build step, no
  frontend framework.
- **Logging: pino** (Fastify's own). Dev uses `pino-pretty` compact single-line
  (`translateTime: 'SYS:HH:MM:ss.l'` — local time, `singleLine: true`, ignore
  `pid,hostname,reqId,req.host,req.remoteAddress,req.remotePort`); per-request logging
  stays on; production is plain JSON, no transport. The sample switches with
  `KAIAK_LOG_FORMAT` — `json` (default) or `text` — the gateway's variable and values,
  so one environment serves both locally (settled 2026-09-24).
- **Boundaries**: entry-point-only imports with an acyclic graph, enforced by an in-repo
  script that reads imports with the TypeScript compiler's parser (settled 2026-09-24).
  Rejected: eslint plugins / dependency-cruiser — they add dependencies for import
  resolution the script does not need. General hygiene comes from `tsc` strict mode with
  `noUnusedLocals` and `noUnusedParameters`; no eslint.
- **The boundary lint uses TypeScript 7's `typescript/unstable/sync` API** (settled
  2026-09-24). TypeScript 7 is the native compiler; its parser is reachable from
  JavaScript only through the `unstable/*` entries (the package root exports the version
  alone), so the lint spawns the bundled compiler and reads each file's parsed import
  list. The script is the one place tied to that API — a breaking change is fixed
  there. Rejected: pinning TypeScript 6 for its classic `ts.preProcessFile` — a pin
  below latest stable (CODING-RULES §3).
- **Testing**: built-in `node:test` runner; the shared fixtures in `protocol/`.

### TypeScript rules (apply in `control/`)

- `strict` is on, including `noUncheckedIndexedAccess` and `exactOptionalPropertyTypes`.
- Avoid `any`. Use `unknown` and narrow at the boundary, or define a proper type.
- Avoid non-null assertions (`x!`). Either narrow with a guard, or fail loudly with a
  thrown error that explains the invariant.
- Async functions return Promises; errors are thrown as structured objects with at
  least a `code` and a `message` (CODING-RULES §5).
- Prefer `const`; `let` only for genuine reassignment; never `var`.

## Transport

- **Gateway ↔ control plane**: plain HTTP (usage batches and statuses as POSTs) plus
  one SSE stream per gateway that carries everything the control plane sends: the
  current config and the complete totals on connect, then every change. A reconnect
  starts over from the current state; there is no cursor to resume from. Contract in
  `docs/specs/CONTROL-PROTOCOL.md`.
- **Client ↔ gateway**: the OpenAI HTTP API, Anthropic Messages and OpenAI
  Responses; SSE for streaming responses.
- **Sample page**: one SSE connection per browser, the same shape: every section on
  connect, then the sections that changed.

## Dependency inventory

- Gateway: none. Toolchain: Go 1.27.1 (`go.mod`).
- `control/` (root `package.json`, dev): **typescript 7.0.2** — type checking
  (`tsc --noEmit`) and the parser the boundary lint reads imports with. Its platform
  binary (`@typescript/typescript-<platform>`) arrives as its own optional dependency.
- `control/` (root `package.json`, dev): **@types/node 26.6.2** — Node's built-in
  module types (`node:test`, `node:fs`, `process`, …) for `tsc`; tracks the Node major.
- `control/kaiak-control` (runtime): **ajv 8.20.0** — JSON Schema validation of the
  config document and the protocol messages (its draft 2020-12 build,
  `ajv/dist/2020`), against the package's copy of `protocol/schema/`, read at
  runtime; and `backend-verify`'s lenient checks of backend answers, against schemas
  of its own. No `ajv-formats`: date and timestamp
  shapes are schema `pattern`s, real-date checks are semantic rules. Brings four small
  transitive packages (`fast-deep-equal`, `fast-uri`, `json-schema-traverse`,
  `require-from-string`).
- `control/kaiak-control` (runtime): **fastify 5.12.5** — the HTTP framework the
  library's plugin (`controlProtocolPlugin`) adapts the core to, and what its tests run
  a real server with. The plugin imports only Fastify's types and runs on the host's
  instance; the core never touches it. Brings **pino 10.3.1** (Fastify's logger — the
  host configures it) and 43 further small transitive packages (Fastify's router,
  serializers, `light-my-request`, …).
- `control/sample` (runtime): **fastify 5.12.5** — the same pin as `kaiak-control`'s (one
  copy installed): the sample creates the Fastify instance kaiak-control's plugin runs on.
- `control/sample` (dev): **pino-pretty 13.1.3** — the development log format
  (`KAIAK_LOG_FORMAT=text`, the `dev` script's default). Brings 12 small transitive
  packages (`colorette`, `dateformat`, `fast-copy`, `help-me`, `pump`, …). `npm start`
  logs plain JSON and never loads it.
- Go dev tools (not dependencies): **staticcheck 2026.2.1** (module v0.8.1), pinned in
  `scripts/check-gateway.sh`.
