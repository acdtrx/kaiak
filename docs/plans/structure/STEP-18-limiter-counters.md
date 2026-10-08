# Step 18 — the limiter's counters

**Status:** done (2026-10-08)

## Intent

A scope's count lives in one place in the limiter, and a counter carries exactly what it
needs. Today a control-plane count lives in the `pushed` map (with its own prune clock)
and is copied by hand into its counter's window, a deleted scope's own count lives in
`retained`, two survival rules govern them, and every counter keeps a whole
`config.Limit` — a placeholder one for unlimited counters — recomputing the nano-USD
limit each time it is needed.

## Findings

- routing-limits F8: one store of counts by key. A pushed window for a scope with no
  counter creates a retained counter with that base; `TakeTotals` sets bases on
  `counters ∪ retained`; complete totals zero the bases they do not list. A retained
  counter is pruned when it has no refs, no own usage and no base for its current
  window. `pushed`, `prunedAt`, `prunePushedLocked` and `applyLimit`'s base branch go.
- routing-limits F6: `counter{key, measure, limited bool, max int64}`, `max` set once in
  `sync`; `Type` read from `c.key.typ`; one `c.rejection(…)` helper for the two
  `Rejection` literals in `Reserve`.

## Files likely touched

- `gateway/internal/limits/{limits,shared,window}.go`, `shared_test.go`, `limits_test.go`.

## Decisions made during planning

- `shared_test.go` stays valid unchanged in what it asserts (decision 17). Before the
  rewrite, read its totals-before-config and complete-totals-after-delete cases against
  the new rule and write down, in the Result, why each still holds.
- The spec's "windows of scopes the config does not have are kept for a reload that adds
  them" still holds; no spec edit.

## Removal checklist (clean at phase end)

- `git grep -nP '\.pushed\b|prunedAt|prunePushedLocked|effectiveLimit' gateway/internal/limits` → none.

## Acceptance criteria

- Limits tests (`shared_test.go`, `limits_test.go`), server limit tests and e2e
  shared/limits scenarios pass with assertions unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.

## Result

**`shared_test.go` against the one-store rule** (read before the rewrite)

- **Totals before config** (`TestTotalsMatching`'s `gone` window, pushed while the
  config lacks the group; `TestTotalsApplyWhateverTheConfig`): a window for a scope
  with no counter creates a retained counter carrying the base. `TakeTotals`' prune
  keeps it, because its current window holds a base. The reload that adds the group
  re-adopts it in `sync`'s `take()` (it already looked in `retained`), so it reads 70.
  The same holds for totals taken before any snapshot is applied: every window becomes
  a retained counter, and the first `sync` takes them up.
