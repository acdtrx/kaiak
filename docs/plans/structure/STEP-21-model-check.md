# Step 21 — "cannot tell which models a backend serves", said once

**Status:** done (2026-10-08)

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

## Result

**What changed**

- **F4 — "cannot tell" is `serves == nil`.**
  - `azure-openai`'s probe still fetches its models list (so a `404` stays a wrong-path
    error) and returns `nil` serves. `azure-anthropic`'s probe sends nothing and
    returns `nil, nil`.
  - `backendKind.listsModels` and `provider.ListsModels` are gone.
  - The model check probes every backend. When the probe answered and `serves` is
    nil, it logs the info line `model check not available for this backend type`
    (`kaiak.backend.id`, `kaiak.backend.type`). The wrong-path and did-not-answer
    warnings come first, so `azure-openai` keeps its wrong-path warning.
  - The circuit (`routing.ProbeNow`) treats nil `serves` as served:
    `serves != nil && !serves(key.Model)`. `ProbeFunc`'s and `ProbeNow`'s comments
    say so.
  - **Behaviour change (decision 10):** `azure-openai`'s model check now logs the info
    line, as `azure-anthropic`'s does.
- **F11 — `ModelChecker` lives in `provider`.**
  - `routing/modelcheck.go` → `provider/modelcheck.go` (`git mv`, staged).
    `NewModelChecker(r *Registry, logger)` takes the registry; the second parameter
    is gone.
  - It reads the 404 with `errors.AsType[*pathMissingError]`. routing's mirrored
    `pathMissing` interface is gone; routing keeps only `ProbeFunc` for its circuits.
  - `PathMissingError` → `pathMissingError{backend, url, hint}`. Nothing outside
    `provider` reads it any more, so the type, its fields and `BaseURLHint()` (now
    the `hint` field) are all unexported.
  - `go list -deps ./internal/provider` names no `routing`: the boundary stays
    acyclic.
- **Wiring.** `cmd/kaiak/main.go` `newGraph`:
  `g.modelChecker = provider.NewModelChecker(g.providers, logger)`; the graph field is
  `*provider.ModelChecker`. The applier hook is unchanged.
- **Docs.**
  - `docs/specs/GATEWAY.md` → Providers → Probe and model check: the info line covers
    every backend whose probe cannot tell, `azure-openai` included (settled
    2026-10-08); an `azure-openai` `404` still warns. The log-attribute table row for
    `kaiak.backend.type` names both Azure types.
  - `docs/ARCHITECTURE.md`: the `ModelChecker` sentence moved from `routing` to
    `provider`. The routing paragraph says a probe half-opens a circuit when it lists
    the model "or [is] unable to tell".
  - `docs/architecture/gateway.html`: the `provider` row of the package table names the
    config-time model check. The Routing and reliability bullet describes behaviour,
    not a package, so it stays.
  - `docs/DEPLOYMENT.md` (Azure OpenAI): one sentence on the info line and the
    wrong-path warning.
- **Tests.**
  - `TestModelCheckWarnsPerMissingModel` moved to `provider/modelcheck_test.go` with
    its assertions unchanged. Its fake probe goes through the unexported
    `newModelChecker`, and its 404 is a real `*pathMissingError`, which replaces the
    `hintedError` stand-in. `logBuffer` moved with it, and `checkModel` and
    `checkSnapshot` replace routing's `queuedModel` and `circuitSnapshot` here.
  - `TestModelCheckSkipsTypesWithoutAModelsList` → `TestModelCheckSaysWhenTheBackendCannotTell`
    (provider). It runs the real `NewModelChecker` over a real `Registry` and an
    `httptest` server and asserts:
    - the info line for an `azure-openai` backend (new) and for `azure-anthropic` (kept);
    - no models warning for either;
    - the wrong-path warning, with `azurePathHint`, for an `azure-openai` backend
      whose `base_url` ends in `/v1`.
    The old "azure-anthropic is not probed" assertion went: it asserted removed
    behaviour (decision 16). Its probe still sends no request, which
    `TestAnthropicTypesProbe` and the backend-type fixture test check.
  - New `TestProbeThatCannotTellHalfOpens` (routing): a probe answering `nil, nil`
    half-opens the circuit, and the trial is admitted.
  - Probe tests now assert the new contract (nil `serves`) where they asserted
    always-true `serves` for the Azure types:
    - `TestModuleURLCredentialAndProbe` (azure-openai case);
    - `TestProbeReportsTheListedModels` (azure);
    - `TestAnthropicTypesProbe` (azure-anthropic; the `ListsModels` assertion went with
      `ListsModels`).
    The non-Azure assertions are unchanged.
  - `backendtypes_test.go` (step 4): the `ListsModels` agreement became "a fixture with
    no `models_list` ⇒ the probe returns nil `serves` and sends no request". The
    converse does not hold (`azure-openai` has a list and cannot tell), and the fixture
    does not say it, so the test does not check it. No protocol fixture changed.
  - `TestProbeOfAMissingModelsList` reads the unexported fields; same assertions.

**Decisions made during the step**

- **The checker keeps a probe function field**, set to `Registry.Probe` by
  `NewModelChecker`. It does not call a stored `*Registry`. This lets the moved test
  keep its scripted probe and its exact log assertions (decision 17). Real probes
  would word "down" as `backend down: …` and give a different hint and `base_url`.
  The new cannot-tell test goes through the real constructor and registry.
- **`pathMissingError` is unexported whole**, not only its fields: after the move
  nothing outside `provider` names it.
- **The info line comes after the probe** for both Azure types. `azure-anthropic`'s
  probe makes no request, so this costs nothing, and there is one rule: probe, then
  speak.

**Report vs code**

- F4 places `listsModels` at `provider.go:310-325` and `ListsModels` at `:346`. They
  were at `:319-321` and `:359-363` after step 20. The shapes were as reported.
- F11 says to move "its two tests from circuit_test.go". Both were there, with
  `logBuffer` and `hintedError`. One moved as is; the other was rewritten for the new
  behaviour (above).
- The provider review's hint holds: `PathMissingError`'s exported fields and
  `BaseURLHint()` were read only by routing's mirror and provider's own test.

**Test counts** (top-level tests from `go test -list`; in brackets, passes including
subtests)

- `internal/provider`: 55 (202) → 57 (204). Moved: `TestModelCheckWarnsPerMissingModel`.
  New: `TestModelCheckSaysWhenTheBackendCannotTell`.
- `internal/routing`: 49 (52) → 48 (51). The two model-check tests left; added
  `TestProbeThatCannotTellHalfOpens`. The circuit tests are unchanged.
- `cmd/kaiak`: 28 (32) → 28 (32).

**Removal checklist**

- `git grep --untracked -nE 'listsModels|ListsModels' gateway/` → none.
- `git ls-files gateway/internal/routing/modelcheck.go` → none (the `git mv` is
  staged).
- Also clean, repo-wide outside `docs/reviews` and `docs/plans`:
  `PathMissingError|BaseURLHint|hintedError|routing\.ModelChecker|pathMissing\b`.
  `NewModelChecker` appears only in `provider` and `cmd/kaiak`.

**Suite**: `scripts/check-gateway.sh` passed (exit 0).

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (110s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
