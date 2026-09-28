# Coding Rules

Generic coding principles for any contributor (human or coding agent) working on this codebase.
They are project-independent: anything that names a concrete technology, directory, or framework
is bound by the project's root `AGENTS.md` (see its Project Facts).
Language-specific rules live in `TECH-STACK.md`, beside the language they govern (§12).

Projects specialize these rules by weaving their concrete instances inline
("Applied: …") under the matching principle — the principle stays general, the
application names the project's case. Section numbers are stable: `CODING-RULES §N`
comments in the source refer to them.

---

## 1. Naming and Semantics

- Functions, files, and modules are named for their **purpose and domain**, not for the underlying
  mechanism or API they call.
- No generic action dispatchers (e.g. `performAction(name, action)`). Each operation gets its own
  purpose-named function.
- Avoid vague names like `doAction`, `update`, `handle`. Be specific: `executeDiskOperation`,
  `updateField`, `validateInput`.

## 2. Code Sharing — by Purpose, Not by Shape

- Extract shared code only when the call sites serve the **same purpose**. Deduplicate by what the
  code *means* in the domain, not by what it happens to look like today.
- Two functions or components that look similar but serve different purposes stay separate — as a
  project grows, their differences grow too, and a premature merge becomes a convoluted
  mega-function or mega-component held together by flags.
- When two things genuinely share a purpose, extract the shared core into a common module or
  component; keep the thin variant layer separate rather than multiplying mode/flag props on one
  unit. If a parameter list is accumulating booleans that change behavior, that is the signal to
  split, not to add another flag.
- Shared utilities live in dedicated modules; never copy-paste the same helper into multiple files.

## 3. Minimal External Dependencies

- Do not add a library for functionality that can be implemented as a small function or that the
  platform already provides.
- Prefer platform built-ins over third-party equivalents.
- Every dependency and tool enters the project at its **latest stable version**, and must
  be actively maintained and non-deprecated — verify both before introducing it. When a
  package becomes deprecated, migrate to its successor. Keep dependencies current
  deliberately: upgrades are scheduled small work items, not emergencies. A genuinely
  blocked upgrade gets its block and recheck condition recorded — never a silent pin.
- No CDN-loaded assets. All JavaScript, CSS, and fonts must be bundled or use system defaults.
- Code-split large features with dynamic imports to keep bundle chunks small.

## 4. Structured Data Parsing

- Never parse structured formats (XML, JSON, YAML, HTML, etc.) with regular expressions. Use a
  dedicated parser.
- When modifying a parsed document, extract the existing element and mutate it rather than
  constructing a replacement from scratch.
- Parsing yields a tree, not meaning: read values from where each contract puts them,
  never by scanning for records that merely *look like* the target. Prefer designs whose
  failure mode is visible-and-absent over silent-and-plausible.

## 5. Error Handling

- Errors are **structured**: at minimum a stable `code` and a human-readable `message`. The
  language idiom that carries them (thrown objects, returned error values) is in `TECH-STACK.md`.
- API responses on failure use a consistent shape across the entire application:
  `{ error, detail? }` — `error` always present, `detail` included where it adds value. The same
  shape applies on HTTP and streaming channels.
  - Applied: the control protocol and `kaiak-control` use this shape. The gateway's
    client API uses the OpenAI error shape instead — the API it implements dictates it
    (`docs/specs/GATEWAY.md`).
- Errors shown to the user are sticky — they remain visible until the user explicitly dismisses
  them. No auto-dismiss on subsequent success.
- Prefer inspecting state up front and choosing the right path once, rather than try/catch/retry as
  control flow.
- Every silent `catch {}` block must have a comment explaining why the error is intentionally
  swallowed.

## 6. Async Patterns and Timing

- Never use `sleep` or timer delays to work around race conditions. Use event signals, or retry
  with exponential backoff.
- Timers used for **scheduling** (periodic push updates, reconnect backoff, TTL cleanup, cron-like
  checks) are allowed; they must not substitute for waiting on the correct readiness signal.
