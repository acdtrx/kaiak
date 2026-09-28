# Group-tree review — 2026-09-27 (at 0.7.0)

Scope: the 0.7.0 change that replaced the fixed owner model with a generic tree of
groups (`docs/plans/group-tree/`). Five read-only reviewers, each on its own detached
worktree at `0.7.0`:

- **[G]** gateway (config, limits, accounting, control, metrics, server);
- **[K]** `kaiak-control` and the sample;
- **[P]** contract parity between the halves (differential testing);
- **[S]** security and abuse;
- **[T]** test quality (mutation testing).

A plain copy for an independent review is at `/tmp/kaiak`; its findings, if any, join
this file as **[B]**.

Every finding is tagged by frequency under legitimate use: `daily` / `occasional` /
`rare` / `adversarial`. The key claims were re-checked against the code by the main
session (both carry-over loops, the missing tests, the stale comment, the plan's
verification box).

## Verdict

**Correctness holds.** No reviewer found a way for a key to escape a limit or a model
restriction on its path, and no group ID or label reaches a refusal, a log line or a
metric against the spec. The halves agree: [P] ran ~10 000 generated configs, 32
message variants and a totals round trip through both halves with zero differences;
[G] independently ran 463 configs with the same result. **Scale is where the work
is**: three quadratic spots and one multiplicative memory cost, all present before
0.7.0 but made the recommended shape by `child_defaults`. **The tests have real gaps**,
one of which contradicts a verification claim in the plan.

## Findings

### Scale and resources

