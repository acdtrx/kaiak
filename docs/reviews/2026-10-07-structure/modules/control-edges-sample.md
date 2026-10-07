# control-edges-sample — structure review

Scope: `control/kaiak-control/src/backend-verify`, `control/kaiak-control/src/fastify`,
`control/sample/src/**`. Read on `main` at `034329e`. Every non-test file read; the
tests were read only for their harnesses and how they are grouped.

## Module summaries

### kaiak-control `backend-verify` (`src/backend-verify/index.ts`, 533 lines; 4 commits on main, none earlier)
- Responsibilities:
  - checks `verifyBackend` input against the config schema's own rules;
  - sends one models-list `GET`, plus `/props` for llama-server, bounded and capped;
  - maps the answer to failure codes;
  - recognizes the server from `owned_by`;
  - reads context length and capabilities with sources, hints and notes;
  - builds the non-hint `metadata` fragment.
- Exported surface used from outside: `verifyBackend`, the `BackendReport` /
  `VerifyBackendOptions` types, and the `verify-input-invalid` error code. The only
  importer is `control/sample/src/verify/index.ts`, through the package entry.
- Dependencies: `config` (`BACKEND_TYPES`, `BackendType`), `schemas`
  (`definitionChecker`), ajv, and global `fetch`.
- Domain concepts it encodes:
  - **backend type → URL layout + credential header**: `modelsListUrl` :311 and
    `requestHeaders` :322. The gateway mirrors this in each `provider/*.go` module's
    `url()` and `header()`.
  - **which types have a list and what the list means**: the azure-anthropic early
    return :151, the azure-openai early return :170, and anthropic skipping recognition
    :183.
  - **server recognition**: `recognizeServer` :392.
  - **per-server field sources**: `readVllmEntry` :405, `readAnthropicEntry` :415,
    `readLlamaServerProps` :435 and `PROPS_CAPABILITIES` :426.
  - **failure codes**: `modelsListOf` :359.
  - **hint vs declared**: `metadataOf` :528.
- On "same knowledge twice" with the gateway's provider package: the duplication is
  **necessary**.
  - The two copies live in different languages and processes.
  - The control plane may not route through the gateway: gateway-side discovery is a
    rejected design in BACKEND-VERIFY.md (Purpose), and of the control half only
    `backend-verify` may talk to backends.
  - The overlap is small: per type, the models-list URL and the credential header.
    Everything else (owned_by, `/props`, `max_model_len`) is verify-only.
  - What can be improved is how the TS copy is shaped (F1), so that it reads 1:1
    against the gateway modules' `url()` and `header()`.

### kaiak-control `fastify` (`index.ts` 185 lines, 13 commits — hot; `gateway-stream.ts` 178; `totals-feed.ts` 176; `sse-client.ts` 97, a test helper)
- Responsibilities:
  - **`index.ts`**: the plugin.
    - mounts `/v1/{usage,status,stream}`;
    - runs the request-check hook and adds the protocol header;
    - sets the error handler and the not-found handler;
    - sets the body limits;
    - logs intake outcomes;
    - starts and stops the core with the app;
    - ends all streams on `preClose` and on `onDeliveryFailed`.
  - **`gateway-stream.ts`**: one gateway's SSE stream.
    - hijacks the reply, sets the headers, sends heartbeats;
    - handles stall and drain backpressure;
    - orders configs by read number and skips a config by its last-sent hash;
    - sends complete totals first, then the changed windows;
    - filters `counted_through` per instance;
    - tears down synchronously.
  - **`totals-feed.ts`**: one `readTotals` per push for every stream of a core.
    - rate limits reads (a leading read, then one trailing read);
    - diffs consecutive reads into the changed windows;
    - lists a window missing from a restored store at `"0"`;
    - tracks `firstRead` so a stream starts from a fresh read.
- Exported surface used from outside: `controlProtocolPlugin` and
  `ControlProtocolPluginOptions`, imported by `control/sample/src/app/index.ts` and the
  GUIDE. Nothing else leaves the folder.
