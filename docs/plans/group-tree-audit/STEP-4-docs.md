# Step 4 — docs and proof

**Status:** done (2026-09-27)

## Intent

Docs follow the decisions; the proof runs.

## Files likely touched

- `control/kaiak-control/GUIDE.md` — S1 (a re-created ID resumes its window; use a
  new ID for a fresh budget), S2 (hard restrictions on the parent), the effective-
  limits bound, S3 in the carry-over notes.
- `docs/DEPLOYMENT.md` — `key_group` in cardinality, queries and alerts; S2; the
  bound and its memory arithmetic.
- `docs/architecture/gateway.html`, `docs/architecture/control-plane.html` — the
  metric label and anything naming `group` as a metric label.
- `docs/BACKLOG.md` — R5 (totals size; trigger: totals messages above a few MiB or
  active windows in the tens of thousands), R6 (seal usage batches by encoded size;
  trigger: a batch set aside for size), R7 (sample page render time; trigger: a
  sample used with thousands of groups).
- `docs/plans/group-tree/OVERVIEW.md` — correct the verification box that claimed a
  gateway test for deleted-group settlement (now true after step 2 — say where).
- `docs/reviews/2026-09-27/AUDIT.md` — nothing (the record of the review stays as
  written).

## Acceptance criteria

- `grep` for the old metric label (`group="`, `by (group)`, usage `group` label) finds
  only `key_group` / `root_group` or unrelated uses.
- `scripts/check-all.sh` green 3× in a row; outputs recorded.
- OVERVIEW verification status filled in.

## Result

- `control/kaiak-control/GUIDE.md`: §5 window identity — windows keyed by group ID,
  a re-created ID reads them again (replaces "no longer read"); §7 group tree —
  `child_defaults` is a default, not a ceiling (S2); the effective-limits bound
  (50 000, `effective-limits-exceeded` at `""`, ~1.4 KB per counter → ~70 MB, D × N);
  a re-created ID resumes its window's spend, new ID for a fresh budget (S1);
  model-set edits: `onLimitCarriedOver`, each side carries against the config it last
  held (S3); keys: `isConfigId`; §11 the move entry names the resume.
- `docs/DEPLOYMENT.md`: Resources headroom lists limit counters (~70 MB at the
  bound); Config for many hosts — S2, the bound and its arithmetic, ID reuse;
  Observability cardinality — `key_group`, why not `group`, `sum by (key_group)`,
  the `root_group` note (S5). No alert or PromQL used the usage group label.
- `docs/architecture/gateway.html`: metrics bullet names `key_group`; the group
  paragraph says defaults are not ceilings and states the bound; footer date line.
  `docs/architecture/control-plane.html`: model-set edits carry against the config
  each side last held; the tree figure's caption names ID reuse; footer date line.
  No SVG text changed (prose, captions and footers only), so no browser check.
- `docs/ARCHITECTURE.md`: unchanged — it names no usage label and no carry-over or
  validation cost.
- `docs/BACKLOG.md`: R5 (totals size — now ~5.8 MB at most under the bound, since
  totals list one window per configured limit), R6 (seal batches by encoded size),
  R7 (sample page render time), each with its trigger; and the step-2 note (a copy
  from a predecessor another new limit claimed reads the pushed base under the
  claimer's key) under Limits — a known imprecision of the code, not an agreement,
  so it belongs in the backlog rather than `GATEWAY.md`, which holds contracts.
- `docs/plans/group-tree/OVERVIEW.md`: the deleted-group box says the gateway test
  was missing at 0.7.0 (review T1) and is now
  `TestSettlementReachesAncestorsOfADeletedGroup` (this plan's step 2).
- Grep for the old label (`group="`, `by (…group)`, `, group)`, `exported_group`,
  `{group`) outside plans, reviews and `node_modules`: only `key_group` /
  `root_group` series, the two notes explaining `exported_group`, and unrelated uses
  (config and totals `group` fields, log field `group`, `scope_kind="group"`).

### Suite

`GOFLAGS=-count=1 scripts/check-all.sh`, three runs in a row, each `all checks
passed`:

| Run | Duration | Gateway e2e | Control | Cross-half e2e |
|---|---|---|---|---|
| 1 | 146 s | `ok kaiak/e2e 94.9s` | tests 510, pass 510, fail 0 | `ok kaiak/e2e 43.9s` |
| 2 | 141 s | `ok kaiak/e2e 91.3s` | tests 510, pass 510, fail 0 | `ok kaiak/e2e 43.8s` |
| 3 | 156 s | `ok kaiak/e2e 95.4s` | tests 510, pass 510, fail 0 | `ok kaiak/e2e 53.9s` |

Each run also: gofmt, vet, staticcheck clean; live-test kit self-test 13/13/13/16;
`gateway checks passed`; `tsc` clean, `boundaries ok`.
