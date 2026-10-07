# Structure review — what to look for

This is a **structural** review, not a bug or security audit (several of those already
ran and their fixes are merged). The code grew through many sequential feature plans
(group tree, tiered pricing, backend types, cache-write units, OTel log export,
Messages/Responses passthrough, control-plane replicas, stateless gateway). The question
for every piece of code: **knowing everything it does today, would we design it this
way?** Find the places where incremental change left a shape that a redesign from the
current state would make simpler, smaller or more general.

Read first: `docs/kaiak.md` (philosophy — binds design), `AGENTS.md` (project rules —
hard constraints), `docs/CODING-RULES.md`, `docs/ARCHITECTURE.md`, and the spec for the
code you review (`docs/specs/GATEWAY.md`, `CONTROL-PROTOCOL.md`, `BACKEND-VERIFY.md`).

## Inside a function

- Functions grown a flag/branch per feature: boolean parameters, special-case `if`s, a
  type `switch` that is repeated elsewhere, long parameter lists.
- Parameters threaded through several layers only to reach one consumer.
- Leftover guards/branches for states that can no longer occur after a later change.
- Two stacked mechanisms for one concern, where fixing the mechanism once would do.
- Logic whose shape reflects the order features arrived in, not the problem.

## Across functions / files / packages

- Near-duplicate logic that drifted (per provider, per API protocol, per limit kind, per
  window, gateway vs control).
- "Shotgun" concepts: adding one thing (a backend type, a usage field, a limit kind, a
  price unit, a protocol message) requires touching N places in lockstep — count them.
- The same state held in two places and kept in sync by hand.
- Grab-bag files/packages holding several unrelated jobs; or one job split across
  several places.
- Wrapper chains, single-implementation interfaces, indirection that no longer earns
  its keep; boundaries that no longer match how data flows.
- Types that mirror other types field by field (conversion layers) without a reason.

## The bar for a finding

- A finding must **remove code, remove a concept, reduce the places to touch for a
  likely future change, or put a responsibility where it belongs**. Naming, formatting,
  comment wording and personal taste are not findings.
- Generalisation needs **at least two real, current uses** — no speculative
  abstraction (YAGNI and "features earn their place" are project rules).
- Proposals must respect the hard constraints in `AGENTS.md`: zero third-party Go
  dependencies, one request pipeline with no side doors, only provider packages talk to
  backends, passthrough preserves unknown fields, no blocking control-plane I/O on the
  request path, gateway stateless, protocol changes land on both sides at once.
- Be honest about cost. A small finding with a clear payoff beats a grand rewrite.
  "Leave as is" is a valid conclusion for a module — say so if it is clean.
- Bugs you stumble on: note them in a separate short section, don't hunt for them.

## Finding format

For each finding:

- **ID / title** — one line.
- **Kind** — in-function | cross-function | cross-module.
- **Where** — `path:line` and the function/type names involved.
- **Now** — the current shape, concretely (quote short snippets if it helps).
- **How it got here** — the incremental path that likely produced it (from the code, and
  from git history where available).
- **Proposed shape** — what it would look like redesigned; concrete enough to judge.
- **Payoff** — lines/concepts removed, places-to-touch before → after for the named
  future change.
- **Cost / risk** — size of the change, tests affected, contracts touched (spec,
  protocol schema, public `kaiak-control` API).
- **Confidence** — high | medium | low, with what would raise it.