- Dependencies:
  - `control-plane` (types plus the core's methods `checkGatewayRequest`,
    `acceptUsageBatch`, `acceptStatus`, `readConfig`, `onConfigRead`, `readTotals`,
    `onTotalsChanged`, `onGatewaysChanged`, `onDeliveryFailed`, `start`, `stop`);
  - `protocol` (`PROTOCOL_HEADER`, `PROTOCOL_VERSION`, `errorBody`);
  - `config-publishing` (`ConfigRead`) and `messages` (`Totals`, `TotalsWindow`,
    `BatchId`);
  - fastify.
- Domain concepts it encodes:
  - **stream ordering** (CONTROL-PROTOCOL.md, Config stream → Order):
    gateway-stream.ts :78–86;
  - **totals delta semantics**: totals-feed.ts :158–176 and gateway-stream.ts :93–117;
  - **window identity** `[group ?? null, type]`: totals-feed.ts :58;
  - **body limits per message**: index.ts :32–40.
- Split against the core: the request checks and the intakes are the core's, and the
  routes are thin. That part is clean. All of the stream's protocol sequencing,
  however, lives in the adapter (F7).

### sample `app` (`src/app/index.ts`, 132)
- Builds the Fastify instance, an in-memory store, the core with log hooks, the plugin
  mount, the status page and the config file (read on ready, watched until close), and
  N protocol replicas (more cores over the same store).
- Used from outside by `main.ts` and its own tests. Dependencies: `config-file`,
  `logging` (type only), `page`, and kaiak-control (`createControlPlane`,
  `createMemoryStore`, `controlProtocolPlugin`).
- Domain concepts: none of its own. It contains the log wording for reload runs and
  expiry sweeps. Clean.

### sample `config-file` (173)
- Reads, parses and publishes the config; skips republishing an unchanged document
  (canonical text); serializes runs; watches the directory with a debounce, including
  ConfigMap `..` swaps; records the last run and the last failure.
- Used by `app` and (types only) by `page`. Depends on kaiak-control
  (`publishConfig`, `ConfigIssue`). Clean.

### sample `page` (`index.ts` 89, `feed.ts` 209, `sections.ts` 329 — hot, 12 commits; `document.ts` 124, `format.ts` 70, `html.ts` 40)
- Responsibilities:
  - `GET /` renders the four sections; `GET /events` is an SSE feed per browser,
    coalesced to one push per second and capped at 32 streams;
  - `html` is an escaping tagged template;
  - the inline CSS and script are hashed into the CSP;
  - formatting of counts, nano-USD amounts, times and URLs.
- Used from outside by `app` (`registerStatusPage`). Depends on kaiak-control
  (`resolveScopes`, and the core's `currentConfig`, `gateways`, `totals`,
  `recentRecords` and the three `on*` listeners) and on `config-file` (types).
- Domain concepts it re-encodes:
  - **limit kinds**: which are counted (hour, month), their units, and the window
    period, in `limitRow` :270;
  - **window identity**: `limitKey` :257;
  - **window starts**: `HOUR_MS` :229 and `monthStart` :292;
  - **USD → nano**: `format.ts` :24;
  - **usage unit columns**: `usageRow` :311;
  - **the group tree**: `groupTree` :187.
- The sections' structure is sound: each section is rendered whole from the current
  state, by one function per section. The arrival of features shows only in
  `limitRow`'s type branch and `usageRow`'s fixed columns, and only the first is worth
  changing (F3).

### sample `settings`, `logging`, `keygen`, `verify`, CLIs, `main.ts`, `index.ts`
- **`settings`**: environment → `Settings`, with listen address and port parsing.
  Clean.
- **`logging`**: the pino-pretty options per format. Clean.
- **`keygen`**: argument parsing, `createKey`, and the paste-ready `keys` entry. Clean.
- **`verify`**: argument parsing, the API-key env read, `verifyBackend`, the report as
  JSON on stdout, and `explain` on stderr. It re-encodes the backend-type check and the
  config metadata's capability list (F9).
- **`main.ts`, `keygen-cli.ts`, `verify-cli.ts`**: thin process entries. Clean.
- **`index.ts`**: a package entry that nothing imports (F5).

## Findings

Ranked by payoff against cost.

### F1 — backend-verify: per-type rules spread over six branch points; replace them with one exhaustive table
- **Kind**: cross-function.
- **Where**: `control/kaiak-control/src/backend-verify/index.ts`:
  - `verifyBackend` :151 (azure-anthropic), :170 (azure-openai), :183 (anthropic: no
    recognition), :191–196 (entry reader chosen by type or server);
  - `modelsListUrl` :311–320;
  - `requestHeaders` :322–331.
