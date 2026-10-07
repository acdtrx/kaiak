# Step 21 — "cannot tell which models a backend serves", said once

**Status:** not started

## Intent

A backend type that cannot list its deployments says so in one way, and the model check
lives with the provider knowledge it uses. Today it is a `listsModels` flag *and* an
always-true probe: `azure-anthropic` has both, `azure-openai` only the second, so its
model check runs, can never warn, and says nothing.

## Findings

- provider F4: `probe` returns `serves == nil` for "cannot tell" (both Azure modules);
  the model check logs its info line when `serves` is nil, after the probe answered (so
  `azure-openai` keeps its wrong-path warning); the circuit treats nil as served;
  `listsModels`, `ListsModels` and `NewModelChecker`'s second parameter go.
  **Behaviour change** (decision 10): `azure-openai` logs the info line.
- routing-limits F11: `ModelChecker` moves to `provider` (over the `*Registry`), calling
  `Probe` and `PathMissingError` directly; routing's mirrored `pathMissing` interface
  goes, routing keeps only `ProbeFunc`; `PathMissingError`'s fields and
  `BaseURLHint()` shrink to what is read.

## Files likely touched

- `gateway/internal/provider/{provider,azure_openai,azure_anthropic,probe}.go` and tests.
- `gateway/internal/routing/modelcheck.go` → `provider`, `circuit.go`, `circuit_test.go`.
- `gateway/cmd/kaiak/main.go` (or `controlplane.go`/the applier closure).
- `docs/specs/GATEWAY.md` → Providers: Probe and model check (the info line applies to
  `azure-openai` too), and `ARCHITECTURE.md` if it places the model check in routing.

## Decisions made during planning

- The boundary stays acyclic: `provider` does not import `routing`.

## Removal checklist (clean at phase end)

- `git grep -nE 'listsModels|ListsModels' gateway/` → none.
- `git ls-files gateway/internal/routing/modelcheck.go` → none.

## Acceptance criteria

- A test: an `azure-openai` backend's model check logs the info line and no warning; a
  wrong path still warns.
- Circuit and probe tests pass unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.
