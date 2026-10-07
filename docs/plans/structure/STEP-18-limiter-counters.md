# Step 18 — the limiter's counters

**Status:** not started

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
