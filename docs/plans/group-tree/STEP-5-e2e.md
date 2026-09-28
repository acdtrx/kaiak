# Step 5 — end to end

**Status:** done (2026-09-27)

## Intent

The whole system runs on the group tree: the gateway e2e suite, the cross-half e2e
(sample + two gateways), the examples, the live kit's generated configs and the image
smoke script. Ends Phase 1 green.

## Files likely touched

- `gateway/e2e/*_test.go` — generated configs and assertions on owners, records,
  totals, metrics, log lines.
- `gateway/e2e/sample_test.go`, `proxy_test.go` — cross-half e2e (`crosshalf` tag).
- `gateway/internal/fakecontrol/` — scripted totals by group.
- `examples/config.json`, `examples/local-config.json` — rewritten: a team with
  projects and prod/dev envs, workloads under them, and a `users` group with
  `child_defaults` holding what `default_user` held.
- `scripts/live/config.go` — generated configs.
- `scripts/smoke-images.sh` — its config and keygen call.

## Decisions for this step

- New e2e: a 4-level tree through the real binary — env limit refuses while the
  project has room; project limit shared by prod and dev; usage records carry the
  path; in control-plane mode, totals for a group applied on a second gateway.

## Acceptance criteria

- `scripts/check-all.sh` fully green (gateway checks, control tests and lint,
  cross-half e2e). Output recorded.
- Phase 1 committed and green.

## Result

**What changed**

- `examples/config.json` — format 2. Team `search` → project `search-rag` (the
  models list, a tokens-per-minute limit shared by both envs) → envs
  `search-rag-prod` / `search-rag-dev` (labels `kind`, `env`; dev narrows the models
  and has its own limits) → workloads `rag-service` / `rag-service-dev`; team
  `research` → project `research-eval` (the GPT budget) → workload `eval-batch` (no
  restriction on its path = every model); group `users` whose `child_defaults` hold
  what `global.default_user` held, with `alice` (override: every model, own budget)
  and `bob` (the defaults). Keys by `group`, one new key `k-rag-service-dev`;
  placeholder hashes as before. Backends, models and prices unchanged.
- `examples/local-config.json` — format 2: `demo-team` → `demo-app` (the group the
  README quick start mints a key for), `users` with `child_defaults` and `me`.
- `gateway/e2e` — every config on format 2 (`testConfig`: `research` → `eval`,
  `users` → `ann`; `reliabilityConfig`: `t` → `w`; `freeConfig` narrows through
  `users.child_defaults`); totals windows without `scope` (global = no `group`);
  usage metric labels `group` / `root_group`; the refusal log fields
  `limit_scope: group`, `group`; rejection scope kind `group`. `fakecontrol` needed
  no change (its windows are opaque JSON).
- Cross-half e2e (`sample_test.go`): the USD budget moves from global to the team
  `research`, the parent of the key's group — the two-gateway budget test now
  proves a group's totals: spend on gw-a is counted by the sample toward
  `research` (window `group: "research"`), and gw-b refuses on it. `used()` takes the
  group; the hourly token check reads the global window by `group == ""`.
  `proxy_test.go` needed no change.
- `scripts/live/config.go` — generated config on format 2: one top-level group
  `live` (every model, 1 request a minute on `live-rpm`) holding `k-live`.
  `docs/testing/LIVE-BACKENDS.md` wording to match (two lines).
- `scripts/smoke-images.sh` — keygen `--group demo-app`. Checked without Docker:
  the script's keygen + `sed` pipeline run locally on `examples/local-config.json`,
  the result passes `validateConfig` (kaiak-control) and boots the gateway binary
  in file mode (`config applied`).
- `control/sample/src/keygen/keygen.test.ts` — the paste-in test is back on
  `examples/config.json` (step 3 had moved it to a fixture while the example was
  format 1), with `--group alice` as the README recipe.
