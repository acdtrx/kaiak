# kaiak — Project Rules

> Entry point for any coding agent working on this project.
>
> **Read `docs/kaiak.md` first** — the philosophy there binds design
> decisions; these rules assume it.
>
> **What lives where:** this file — working method and hard rails; `docs/CODING-RULES.md`
> — generic coding principles, always applicable; `docs/TECH-STACK.md` — the stack and
> the reasoning behind it; `docs/specs/<SUBSYSTEM>.md` — the contract docs, one per
> subsystem; `docs/plans/` — plans for large work; `docs/BACKLOG.md` — deferred items;
> `docs/reviews/` — audits.
>
> **Structure:** sections marked `[PROJECT]` are the project's own — filled in per
> project and edited as it grows. Everything else is stable methodology shared with the
> template this structure comes from: change it deliberately, not casually. When a
> methodology section is legitimately replaced by a project-specific version (e.g. a
> project with no test suite replaces Testing & Verification with its own verification
> model), the replacement is marked `[PROJECT]` too. The markers are the seam for
> template syncs: unmarked sections diff mechanically against the template — divergence
> there is either drift to fix or a lesson to backport; marked sections are expected to
> differ.

## Interacting with the user

Assume the user has technical knowledge. Use concise but clear responses. Prefer lists of
items over long prose paragraphs. Avoid terminology that is niche or trendy, prefer plain
english.

## Project Facts `[PROJECT]`

- Two halves, one repo (layout in `docs/TECH-STACK.md`):
  - `gateway/` — the product: Go (current stable), one static binary `kaiak`. Go modules
    only; third-party modules only as the dependency rule below allows.
  - `control/` — Node.js (current stable), ESM, native TypeScript; npm workspaces
    `control/kaiak-control` (the reusable library, package of the same name) and
    `control/sample` (the sample control plane). Package manager: npm — do not
    introduce others.
  - `protocol/` — JSON Schemas and shared fixtures: the contract both halves test against.
- Commands (keep this line current):
  everything — `scripts/check-all.sh` (the gateway checks, control `npm test` +
  `npm run lint`, then the cross-half e2e: the sample control plane and two gateways,
  also on two control-plane cores over one store, `gateway/e2e` build tag `crosshalf`;
  any cwd) — the one command for verification;
  gateway — all checks `scripts/check-gateway.sh` (gofmt, vet, staticcheck, uncached race
  tests incl. the e2e test in `gateway/e2e`, then lint and self-test of the live-test
  kit in `scripts/live`; any cwd) · run `go run ./cmd/kaiak` from `gateway/`;
  live backends (opt-in) `go -C scripts/live run . -kind vllm|llama-server|openai|azure-openai|anthropic|azure-anthropic …`
  (`docs/testing/LIVE-BACKENDS.md`);
  control — from `control/`: `npm test` · `npm run lint` (`tsc` + boundary lint) ·
  sample control plane `KAIAK_SAMPLE_CONFIG=<file> KAIAK_CONTROL_TOKEN=<token> npm run dev -w sample`
  (`npm start -w sample` for JSON logs) · keygen
  `npm run keygen -w sample -- --id <key-id> --group <id>` · verify a backend
  `npm run verify -w sample -- --base-url <url> [--type <type>] [--api-key-env <NAME>] [--model <name>]`;
  images (linux/amd64, on any Docker context; any cwd) — build
  `scripts/build-images.sh --repo <registry/namespace> [--context <ctx>] [--builder <builder>]`,
  smoke `scripts/smoke-images.sh --gateway <image> --sample <image>`, push
  `scripts/build-images.sh --push` (smoke first, pushes only if it passes).
- Hard constraints:
  - The gateway's dependencies are **minimal, maintained and clearly worth what they
    cost** (settled 2026-10-08): each one a dated ruling in `docs/TECH-STACK.md`, asked
    for first. The binary stays usable standalone — one static binary, depending on no
    service but the control plane; storage and management stay with the app built on
    `kaiak-control`.
  - The gateway serves traffic with **no control plane reachable** (file mode; in
    control-plane mode once booted, or from the seed config at boot) — the control
    plane is never in the request path.
  - The control plane owns config, usage totals and budgets; the gateway holds no state
    and writes nothing to disk.
  - Subsystem boundaries: Go `internal/` packages (compiler-enforced visibility, no import
    cycles); `control/` entry-point-only imports with an acyclic graph (lint-enforced).