| # | Finding | Freq | Found by |
|---|---|---|---|
| R1 | **Carry-over on reload is quadratic, under the lock.** Each new limit identity scans every counter (gateway `limits.go` `carryOver`, under `l.mu` in the first `Reserve` after a swap) or every dropped limit with two `JSON.stringify` per comparison (kit `aggregate.ts` `carryOvers`, inside the publish's totals turn, blocking the event loop). A `child_defaults` limit edit on a group with N children makes N new identities. Measured: gateway 1.9 s at 10k children, 36 s at 40k (all admission blocked); kit 6 s at 10k, 25 s at 20k (streams, acks, heartbeats stalled). Predates 0.7.0 (`default_user`). | occasional, high | G, K, S (×2) |
| R2 | **`child_defaults` multiplies memory with no bound.** D default limits × N children = D×N counters on every gateway. A 276 KB config (3000 × 3000) → 9 M counters, +12.7 GiB heap per gateway; the kit validates and publishes it (+454 MiB at 2000 × 2000), every gateway OOMs, restarts, refetches it — a fleet-wide crash loop (with a data directory, the last-known-good copy is the same config). Predates 0.7.0 (`default_user` × users). | rare (needs thousands of defaults), fleet-wide impact | S |
| R3 | **Kit tree validation is quadratic** on long chains and cycles (`tree.ts` `ancestryOf` per group, unmemoized): 9 s for a 20k chain, 18 s for a 20k cycle; the gateway's `tree()` memoizes and is linear. Only invalid configs. | adversarial (config author) | K, S |
| R4 | **Gateway `mergeLimits` is quadratic** (identity strings rebuilt per default × override pair; the kit uses a map): 10k × 10k on one child → 9.9 s per config apply on every gateway. | adversarial (config author) | S |
| R5 | **Totals messages have no size bound below the 16 MiB message cap.** Every ack carries the full totals (~115 B per window); past ~146k active hour/month windows (e.g. ~73k users each with two inherited defaults, all active) acks fail, batches retry forever, and after the outage grace USD-limited models answer `503 budget_unavailable` fleet-wide. Predates 0.7.0. | rare (very large deployments) | S |
| R6 | **A usage batch can exceed the 2 MiB body limit** and is then set aside uncounted (413 `request-invalid` is a batch refusal): only with backend model names made of `<`, `>`, `&`, `"` (escaped ×6 by Go's JSON encoder) — 1.11–1.30 of the limit; the group paths alone reach 0.69. The kit's comment "well under 2 KiB each" is stale. | rare | S |
| R7 | **Sample status page renders the tree quadratically** (`childrenOf` scans all scopes per node; key arrays copied per key): 0.6 s at 10k groups, on every page load and publish, on the event loop the protocol plugin shares. | occasional (large trees), sample only | K, S |

### Test gaps (mutation-proven: the mutation left every suite green)

| # | Gap | Severity | Found by |
|---|---|---|---|
| T1 | **The gateway never tests settling a request whose group was deleted mid-flight** (usage must still reach the surviving ancestors). Mutation: skip settlement when a held counter was dropped — all green. **The plan's verification box claims this is covered by gateway counters (STEP-4); it is not** — only the kit test exists. | high | T (confirmed) |
| T2 | **The kit's aggregation never sees a path deeper than 2** (`usage.test.ts` and the cross-half e2e use two-level paths). Mutation: count only the first and last listed groups — all green. | high | T |
| T3 | **Refusal text is pinned for only two messages.** Adding the group ID to the tokens, "request too large" and `budget_unavailable` messages — all green (the only token refusal tested is global, whose ID is empty). | medium | T |
| T4 | **"global limit" is never asserted** — no test contains the string; forcing every refusal to say "group limit" stays green. | low–medium | T (confirmed) |
| T5 | **The gateway's carry-over is not pinned to one group** (dropping the group condition stays green; the kit's twin is covered). | medium | T |
| T6 | **The spool format bump is not pinned** (the discard test writes `spoolFormat-1`, which moves with the constant; no stale batch file is tested). | low | T |
| T7 | Fixtures: schema-kind invalid fixtures don't pin the failing path in either half; resolution fixtures lack `child_defaults.allowed_models: []` and a 10-level chain. | low | T |

Mutations that **were** caught (limits skipping a level, records naming only the last
group, `root_group`, `group_label`, `child_defaults` merge/override/models,
intersection, `["*"]`, counter identity, file and cache versions, pushed window group,
`group-parent-changed`, kit counting and carry-over, sample tree and totals) are
listed in [T]'s report; coverage of the settled decisions is otherwise good.

### Semantics and docs

| # | Finding | Freq | Found by |
|---|---|---|---|
| S1 | **A group deleted and re-created with the same ID resumes its current-window spend** — under its new parent, since windows are keyed by group ID. The spec's general rule allows it ("a limit that returns within the same window comes back with what was counted"); `GUIDE.md` says the opposite ("windows of a deleted group are no longer read"), and "a move is a delete and a create" reads as a fresh start. Both halves agree. | rare | K, P, S |
| S2 | **`child_defaults` is a default, not a ceiling**: a child's own `allowed_models` or same-identity limit replaces the default whole, so it can loosen it; only the parent's own lists and limits bind the subtree. Intended (decision 6), but the spec's `users` example invites using defaults as policy without saying hard restrictions belong on the parent. | occasional (config authors) | S |
| S3 | **Carry-over compares with a different "previous" config per half** when a gateway skips a version (it carries against the config it last held; the kit against the previous published one). Self-corrects with the next totals; the spec doesn't say which. | rare | P |
| S4 | **The usage metric label `group` collides with a common Prometheus target label**: with `honor_labels: false` it is renamed `exported_group` and dashboards silently find nothing. | occasional | G |
| S5 | **`root_group` bounds series only when top-level groups are few** (users as top-level groups gain nothing from `group_label: false`); documented only implicitly. | rare | S |
| S6 | `resolveScopes` "config order" is object-key order: integer-like group IDs come first. Cosmetic (the sample's sibling order). | rare | K |

### Minor and by design (recorded only)

- Refusal bodies and `x-ratelimit-*` show the refusing limit's value and use — with
  deep trees a leaf key reads an ancestor's aggregate (e.g. company-wide spend). No ID
  or label leaks; the shape predates 0.7.0 (team scope). [S]
- Sample page: labels (possibly cost centers or emails) now appear on the
  unauthenticated page, whose code says it "holds nothing secret"; bidi and zero-width
  characters pass the label pattern (UI spoofing in control-plane UIs). [S]
- Keygen `--group` doesn't check the ID's shape, so a typo fails only after the key
  was shown once (0.6.1 didn't check either). [K]
- The sample keeps the last of a repeated member in its hand-edited config file
  (`JSON.parse`); the gateway in file mode refuses the same file. Settled decision for
  documents the kit sends; this file is hand-written. [P]
- A group may be named `global`; totals work, one log field becomes ambiguous
  (`limit_scope` disambiguates). [P]
- Forged records (token holder) may list any 1–8 existing groups; no new power — the
  token holder could already forge usage. [P, S]
- A request admitted across a reload that deletes a group skips that group's limits
  once (path from the request's snapshot, counters from the live one). Negligible. [S]

## Proposed triage (awaiting the user)

- **Implement**: R1 (occasional, blocks traffic), R2 (a bound in both halves — fleet
  crash loop), R3 and R4 (cheap, same code), T1–T6 (and correct the plan's
  verification box for T1), S1–S5 as spec/GUIDE/DEPLOYMENT wording, the stale comment
  (R6), keygen's group check (cheap).
- **Decisions needed**: S1 keep resume-on-reuse (document "use a new ID for a fresh
  budget") or drop a deleted group's windows at publish; S4 document the collision or
  rename the label (e.g. `key_group`); R2 the shape and value of the bound.
- **Backlog**: R5 (trigger: totals messages above a few MiB, or active windows in the
  tens of thousands), R6's real fix (seal batches by encoded size; trigger: a batch set
  aside for size), R7 (trigger: a sample used with thousands of groups).
- **Recorded only**: the minor list above.
