# Step 4 — e2e and docs

**Status:** done (2026-10-02)

## Intent

Prove the unit end to end and bring the operator docs to it. This step ends the
phase: the full suite is green.

## Files likely touched

- `gateway/e2e/e2e_test.go`: a model pricing `tokens_cache_write`; the fake backend
  reports writes on a non-stream and a stream request; each settles at the write
  price; the log line and `kaiak_usage_tokens_total` carry the unit; a request whose
  written tokens take it over a tier threshold settles at the upper tier.
- The cross-half e2e: a record with writes reaches the sample control plane, and its
  tokens count toward a `tokens_per_hour` total.
- `docs/DEPLOYMENT.md`: Azure → Prices — price `tokens_cache_write` for gpt-5.6 and
  later (1.25× input), with the example entry.
- `docs/architecture/gateway.html`, `docs/architecture/control-plane.html`: units,
  `Kaiak-Protocol: 4`.
- `docs/kaiak.md`, `README.md`: anywhere they list units or name the versions.
- `docs/BACKLOG.md`: remove the *Cache-write usage unit* entry (resolved).

## Acceptance criteria

- `scripts/check-all.sh` green three times in a row with the Go test cache cleared,
  output recorded here.
- Repo-wide grep, outside `docs/plans` and `docs/reviews`: every list of token units
  names five (or says why not); protocol 3 and config format 3 remain only in the
  old-version cases.
- Status recorded in every step file and the overview's verification status; the
  phase is committed.

## Result

**What changed**

- `gateway/e2e/e2e_test.go`: `TestInputWrittenToTheCache` — the file-mode gateway
  with the e2e config plus two models: `written`, tier 0 (`tokens_in` 1,
  `tokens_cached` 0.1, `tokens_cache_write` 1.25, `tokens_out` 2 USD per million) and
  a tier above 100 input tokens (2, 0.2, 2.5, 4); `write-unpriced`, one tier with no
  write price (1, 0.1, —, 2). The fake backend reports each request's usage (10
  tokens out); the log line must carry the five units, `estimated=false` and
  `cost_usd`:
  - 60 prompt, 40 written, non-stream → 0.00009 (20×1 + 40×1.25 + 10×2);
  - 60 prompt, 10 read, 40 written, streamed (usage chunk withheld from the client)
    → 0.000081 (10×1 + 10×0.1 + 40×1.25 + 10×2);
  - 101 prompt, 41 written → 0.0002625, the upper tier (60×2 + 41×2.5 + 10×4): only
    the written tokens take the input size past 100;
  - `write-unpriced`, 60 prompt, 40 written → 0.00008 (writes at `tokens_in`: 20×1 +
    40×1 + 10×2);
  - metrics: `kaiak_usage_tokens_total{…model="written",…,unit="tokens_cache_write"}`
    121, `unit="tokens_cached"` 10, `write-unpriced`'s written 40, and
    `kaiak_usage_cost_usd_total` for `written` 0.0004335.
- `gateway/e2e/sample_test.go` (cross-half): the subtest *input read from and written
  to the cache counts toward the token total* — the backend reports 2036 prompt
  tokens (3 plain, 1024 read, 1009 written) and 40 out on one request through gw-a;
  the sample's global `tokens_per_hour` total must equal every answer's
  `total_tokens` served so far, now including 2076 for it. The backend's reply is
  reset after it.
- Mutation checks, each restored after (`git diff` clean on the mutated file):
  - `inputSize` without `tokens_cache_write` → *written over the threshold* fails
    (0.00013125, the lower tier) and the cost metric (0.00030225);
  - `cacheInputPrice` falling back to 0 instead of `tokens_in` → *written unpriced*
    fails (0.00004);
  - kaiak-control's `amountFor` without `tokens_cache_write` → the cross-half subtest
    fails (the hourly total short of 2351), and every later total check with it.
- Docs:
  - `DEPLOYMENT.md` Azure → Prices: price `tokens_cache_write` for gpt-5.6 and later
    (about 1.25× input, reads 0.1×; left out → the tier's `tokens_in`, about 25%
    under; written tokens count toward the tier's input size), a two-tier example
    (the shape of `config/valid/price-cache-write.json`).
  - `docs/testing/LIVE-BACKENDS.md` (Azure, assumption 5): the usage fields name
    `prompt_tokens_details.cache_write_tokens`; a fresh long prompt sent twice shows
    `tokens_cache_write` on the first log line and `tokens_cached` on the second.
  - `docs/architecture/gateway.html`: the units list (five, what each input unit is,
    the three add up to the prompt), the tier's input size, the unpriced rule; the
    seams table's units; footer. `docs/architecture/control-plane.html`:
    `Kaiak-Protocol: 4` (callout and diagram); footer.
  - `README.md`: config format and protocol at version 4. `docs/kaiak.md`: no
    change — it lists no units and names no version.
  - `docs/BACKLOG.md`: the *Cache-write usage unit* entry removed; the *Bedrock
    provider* entry keeps its one Bedrock-specific point (`cacheWriteInputTokens` →
    `tokens_cache_write`; Bedrock prices writes by cache lifetime, which the one unit
    does not split — the spec's rejected alternative).
- Repo-wide check, outside `docs/plans` and `docs/reviews`:
  - Token-unit lists: every list names the five units — specs, schemas, GUIDE,
    `types.ts`, the architecture pages, the gateway's unit constants, schema check,
    meter, limits sum, log line, live kit. Not five, deliberately: the price-unit
    lists (four — `tokens_reasoning` is never priced); input-size sums (three input
    units); token-limit sums (four, reasoning inside `tokens_out`); the format-1 and
    format-2 spool records in `internal/control/usage_test.go` (four, the old
    shapes); the vLLM note in `DEPLOYMENT.md` (cached tokens only — vLLM reports no
    writes). `sections.ts`'s "all four are zero" counts serving states, not units.
  - Config format 3 remains only in `config/invalid/format-version-3.json` and the
    discarded last-known-good file in `internal/control/seed_test.go`; protocol 3 only
    in `messages/status/invalid/protocol-version-3.json` and the mismatch tests;
    "format version 3" in `GATEWAY.md` is the new usage-spool format. The GUIDE header
    names format 3 / protocol 3 as its 2026-09-29 history.

**Decisions made during the step**

- A new e2e test beside `TestTieredPrices` rather than more cases in it: the write
  price, the unpriced rule and the metric need their own models.
- The cross-half check rides the existing exact token-total check (`allCounted`):
  a sum that missed written tokens comes up 1009 short, as the mutation showed.
- The Bedrock entry gains one sentence so the removed entry's Bedrock point is not
  lost.

**Suite** — `scripts/check-all.sh` three times in a row, the Go test cache cleared
before each:

all three passed (152 s, 145 s, 148 s).

- Gateway: gofmt, vet, staticcheck; `go test -race ./...` every package ok (511
  top-level tests; e2e 96.5 s, 94.9 s, 92.6 s); live-test kit lint and self-test pass
  (vllm, llama-server, openai, azure-openai, vllm with two backends).
- `control npm test`: 574 pass, 0 fail, each run. `npm run lint`: `tsc` clean,
  `boundaries ok`.
- Cross-half e2e: ok (48.9 s, 43.8 s, 48.9 s).
- The phase is green; no expected reds remain. Live Azure check pending (OVERVIEW).