- Do not add new dependencies without asking first. Every dependency enters at its latest
  stable version, verified maintained (`docs/CODING-RULES.md` §3).
- Full stack details and the reasoning behind them: `docs/TECH-STACK.md`.

## Triage

Classify every request before acting:

- **Small** — bug fix, rename, config tweak, isolated change (roughly <50 LOC, one
  subsystem): implement directly, then run the relevant tests. No plan files, no ceremony.
- **Large** — new feature, refactor, anything spanning multiple subsystems or sessions:
  follow Pre-Implementation Analysis and the Plans workflow below.

When unsure, ask — a one-line question is cheaper than a wrong plan or a sprawling "small" fix.

## Pre-Implementation Analysis (large work)

1. **Read first.** `docs/CODING-RULES.md` always; the matching `docs/specs/` entry and the
   relevant `docs/TECH-STACK.md` sections for the subsystem being touched; the relevant
   source.
2. **Validate the request.** Restate it, list ambiguities and assumptions surfaced by the
   codebase, and confirm with the user before planning.
3. **Implement following documented patterns.** Where the docs and the code disagree, trust
   the code and flag the discrepancy.

## Plans

For large work, create `docs/plans/<topic>/`:

- `OVERVIEW.md` — goal, scope, out-of-scope, constraints, risks, the step list grouped
  into **phases** (each phase names its goal and its steps: phase 1 = steps 1–4, …), and
  how the finished feature will be verified end to end.
- `STEP-N-<name>.md` — one file per step. Each states **intent, files likely touched,
  decisions made during planning, and acceptance criteria** — not implementation code.
  Each step must be independently verifiable.

Plans capture the *thinking* so a future session (or another agent) can pick up mid-stream.
After completing a step, record its status in the step file and commit before moving on.

Steps group into **phases** by goal. The full suite must be green at **phase ends**, not
after every step — forcing green mid-phase bends rework out of shape (code kept a step
longer only so a test passes, then deleted). Within a phase a step still runs the suite
and records the result in its step file: expected reds are named, each with the phase
step that clears them — never left silent. A phase ends committed and green, a
legitimate stopping point for the plan.

Plan steps are implemented by **subagents only**, running the same model as the main
session: the main session prepares each step's brief, launches the subagent in the
worktree, and reviews its work against the step's acceptance criteria — it does not
write the implementation itself.

## Testing & Verification

- New behavior needs tests: component tests for logic, e2e for user-facing flows.
- A step is **done** only after the suite has actually been run and its result recorded —
  green, or the expected reds named with the step that clears them; the suite must be
  fully green at every **phase end** (see Plans), and a plan is done only after the full
  suite passes. Show the output. Never claim success without running something.
- Never skip, delete, or weaken a failing test to make the suite green. If a test seems
  wrong, say so and ask.

## Debugging

Find the root cause before fixing. No symptom patches, no speculative try/catch wrapping,
no `setTimeout` to mask races, no "this should fix it" without understanding why it broke.

- **Prove the failure mechanism before designing the fix.** Name the specific failure
  mode, then find the one-line check that proves it (a log tail while reproducing, a
  CLI probe, runtime state). If you can't articulate the check, you don't understand
  the bug yet — keep investigating. "Designing for both cases" usually means this step
  was skipped. When the user asks for manual validation, treat it as a real request —
  it usually catches a leap.
- **No stacked safety nets.** Fix the mechanism and trust events. One reconciler per
  legitimate concern is fine; layering a second polling/probing fallback for the same
  concern is clutter. Fall back to polling only when a real failure mode demands it —
  and ask first.

## Scope

- Don't silently fix or refactor unrelated code mid-session: either it's in scope, or it
  goes to `docs/BACKLOG.md` as a structured entry. The user can also ask to "add this to
  the backlog" directly. Entries that grow graduate to `docs/plans/<topic>/`.