- **Now**: what a type means for verification is answered in six places, each by an
  `if` or a `switch` on `settings.type`:
  - the URL suffix: `/openai/v1/models`, `/models?limit=1000` or `/models`;
  - the credential header: `api-key`, `x-api-key` or `Authorization: Bearer`;
  - extra headers (`anthropic-version`);
  - "has no list" (azure-anthropic) and "lists base models, not deployments"
    (azure-openai);
  - "recognize the server from `owned_by` or not";
  - which list-entry field gives `context_length` (`max_input_tokens` for anthropic,
    `max_model_len` when the server is recognized as vLLM).

  Every `switch` ends in a `default` / `else` that means "OpenAI-shaped". A type
  added to `BACKEND_TYPES` therefore compiles with no error and silently gets
  `Bearer` + `/models` + `owned_by` recognition.
- **How it got here**:
  - `190cd77` (verifyBackend step 2) had two shapes: OpenAI and azure-openai;
  - `d987616` (backend types) added vllm, llama-server and openai as default-branch
    types;
  - `7d51838` added the two Anthropic types as more `if`s;
  - `43f4bbe` turned azure-anthropic into an early return.
- **Proposed shape**: `const TYPE_RULES: Record<BackendType, TypeRules>`. It mirrors
  the gateway modules' `url(path)` and `header()`:
  ```ts
  interface TypeRules {
    url(base: string, path: string): string;            // as provider/<type>.go url()
    headers(credential?: string): Record<string, string>; // as provider/<type>.go header()
    list: "models" | "base-models" | "none";            // none: not-checkable; base-models: azure-openai
    listQuery?: string;                                  // anthropic: "?limit=1000"
    recognizeServer: boolean;
    entryContextField?: string;                          // anthropic: "max_input_tokens"
  }
  ```
  `verifyBackend` then reads `rules.list` once, and `vllm`'s `max_model_len` stays
  keyed by the recognized server. The `Record` makes `tsc` refuse a new backend type
  until someone decides its row. The table also reads row by row against
  `gateway/internal/provider/{openai,azure_openai,anthropic,...}.go` and the spec's
  Requests table.
- **Payoff**:
  - Adding a backend type: 6 scattered branch points → 1 table row, enforced by the
    compiler (today nothing enforces it).
  - About 20 lines of branching become about 30 lines of table, roughly neutral in
    size, but the "default means OpenAI" trap is gone.
- **Cost / risk**: small, one file. The existing 769-line test file covers each type's
  URL and headers, so its tests should stay green unchanged. No contract change.
- **Confidence**: high.

### F2 — backend-verify: one "read a context length" block written three times, one "describe an answer" written twice
- **Kind**: in-function / cross-function.
- **Where**: `backend-verify/index.ts`:
  - `readVllmEntry` :405–413 and `readAnthropicEntry` :415–423, identical except for
    the field name;
  - the `/props` context read :455–461, the same block again;
  - `modelsListOf` :359–389 and `propsOf` :481–498: six of seven `Answer` cases
    produce the same message text;
  - `modelAt` :399–403 exists only because the entry readers are called by index
    inside `forEach`.
- **Now**: the triple copy is "`readField(doc, field, isContextLength)` → set
  `context_length` + `sources.context_length`, else push `absentNote`".
  `modelsListOf` maps the `Answer` kind to a code and a message; `propsOf` repeats the
  same messages without the code.
- **How it got here**: vLLM came first (`190cd77`), Anthropic later (`7d51838`), and
  each was copied from the one before. `propsOf` was written beside `modelsListOf`
  rather than factored with it.
- **Proposed shape**:
  - `readContextLength(doc, url, field, model)` used three times;
  - `describeAnswer(answer, url, timeoutMs): string` for every non-JSON kind, with
    `modelsListOf` adding only the code (and the 401/403 and 3xx wording);
  - build `models` with `list.data.map(entry => { const m = …; read…; return m; })`,
    which removes `modelAt`.
- **Payoff**: about 35 lines and two helpers removed. Message wording lives in one
  place. A fourth context source (one is likely, e.g. SGLang) becomes one call.