- Use `AbortController` (or the platform's cancellation primitive) for cancellable operations
  instead of boolean flags.
- Prefer streaming over buffering: pipe data through transform streams instead of reading an
  entire payload into memory and then writing it out.

## 7. Architecture Boundaries

- External system integrations (databases, APIs, system daemons) are accessed through a single
  dedicated module. No other module imports the client library or opens a connection directly.
- Subsystems live in dedicated folders, each with a single public entry point. Other code
  imports a subsystem only through that entry, and the dependency graph stays **acyclic**
  — no declared layer order to maintain: tooling computes the import graph and fails on
  deep imports and on any cycle (a lint, or the compiler where the language enforces it).
  The application layer does the wiring. Exposing an entry-point API aimed at one
  specific consumer is a design smell — surface it for review instead of shipping it
  quietly.
- **Capability fences are extended by declaration, never worked around.** When a subsystem
  legitimately needs a fenced capability (network access, the database), the fix is one
  line: add it to the owners list in the boundary lint and flag the addition for review.
  Routing around a fence (alternate APIs, moving code into an owner) is never acceptable.
- **Decouple mechanism from trigger.** Any background or maintenance mechanism is an
  invocable unit; its triggers (debounce, boot, schedule, events, a manual run) are wired
  separately, and any trigger can invoke any mechanism. A manual invocation must always
  work — for testing, or because it is needed now. Every execution records what triggered
  it, when, and the result.
- Prefer native/API access over shelling out to CLI tools. Only exec a binary when there is no
  practical programmatic alternative, and confirm first.
- **All operations execute on the backend; the frontend is an observer.** The client
  inputs data and commands and renders server state — it never holds authoritative state
  (e.g. browser localStorage) or performs an operation the server could not complete
  with no client attached.
- **Live data is pushed, not polled.** Use the project's server-push channel (SSE, WebSocket, or
  realtime framework — see `TECH-STACK.md`) for data that updates over time. One-time GET is
  acceptable for static or on-demand data.
- All persistent state lives in the **designated state directory defined in the root `AGENTS.md`**.
  The application is the source of truth; nothing may rely on writing outside it to function.

## 8. Frontend Patterns

- Initialise form/component state with meaningful defaults derived from the data model. Never
  initialise with an empty object and hope for the best.
- Effect dependencies must be **stable**: depend on primitive values or serialised representations,
  not on object references that change identity every render.
- Memoise callbacks that are passed to effects or child components to prevent unnecessary
  re-executions.
- Do not reimplement functionality that the framework already provides. If something seems missing,
  investigate the framework first.

## 9. Security

- Validate and sanitise all user input at the **API boundary** using the one schema-validation
  mechanism named in `TECH-STACK.md` — not ad-hoc checks or a second validation library (§3). Internal library functions may assume valid input.
- Pass secrets to subprocesses via stdin or environment variables, never as command-line arguments
  (visible in process lists).
- Sanitise subprocess error output before exposing it to clients (e.g. mask credentials).
- Rate-limit authentication and other sensitive endpoints.
- Block requests to private/loopback addresses when the URL originates from user input (SSRF).
- Never put auth tokens in URLs (`?token=...`) — they get logged. Use cookies or `Authorization`
  headers.

## 10. Code Quality

- Remove all debug logging before committing.
- Fix root causes, not symptoms. Do not add workaround scripts to patch over underlying bugs.
- When a module grows large, split it by **domain of functionality**, not arbitrarily.
- If a helper is small and used in only one file, keep it inline. Do not create a module for every
  three-line function.
- No commented-out code in the repository. Use version control history to recover old code.
- When adding a new subsystem, mirror existing patterns: dedicated module, dedicated routes.

## 11. Code Style

- Prefer early returns. Use guard clauses at the top of functions to handle error/edge cases and
  return early, reducing nesting depth.
- Consistent import ordering: (1) platform/standard library, (2) third-party packages,
  (3) project modules. Separate groups with a blank line.
- One UI component per file. Small local sub-components may stay in the same file only if they are
  not exported.
- Colocate related files. Keep components, styles, and tests for a feature in the same directory.

## 12. Language Rules

- Language-specific rules — strictness settings, error idioms, concurrency and cancellation
  conventions, formatting — live in `TECH-STACK.md`, one block per language in use. They bind
  as firmly as the sections here; this file stays language-neutral.

## 13. Prompts and Model-Facing Text (when the project calls LLMs)

- **Prompt fixes generalize.** When a prompt, skill or fragment produces a wrong output in
  a tested case, improve the general definition or accept the variance — never add an
  exception targeting exactly the observed case. A rule per failure is overfit teaching;
  if the general wording cannot be made better, the miss is recorded in the backlog and
  watched, not patched.