- YAGNI: build what the step requires, nothing speculative.
- **Features earn their place.** Brainstorm freely, build reluctantly: unless there is a
  strong reason to believe a feature helps, find a confined way to test its usefulness
  before building it in. Both the user and the agent hold this line — and the agent should
  invoke this rule out loud when feature imagination runs ahead (the user asked to be
  reminded).
- Match the existing patterns of the file being edited over personal preference.

## Documentation

Document **decisions and contracts, not implementations**. If a doc would merely restate
what the code already says, don't write it — the code is the source of truth and mirrors
go stale.

**Decisions live where their topic lives — inline, dated.** A settled decision is recorded
in the doc that owns it: subsystem behavior in that subsystem's `docs/specs/` file, stack
choices in `docs/TECH-STACK.md`, methodology in this file — tagged `(settled YYYY-MM-DD)`,
with the reasoning, and what was rejected when that matters. There is no separate decision
log: one home per decision, no duplicates — a decision that was merely *discussed* and
already lives in its owning doc is not recorded again anywhere else.

**Comments hold current agreements only.** A comment states the constraint, invariant or
reasoning that is true *now* — never the code's own history: no "since <date>", no
"used to be", no narration of previous iterations. Git is the archive. Naming a rejected
alternative is allowed only when it documents a live constraint (why the obvious
approach fails), not what this code did before. When touching code, bring any
history-narrating comment you meet up to this rule.

- `docs/specs/<SUBSYSTEM>.md` — one per subsystem, **created during the design/planning
  of the feature that first settles that subsystem's behavior** — not extracted later
  from code. It holds the subsystem's purpose, its principles, its genuine contracts
  (external API shapes, invariants, protocol behavior) and its settled decisions, dated.
  Update it **in the same edit** that changes a contract or settles a direction;
  implementation changes that alter no agreement require no doc edit.
- `docs/ARCHITECTURE.md` — module boundaries, data flow, deployment shape, with diagrams
  showing the connections between subsystems and the flow of information. Update when
  the shape changes.
- `docs/BACKLOG.md` — deferred bugs and ideas (see Scope). Each entry names its
  **revisit trigger** — the observed condition that would make it worth building. An
  entry that gets implemented or otherwise resolved is **removed in the same change** —
  git history is the archive; the file lists only what is still open.
- `docs/reviews/<YYYY-MM-DD>/AUDIT.md` — code/security audits; add `IMPLEMENTATION.md`
  when an audit yields substantial follow-up work, grouped by severity, each item with
  file, line, and fix sketch.

## Git

- Conventional-ish commits, one logical change per commit.
- **Development happens in a git worktree under `.claude/worktrees/`** (e.g.
  `.claude/worktrees/<branch>`, gitignored), one per build — the harness-blessed
  location, no permission prompts. The main checkout stays on `main`, clean, for
  discussion, assessment, planning and review. Worktrees stay disposable by discipline:
  commit at step boundaries — a worktree must never hold anything worth losing. Run
  `git worktree prune` after deleting one.
- Large work (plans) gets a branch; the branch **rebases onto main before the ff
  merge**, so main never freezes — small unrelated work keeps landing on main while a
  plan is in flight. Same-file collisions with an active step are the one case to wait
  out.
- Small work gets done on main directly.
- Commit at step boundaries; the suite is green at phase boundaries (see Plans).
- Never push, force-push, or rewrite history without being asked.
- **Tags are anchors.** Annotated semver tags on main, `v`-prefixed from `v0.7.4` on
  (`v0.7.4`; earlier tags have no `v`), tagged after notable merges and **always
  immediately before a large plan's implementation begins** — the tag names the world
  the plan started from, for diffing and for bailing out. The message says what the
  anchor holds in one line; for a release it becomes the GitHub Release's notes.
  Pushing a `vX.Y.Z` tag to `github` **is a release** (the `release` workflow builds,
  smoke-tests and publishes the images): push only when asked. `[PROJECT]`