- `README.md` — the sentence describing what `examples/config.json` holds names the
  group tree.

**New e2e test** — `gateway/e2e/grouptree_test.go`, `TestGroupTreeEndToEnd`: the
real binary in control-plane mode (fakecontrol), tree `acme` → `acme-rag` →
`acme-rag-prod` / `acme-rag-dev` → `rag-api` / `rag-sandbox`, plus `acme-search` →
`search-api`:

- an env limit refuses while its project has room: prod's 2 `chat` a minute refuse
  the third (429, "group limit", no group ID or label in the body; log
  `limit_scope=group`, `limit_id=acme-rag-prod`, `group=rag-api`), dev under the same
  project still serves;
- a project limit is shared by its envs: the project's 3 `rpm` a minute are used by
  2 prod + 1 dev; the next from either env is refused with `limit_id=acme-rag`;
  the sibling project serves; `kaiak_limit_rejections_total{scope_kind="group"}` = 3;
- usage records carry the path: `groups` = `[acme, acme-rag, acme-rag-prod, rag-api]`,
  `[acme, acme-rag, acme-rag-dev, rag-sandbox]`, `[acme, acme-search, search-api]`;
  usage metrics carry `group` and `root_group="acme"`;
- totals for a group apply to that group's keys alone: a pushed window
  `group: "acme-rag"` over the spent budget refuses `priced` for both envs' keys
  (budget_exceeded, `limit_id=acme-rag`) — neither key had spent anything locally —
  while `search-api` serves it;
- a label value (`cost_center`) never appears in a refusal or the gateway log.

The second-gateway half of "totals for a group applied" is the cross-half test
above, with the real sample aggregating toward the ancestor group.

**Suite** — `GOFLAGS=-count=1 scripts/check-all.sh`, exit 0:

```
==> go test -race (gateway)
ok  	kaiak/cmd/kaiak	2.232s
ok  	kaiak/e2e	95.178s
ok  	kaiak/internal/accounting	2.778s
ok  	kaiak/internal/auth	2.470s
ok  	kaiak/internal/clip	1.652s
ok  	kaiak/internal/config	2.869s
ok  	kaiak/internal/control	14.311s
ok  	kaiak/internal/limits	4.157s
ok  	kaiak/internal/metrics	3.175s
ok  	kaiak/internal/provider	3.735s
ok  	kaiak/internal/routing	3.858s
ok  	kaiak/internal/schemacheck	4.287s
ok  	kaiak/internal/server	13.287s
ok  	kaiak/internal/sse	4.740s
ok  	kaiak/internal/state	5.075s
==> gofmt (live-test kit)
==> go vet (live-test kit)
==> staticcheck 2026.2.1 (live-test kit)
==> live-test kit self-test
self-test passed for vllm, openai, azure-openai, vllm with two backends
gateway checks passed
==> npm test (control)
ℹ tests 504
ℹ pass 504
ℹ fail 0
==> npm run lint (control)
boundaries ok
==> cross-half e2e (sample control plane + two gateways)
ok  	kaiak/e2e	53.841s
all checks passed
```

No flakes seen; no stray processes afterwards.

**Decisions made during the step**

- The new tree test runs in control-plane mode, not file mode: limits are enforced
  the same way in both, and only there can the test read the records' `groups` and
  push a window naming a group.
- Which totals the gateway applied is marked the way the shared-limits test does it
  (a live count set after the windows, seen in the backend cap's share), not by
  polling requests: a polled `priced` request that got through would spend locally
  and refuse on its own, hiding a window that was never applied.
- The cross-half budget sits on the key's parent, not its own group: counting
  toward an ancestor is the part only the tree adds.
- The live kit's config uses one top-level group: its checks need one key with every
  model and a limit, nothing deeper.
- The example's `research` branch skips the env level: the order and number of
  levels are free per branch, and the example shows it.

**Phase 1 complete** (steps 1–5): committed and green.