- **Cost / risk**: small. Notes are not a contract (spec: "wording is not a contract"),
  and tests that match on note text need to keep the same strings.
- **Confidence**: high.

### F3 — The page re-derives totals-vs-limits from a gateway-shaped message; `core.totals(instance)` is left with one production caller, which passes `""`
- **Kind**: cross-module.
- **Where**:
  - `control/sample/src/page/sections.ts` :227 (`PAGE_READER = ""`), :229 `HOUR_MS`,
    :233 `core.totals(PAGE_READER)`, :257 `limitKey`, :262 `usedByLimit`, :270–283
    `limitRow`, :292 `monthStart`;
  - `page/format.ts` :24 `usdToNano`;
  - `control/kaiak-control/GUIDE.md` :594 and :601–612, which tell every host to write
    the same matching code;
  - library side: `usage/windows.ts` :11–21 (`currentWindows`, `windowStartFor`),
    `usage/aggregate.ts` :13 (`COUNTED_TYPES`), `fastify/totals-feed.ts` :58
    (`windowIdentity`), `messages/semantic.ts` :42 (identity again),
    `control-plane/index.ts` :216–219 (`totals(instance)` wraps `readTotals`).
- **Now**:
  - The page calls `totals("")`, which builds a gateway `Totals` message with a
    `counted_through` for a fake instance; the page throws that field away.
  - It rebuilds the window identity (the fifth copy of `JSON.stringify([group ?? null,
    type])` in `control/`).
  - It recomputes the hour and month window starts with its own clock, though the core
    already returns `windowStarts` from `readTotals()` by the core's clock.
  - It decides which limit types are counted with an `if`, while the library holds
    `COUNTED_TYPES`.
  - `grep` finds no production caller of `ControlPlane.totals` other than this page.
    The stream moved to the totals feed's `readTotals` in `635b126`, and only tests
    call `totals(instance)` now.
- **How it got here**:
  - `totals(instance)` was the stream's read before control-replicas step 12, and the
    page reused it.
  - When the stream moved to `readTotals` (`635b126`), the page kept the old call, and
    the GUIDE snippet copied the page.
- **Proposed shape**, in two sizes:
  - **(a) Cheap, sample only.** Read `core.readTotals()`. Use `read.windowStarts[type]`
    as the fallback start and `read.liveGateways` for the count. This drops
    `PAGE_READER`, `HOUR_MS` and `monthStart`. Then remove `totals` from the
    `ControlPlane` surface, and from `Usage` if `usage`'s tests can call `readTotals`
    plus `countedThrough`. Update GUIDE §9 to match.
  - **(b) Library-owned view.** Every real control plane with a UI has to write this
    join (the GUIDE says so). Add `limitUsage(config, read)` to kaiak-control →
    `{ scope: ResolvedScope; limit: Limit; counted: false } | { …; counted: true;
    used: bigint; ceiling: bigint /* nano for usd */; windowStart: number }` per scope
    and limit, built from `resolveScopes`, `COUNTED_TYPES` and one exported
    `windowIdentity`.
    - The page's `limitKey`, `usedByLimit`, `usdToNano`, the type branch and the
      fallback start all go.
    - The GUIDE snippet becomes one call.
    - The four internal copies of the identity can share the one export.
- **Payoff**:
  - (a) removes about 15 lines, a fake-instance hack and one public method
    (`totals`).
  - (b) also removes about 30 page lines, collapses 5 identity copies into 1, and
    takes away the hand-written join that every host has to write.
  - A new counted limit kind (e.g. a daily window): today it touches page `limitRow`
    and the GUIDE snippet in addition to the library; after (b), library only.
- **Cost / risk**:
  - (a) is small, but removing `totals` is a public kaiak-control API change and
    needs tests rewritten in `usage.test.ts`, `control-plane*.test.ts` and the fastify
    tests.
  - (b) is medium: a new public function, GUIDE §9, and page tests.
  - No protocol change either way.