- **Remotes** `[PROJECT]`: `origin` (private) holds `main`, every tag, and the
  `private` branch — the development history before the first public release (0.7.3).
  `github` (public) gets `main` and release tags from 0.7.3 on, nothing else: never
  push `private` or the tags before 0.7.3 there (they point into the private history),
  and never `git push --tags` to it.

## Deployability `[PROJECT]`

- The deliverables are **container images** (gateway; sample control plane for demos and
  validation) and the plain binary. Kubernetes manifests are **out of scope** — operators
  own their cluster setup (settled 2026-09-24). The image and binary must still be
  Kubernetes-ready: config via env, readiness/liveness on the admin port, draining on
  SIGTERM (see `docs/specs/GATEWAY.md`).
- Local run is first-class: `kaiak` with a config file and no other process; the
  sample control plane is one `npm` command away.
- Fixes for build or run issues land in the codebase or `scripts/`, never as notes
  about manual steps.
- **The gateway is stateless** (settled 2026-09-25; nothing on disk at all, settled
  2026-10-07): with only `KAIAK_CONTROL_URL` and `KAIAK_CONTROL_TOKEN` (plus backend
  API-key variables) it runs on a read-only filesystem with no volume and writes
  nothing, ever. Usage not yet delivered waits in memory, and the drain reserves its
  last seconds (`KAIAK_DRAIN_FLUSH_RESERVE_MS`) to deliver it; the `usage flushed` /
  `usage not flushed` log line says whether it did. A pod that dies undrained loses only
  that undelivered usage. The seed config (`KAIAK_SEED_CONFIG_FILE`, free models only)
  is the backup for a boot while the control plane is unavailable; with neither, the
  gateway exits and is restarted. File mode (a config file, no control plane) is for
  local and development use: its counts start from zero at every start.

## Feature-Building Mode (No Backwards Compatibility) `[PROJECT]`

Until this project ships to real users:

- No automatic upgrade paths between intentional versions, no dual-read of deprecated
  formats, no legacy-cleanup code — **not even for artifacts produced earlier in the
  same session**. New behavior targets the current format only; when replacing an
  approach leaves stale state behind, state the manual cleanup clearly (one `rm`, one
  restart) instead of encoding it.
- Fixing state produced by past bugs is normal engineering, not "migrations."
- There is no database, and the gateway writes no files: no stored state survives a
  version change, so there is nothing to migrate.
- The gateway ↔ control plane protocol carries a version; mismatched versions refuse to
  talk. Both halves move together — no support for older protocol versions.

Replace this section with a compatibility policy when the project graduates.

## Project-Specific Rules `[PROJECT]`

> Rules unique to this project as they emerge — domain naming conventions, architectural
> boundaries (e.g. "only `lib/db/` may import the database driver"), required error shapes.
> Generic principles belong in `docs/CODING-RULES.md`, not here.

- **One pipeline, no side doors.** Every client request passes the full request
  pipeline (auth → limits → routing → provider → accounting). No endpoint or fast path
  skips a stage; new cross-cutting behavior (caching, guardrails, prompt logging) is a
  new stage, never a special case inside a provider.
- **Only named code talks to backends.** In the gateway, only provider packages open
  a connection to a model server or cloud API. In `control/`, only `kaiak-control`'s
  `backend-verify` does, and only when the app calls it (`docs/specs/BACKEND-VERIFY.md`).
- **Passthrough preserves what it doesn't understand.** A request going to a backend
  that speaks its API (OpenAI, Messages, Responses) is forwarded with the minimal
  edits the gateway owns (model name, output-limit defaults/ceiling, usage reporting
  flags, service tier — `standard_only` on Anthropic — and `store: false` on
  Responses); unknown fields survive untouched.
- **No blocking control-plane I/O on the request path.** Usage and status reports are
  queued and sent in the background.
- **Nothing sensitive in logs, metrics or usage records**: no client keys (key IDs
  only), no provider secrets, no prompt or response content.
- **Protocol changes land on both sides at once**: `docs/specs/CONTROL-PROTOCOL.md`,
  `protocol/` schemas and fixtures, the gateway and `kaiak-control` — one change.