- **Complete totals after delete** (`TestARecreatedGroupKeepsItsSpend` and
  `TestARunningRequestHoldsTheCountsOfADeletedGroup`, `shared=true`;
  `TestTotalsMatching`'s last block): complete totals zero the bases on
  `counters ∪ retained`, which matches the old "not listed since the last complete
  totals → base 0". Before, a retained counter's base went stale and was overwritten
  from `pushed` at re-adoption. Now it is current all along. Observably the same:
  only a live counter's count is ever read or enforced.
- **Reload adds the scope**: `take()` re-adopts the retained counter with its base and
  window ("for free", verified by the `gone` case and by the new test below).
- **Retained counts**: the prune rule becomes `refs == 0 && usedAt(now) == 0`, meaning
  no request holds the count and its current window holds no own usage and no base.
  In file mode the base is always 0, so the rule is unchanged there. In control-plane
  mode, a deleted scope that has a base but no own usage used to be pruned from
  `retained` while `pushed` kept its window. Now the counter stays, and both cases
  end when the window ends.
- `TestEndedPushedWindowsAreDropped` read `l.pushed` directly. Its three assertions now
  read `l.retained`, the store the windows live in, with the same keys, times and
  expectations: an ended hour window gone, a current month window kept, an ended month
  window gone.

**What changed**

- `limits.go`: `counter{key, measure, limited, max, w, refs}`. `max` is set once in
  `sync` (`maxOf(lim)`, the configured value in the counter's unit), and `Type` is
  read from `c.key.typ`. The placeholder `config.Limit{Type: typ}` is gone.
  `admits`, `blockedByRunning` and `warnSmallSharesLocked` read `c.max`.
  `c.rejection()` fills a refusal's identity and limits; `Reserve`'s two literals use
  it, adding `Unavailable`, or `Used`/`Requested`/`RetryAfter`. `applyLimit` only
  sets the enforced limit (`max`, or a per-minute share). Its base branch and the
  hand copy from `pushed` are gone.
- One store of counts: `Limiter.pushed` is gone, and so is `prunePushedLocked`.
  `housekeepLocked` prunes only the retained counts. `retained` now also holds counts
  that pushed windows created for scopes the config does not have.
- `shared.go`: `TakeTotals` re-shares the per-minute counters only when the live count
  changes. Complete totals zero the bases on `counters ∪ retained`. Each window sets
  its base on `pushedCounterLocked(k)`, which returns the live counter, else the
  retained one, else a new retained counter.
- `window.go`: `window.pushed()` renamed `currentBase()`. The checklist's `\.pushed\b`
  matched it, and the new name says what it returns.
- `shared_test.go`: `TestEndedPushedWindowsAreDropped` reads `l.retained` (above). New
  test `TestCompleteTotalsZeroADeletedGroupsBase`: a deleted group keeps its pushed
  base for a re-creation (400), and complete totals that do not list it zero it (0).

**Decisions made during the step**

- **Prune on "the window counts nothing"** (`usedAt(now) == 0`), not "no base for the
  current window" read as `baseStart != start`. A base zeroed by complete totals or
  pushed as "0" holds nothing a re-created scope needs: a fresh counter also starts
  at 0. Keeping such counters until their window ends would only hold memory.
- **No guard against per-minute types in `TakeTotals`.** The control client admits
  only counted types as totals windows (`control/schema.go` `windowTypes`), as the
  old `prunePushedLocked` already assumed (`kindOf` panics on an unknown window).
  `pushedCounterLocked`'s comment says so.
- **Complete totals zero every counter's `base`**, per-minute ones included. Their
  base is never read (only shared windows count it), so this needs no type check.
- **One added test** (`TestCompleteTotalsZeroADeletedGroupsBase`). Mutation checks
  showed that the retained branch of the complete-totals zeroing was pinned by no
  test, and the old code's equivalent (`pushed` replaced on complete totals) was not
  pinned either. The other two new paths are pinned already: without creating a
  retained counter for an unknown scope, or with the old prune rule ignoring the base,
  `TestTotalsMatching` and `TestEndedPushedWindowsAreDropped` fail.
- No spec edit, as planned. "Windows of scopes the config does not have are kept for a
  reload that adds them" and "Pushed windows are dropped once their window has
  passed" still hold.

**Report vs code**

- F8: as reported. `prunedAt` was already gone (step 6's single `housekeptAt`
  clock). The old code also never updated retained counters' bases on totals: they
  were set from `pushed` only at re-adoption.
- F6: as reported. `effectiveLimit` had 7 uses (`applyLimit`, `admits`,
  `blockedByRunning`, `Reserve` ×2, `warnSmallSharesLocked` ×2), and the placeholder
  `Limit` was set in 2 places in `sync`.

**Tests** (`go test -count=1 -v`, `--- PASS`, subtests included; before → after, no
skips, no fails)

- `internal/limits` 85 → 86 (+1: the new test); `internal/server` 457 → 457;
  `cmd/kaiak` 32 → 32. `go test -race -count=5 ./internal/limits/`: ok. No existing
  assertion changed. The e2e shared/limits scenarios (`TestSharedLimitsAcrossGateways`,
  `TestRejectedConfigKeepsBudgetsEnforcedFromStreamTotals`,
  `TestReadinessWaitsForTheFirstTotals`,
  `TestFirstTotalsLateRefuseBudgetsUntilTheyArrive`, the key-limit tests) passed
  unchanged in the suite.

**Removal checklist** (`git grep -nP`)

- `\.pushed\b|prunedAt|prunePushedLocked|effectiveLimit` in `gateway/internal/limits`
  → none.
- Also: `c\.limit\b|config\.Limit\{Type` in `gateway/internal/limits` → only test-local
  table fields (`limits_test.go:506,547`, `shared_test.go` share table).

**Production lines** (`limits/` non-test: `limits.go`, `shared.go`, `window.go`,
`headers.go`): 1362 → 1349.

**Suite**: `scripts/check-gateway.sh` passed (exit 0) on the code before the new test
was added. The test only adds to `shared_test.go`, and afterwards `go vet`, `gofmt`
and `go test -race -count=5 ./internal/limits/` passed.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (106s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