- **Confidence**: high for (a). Medium for (b): it has one current caller, so (b) rests
  on "every host re-writes it" (the GUIDE's own words) rather than on two uses in the
  code.

### F4 — The fastify tests: five files, five private copies of one harness, two of them named after review rounds
- **Kind**: cross-function (tests).
- **Where**: `control/kaiak-control/src/fastify/{fastify,round-2,round-3,status-totals,usage-route}.test.ts`.
  The preambles run 61–161 lines before the first test.
- **Now**:
  - Each file has its own fixture reader, core builder, `closers` with `afterEach`,
    `startApp`, `openStream` and gateway headers. `configNumbered` appears twice.
  - All of them hard-code `"kaiak-protocol": "5"`: 5 fastify files, plus 4 more in
    `control/` (`protocol.test.ts`, `control-plane.test.ts`, `sample/src/main.test.ts`,
    `sample/src/app/app.test.ts`).
  - `round-2.test.ts` and `round-3.test.ts` group tests by the audit round that found
    them, not by behavior. Examples: "totals keep listing a window the current config
    no longer limits" and "a delayed first status read does not overwrite a newer
    status" are core or totals behavior tested through HTTP.
- **How it got here**: the round-2 and round-3 fixes of the control-replicas reviews
  (`c2b1b87`, `a7526cd`) each added a self-contained file instead of extending the
  subject files.
- **Proposed shape**:
  - One `fastify/test-harness.ts` beside `sse-client.ts`, with `startApp`,
    `openStream(base, instance?)`, `core(store, opts)`, `configNumbered`, `fixture`
    and closers, building headers from `PROTOCOL_VERSION`.
  - Fold round-2 and round-3 into the subject files: stream ordering →
    `fastify.test.ts`; totals → `status-totals.test.ts`; status receipt →
    `control-plane` tests.
- **Payoff**:
  - About 250 lines of duplicated setup removed.
  - A protocol version bump touches 0 fastify test files instead of 5.
  - A reader looking for "stream ordering" tests finds them in one file.
- **Cost / risk**: medium, mechanical, tests only. No contract change.
- **Confidence**: high on the duplication. The regrouping is judgment.

### F5 — `control/sample/src/index.ts` is a package entry nothing imports
- **Kind**: cross-module.
- **Where**: `control/sample/src/index.ts` (21 lines, 13 names re-exported) and
  `control/sample/package.json` `"exports": "./src/index.ts"`.
- **Now**: nothing imports it.
  - No file imports `kaiak-sample`.
  - The process entries, the Dockerfile (`CMD node sample/src/main.ts`) and the e2e
    (through `npm run dev -w sample`) all bypass it.
  - The sample's tests import the subsystems directly.
- **How it got here**: it was created with the sample (`5a03046`, private history) in
  the shape of the kaiak-control package entry.
- **Proposed shape**: delete it and the `exports` field. Exports that then exist only
  for it go too, unless the module's own tests use them: `STARTUP_TRIGGER`,
  `WATCH_TRIGGER`, `DEFAULT_LISTEN` and `formatKey` are each used inside their module.
- **Payoff**: one file, one false concept ("the sample is a library"), and 13
  re-exports that had to track every rename.
- **Cost / risk**: trivial. Check that `check-boundaries.ts` does not require a
  package entry (it treats subsystems as `src/<dir>/index.ts`, so it does not appear
  to).
- **Confidence**: high.

### F6 — `errorBody` takes only `ProtocolError`; the two intake routes hand-map the identical shape
- **Kind**: cross-function.
- **Where**:
  - `fastify/index.ts` :134–136 and :154–156:
    `{ error: intake.error.code, detail: intake.error.message }`;
  - `protocol/index.ts` :56 `errorBody(error: ProtocolError)`;
  - the intake error types `usage/index.ts` :34 and `gateways/index.ts` :36 are both
    `{ code, message, status: 400 }`.
- **Proposed shape**: `errorBody(error: { code: string; message: string })`, so both
  routes become `reply.code(e.status).send(errorBody(e))`. Possibly one shared
  `IntakeError` type for the three identical shapes.
- **Payoff**: two hand mappings removed, and one way to make an error body.
- **Cost / risk**: trivial. No contract change.
- **Confidence**: high. The payoff is small.

### F7 — The stream's protocol sequencing lives in the Fastify adapter; the GUIDE tells other hosts to re-implement it
- **Kind**: cross-module.
- **Where**:
  - `fastify/gateway-stream.ts` :57–117: read-order and hash skipping; complete, then
    changed totals; the `counted_through` filter;
  - `fastify/totals-feed.ts` entirely: framework-free except a `FastifyBaseLogger`
    type;
  - `control/kaiak-control/GUIDE.md` :147–156: "a host serving streams some other way
    does the same, ordering … by when the read was issued …, starting each stream's
    totals from a `readTotals` issued after the stream connected";
  - `docs/TECH-STACK.md` :171: "The kaiak-control core is HTTP-framework-agnostic … the
    protocol logic … is written once".
- **Now**: the most subtle protocol logic in `control/` sits under `fastify/`, beside
  the socket handling: the ordering rules from CONTROL-PROTOCOL.md (Config stream →
  Order), the delta rules for Totals, and the fresh-read rule for a new stream. Two
  consequences:
  - Core behavior can only be tested through HTTP and SSE (see F4's
    `round-2`/`round-3` tests).
  - A non-Fastify host has to re-derive about 250 lines from a GUIDE paragraph.
- **How it got here**: control-replicas step 12 (`635b126`) rewrote the stream and put
  the new feed next to it. The previous stream was simpler and adapter-sized.
- **Proposed shape**:
  - Move `totals-feed.ts` into the core (e.g. `control-plane/` or `usage/`), taking a
    plain logger callback.
  - Extract from `gateway-stream.ts` a framework-free session:
    `openGatewaySession({ core, feed, instance, send(event: string): void,
    writable(): boolean })`, returning `{ drained(), close() }`.
  - Leave in the Fastify file: hijack, headers, heartbeat, the stall timer and socket
    events (about 70 lines).
- **Payoff**:
  - Protocol logic is written once, as TECH-STACK claims.
  - The ordering and delta rules get unit tests without a server.
  - GUIDE §4's paragraph shrinks to "call `openGatewaySession`".
- **Cost / risk**:
  - Medium: about 350 lines moved, a new core API, and the backpressure coupling
    (totals held while `writableNeedDrain`) must cross the seam as `writable()` and
    `drained()`.
  - There is only one adapter today, and TECH-STACK says the real control plane
    starts from Fastify. So under the two-uses bar this is a "responsibility belongs
    in the core" argument, not a generalization.
- **Confidence**: low–medium. It would rise if a non-Fastify host were planned, or if
  the next stream change again had to be tested only through HTTP.

### F8 — Two hand-written SSE writers (gateway stream, page feed) have already drifted
- **Kind**: cross-module.
- **Where**: `fastify/gateway-stream.ts` :29–36, :44–52, :63–76, :136–163, and
  `sample/src/page/feed.ts` :45–52, :144–187.
- **Now**: identical `EVENT_STREAM_HEADERS` and `HEARTBEAT` constants, and the same
  hijack / `writeHead` / `flushHeaders`, write-with-stall-timer-on-drain, "heartbeat
  unless `writableNeedDrain`", `raw.destroyed` check and end-all set. The two copies
  have drifted:
  - the gateway stream got a `closed` mark checked by every write and an
    `error` listener (round-2, "a stream ended during config delivery writes nothing
    into the ended response");
  - the page feed has neither (see Bugs).

  Both also implement the same "leading run, then one trailing run per interval"
  limiter: `totals-feed.ts` `requestRead` :78–93 and `feed.ts` `markChanged` :105–118.
- **Proposed shape**:
  - Either export a small `openEventStream(reply, { heartbeatMs, stallMs, log })` →
    `{ write, end, onClose }` from kaiak-control and use it in both;
  - or, cheaper and arguably better, since kaiak-control's purpose is the protocol
    rather than browser pages: fix the page feed in place to match (close mark +
    `error` listener), and accept the duplication.
- **Payoff**: about 40 lines either way. Sharing stops future drift; the in-place fix
  closes the current gap.
- **Cost / risk**: sharing adds public API to kaiak-control for the sample's benefit.
  The in-place fix is about 5 lines.
- **Confidence**: medium that the drift matters. Low that sharing is worth the new
  public surface. The recommendation is the in-place fix.

### F9 — verify CLI: a stacked type check and a hand-copied list of required metadata
- **Kind**: in-function.
- **Where**:
  - `control/sample/src/verify/index.ts` :76 and :98–100: `isBackendType` checked
    before `verifyBackend`, which checks the same thing against the schema and throws
    `verify-input-invalid`, already handled at :48;
  - :129–135: `["streaming","tools","vision","reasoning"]` and `reasoning_efforts`,
    the config schema's required `metadata.capabilities`, typed by hand;
  - :123: a cast to `"vision" | "tools" | "reasoning"`.
- **Proposed shape**:
  - Pass the string through and let the library's input error speak. That costs
    losing the echo of the bad value; the library deliberately echoes nothing, though
    a `type` is not secret.
  - For the capability list, either import a constant kaiak-control already has
    (none is exported today) or leave it.
- **Payoff**: about 6 lines. A capability added to config metadata would no longer
  need a sample edit (today it does).
- **Cost / risk**: trivial.
- **Confidence**: medium. Low payoff, so "leave as is" is defensible.

### Clean
- Clean: `sample/settings`, `logging`, `keygen`, `config-file`, `app`, `main.ts`, the
  CLIs, and `page/html.ts`, `format.ts` (apart from `usdToNano`, F3) and
  `document.ts`.
- `document.ts`'s client copy of `formatRelative` is necessary, because it runs in the
  browser, and it is commented as a pair.
- Fastify `index.ts`'s route handling, against the core, is otherwise clean: checks and
  intakes are the core's, and routes only map results to HTTP and log lines.

## Cross-module hints

- **Window identity `JSON.stringify([group ?? null, type])`** has 5 copies:
  `kaiak-control/src/messages/semantic.ts:42`, `usage/aggregate.ts:17` and
  `storage/memory.ts:188` (both with `windowStart`), `fastify/totals-feed.ts:58`,
  `sample/src/page/sections.ts:258`. One exported helper would serve all.
- **`ControlPlane.totals(instance)`** has no production caller except the sample page
  with `""` (F3). The usage reviewer should weigh removing it from `Usage`.
- **Backend type → models-list URL + credential header** lives in three places:
  - `gateway/internal/provider/*.go` (`url()`, `header()` per module);
  - `backend-verify/index.ts` :311–331;
  - two spec tables (GATEWAY.md :423–440 and BACKEND-VERIFY.md Requests).

  The duplication is necessary across languages, but no test ties the two code
  copies. A shared fixture in `protocol/fixtures/` (type → expected models-list URL
  for a sample base_url, credential header name and form, extra headers) checked by
  one test per half would catch drift. That follows the existing "protocol/ is what
  both halves test against" pattern. Anthropic's `?limit=1000` and
  `anthropic-version` are the likeliest to drift.
- **Usage unit columns**: `sample/src/page/sections.ts` :306 and :321–325 list the five
  `units` fields by hand. A new usage unit touches the page as well as the schema,
  messages, gateway accounting and usage aggregation (cache-write in `38c1e54` did).
  This is acceptable for a UI. Worth counting in the usage-field shotgun tally.
- **Three identical `{ code, message, status }` error types**:
  `protocol/index.ts:20`, `usage/index.ts:~31`, `gateways/index.ts:~33` (F6).
- **`messages`/`config` own the config-metadata capability list**, which the sample
  `verify` copies by hand (F9). If the metadata gains a capability, grep the sample.

## Bugs noticed in passing

- **The page feed can write after `end()`, which crashes the process at shutdown**
  (`control/sample/src/page/feed.ts` :156–168, :205–207).
  - `endAll()` (`preClose`) calls `raw.end()`, but the browser stays in `open` and
    `receiving` until the `close` event. A heartbeat tick or a push turn finishing its
    render in that window calls `raw.write` on an ended response.
  - `ServerResponse` then emits `ERR_STREAM_WRITE_AFTER_END` as an `error` event, and
    no listener is attached. I confirmed on Node v26.9.0, with a standalone script
    (now deleted), that this becomes an uncaught exception.
  - The gateway stream fixed this exact shape in round-2 (`closed` mark +
    `raw.on("error")`). The window is narrow and only at shutdown, so the effect is
    exit status 1 instead of 0.
- **The config section is O(k²) at target scale** (`page/sections.ts` :191, :193).
  - `keysByGroup` grows each group's array by spreading. 7k keys in one group is about
    24M element copies per config render.
  - `childrenOf` scans every scope for each node, which is O(groups²).
  - Every key is also rendered twice: in the Keys list and in the group tree.
  - This runs only on config renders (publish or file run), not per totals push. It is
    a demo page, but noted against the 3–7k keys target.
