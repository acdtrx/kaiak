# Step 10 — deployment guide

**Status:** done (2026-09-25)

## Intent

`docs/DEPLOYMENT.md`: running kaiak for real on Kubernetes (manifests stay the user's).

## Contents

StatefulSet + PVC per pod (stable instance ID, spool and LKG survive), data-dir lock,
`terminationGracePeriodSeconds` sized to the longest stream + drain, `/readyz` and
`/healthz` probes, admin-port NetworkPolicy, Prometheus scrape, `GOMEMLIMIT`,
`max_in_flight` = host capacity (split automatically), timeouts, cardinality guidance,
starter alerts (outage, spool depth, ack age, circuit open, queue rejections, config
rejected), control-plane single-process rule, the seed config, Azure notes.

## Acceptance criteria

- Reviewed against the code; every env var and metric named exists.

## Result

**Files**: `docs/DEPLOYMENT.md` (new); `docs/ARCHITECTURE.md` (a short "Deployment
shape" section); `README.md` ("Running it for real" pointer). Docs only — no code,
spec or schema changed.

**Guide outline**: Topology (replicas behind a Service; one control-plane process per
store, `Recreate`; the sample is in-memory, not billing; file mode is single-replica)
· Gateway pods (StatefulSet + PVC per pod, stable instance ID, `Parallel` pod
management, seed config, why not Deployment + emptyDir, no shared volumes, scale-down
and seed staleness) · Probes and lifecycle (`/readyz`, `/healthz`, drain sequence,
`terminationGracePeriodSeconds` ≥ grace + timeout + 10 s, sizing the drain timeout to
long streams, the flush sharing the drain deadline, no `preStop` — raise the grace
instead) · Resources (2 × body budget + headroom, `GOMEMLIMIT` ≈ 90% of the limit, CPU,
disk) · Environment (every `KAIAK_*` variable: gateway, sample, image build) · Config
for many hosts (backend per vLLM process, `max_in_flight` split, the four timeouts and
ingress timeouts/body size, `max_n`, Qwen defaults, per-minute shares vs keep-alive,
credentials before config, file-mode SIGHUP) · Azure (v1, key per resource,
deployment name, probe model check skipped, 429 spill-over, `Retry-After-Ms`, prices
and the outage refusal, live kit first) · Observability (scrape, NetworkPolicy,
token, cardinality, 17 starter alerts) · Secrets and trust · Upgrades (no
compatibility, spool flush, data-file versions table) · Images.

**Verification method**:

- Env vars: `grep -rhoE 'KAIAK_[A-Z0-9_]+'` over `gateway/`, `control/kaiak-control/src`,
  `control/sample/src`, both Dockerfiles and `scripts/` → 22 names; each read site
  opened (`gateway/cmd/kaiak/main.go` `readSettings`/`readControlSettings`,
  `control/sample/src/settings/index.ts`, `scripts/build-images.sh`); defaults taken
  from the code (`defaultListenAddr`, `defaultAdminAddr`, `defaultDrainGrace/Timeout`,
  `defaultBootWait`, `server.DefaultClientTimeouts`, `server.DefaultBodyMemory`,
  `state.DefaultDir`, `DEFAULT_LISTEN`, the Dockerfiles' `ENV`). A script then checked
  every `KAIAK_*` in the guide against those files (all found) and every name from
  the code appears in the guide.
- Metrics: every `kaiak_*` in the guide checked by script against the quoted names in
  `gateway/internal/metrics/*.go` (16 names, all found); label values used in alert
  expressions checked in code: error classes (`metrics/ops.go`), attempt outcomes,
  queue reasons `full`/`timeout`, transitions `to="open"` (`routing.CircuitOpen`),
  batch results `acked`/`rejected`/`failed` and drop reasons
  `invalid`/`spool_unwritable` (`metrics/delivery.go`, `control/usage.go`), config
  load `result="rejected"`.
- Endpoints and texts: `/metrics`, `/healthz` (`ok`), `/readyz` (`ready`, `draining`,
  `config not loaded`) in `server/admin.go`; flush log lines `usage flushed` / `usage
  not flushed: left in the spool for the next start` in `control/usage.go`; the drain
  deadline (grace + timeout from the drain's start), `finalStatusTimeout` 2 s and
  `adminShutdownTimeout` 5 s in `cmd/kaiak/main.go`.
- Config defaults: `config/snapshot.go` constants and `config.schema.json` defaults
  (timeouts 5 s/60 s/30 min/120 s, body cap 4 MiB, `max_n` 8, grace 15 min, queue
  100/30 s, 3 attempts, circuit 5/10 s); lease TTL 30 s
  (`kaiak-control/src/control-plane/index.ts`); proxy variables honored by both the
  provider transport (`http.ProxyFromEnvironment`) and the control client (a clone of
  `http.DefaultTransport`); non-stream `choices` kept up to 4 MiB
  (`accounting/meter.go` `maxChoicesBytes`).

**Spec/code and doc contradictions found** (reported, not fixed):

1. `kaiak_config_loads_total`'s `trigger` values: GATEWAY.md's metric table and the
   metric's help text (`metrics/ops.go:142`) list `startup`/`sighup`/`control`/
   `last-known-good`; the control client also applies the seed with trigger `seed`
   (`control/client.go:32`, `:319`), so a `seed` series exists. GATEWAY.md's log
   trigger list already names `seed`.
2. `docs/kaiak.md` (Domain model → Model) says deployments have "load balancing and
   fallback order"; GATEWAY.md settles **no fallback** and no weights or order
   (Routing and reliability, 2026-09-24).
3. GATEWAY.md Lifecycle's Kubernetes line sizes `terminationGracePeriodSeconds` as
   grace + drain timeout + "a few seconds" (> 65 s at the defaults); after the drain
   the process can still spend up to 2 s (final status) + 5 s (admin shutdown with a
   scrape open) + the totals/snapshot write. The guide says + 10 s (75 s).
4. Not a contradiction but a gap against GATEWAY.md's memory statement ("peak body
   memory ≈ 2× the budget"): every non-stream answer in flight keeps its `choices`
   up to 4 MiB for output estimation (`accounting/meter.go:44`), outside the body
   budget — bounded by answers in flight × 4 MiB, not by the budget. The guide
   counts it in the headroom.
5. Operational, consistent with the spec: the usage flush gets only what remains of
   grace + drain timeout, so a drain that runs to its timeout leaves the flush no
   time (batches stay in the spool). AGENTS.md's "which is why shutdown flushes it"
   reads as a guarantee it is not. The guide tells operators to check the
   `usage flushed` line before a spool-format upgrade.

**Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gofmt, vet,
staticcheck, gateway race tests incl. e2e 41.9 s, live kit self-test 13/13 ×3 + 16/16,
control 443/443, lint + boundaries, cross-half e2e 58.3 s). No expected reds.

## Decisions for the user to confirm

1. **No YAML** — the guide names Kubernetes fields in tables (StatefulSet,
   `volumeClaimTemplates`, `podManagementPolicy: Parallel`, probes,
   `terminationGracePeriodSeconds`, `fsGroup: 65532`, NetworkPolicy) but ships no
   manifests or fragments, per the out-of-scope ruling.
2. **Control plane `Recreate`, `replicas: 1`** — also for the sample (two in-memory
   processes behind one Service would be two stores).
3. **Sizing numbers are starting points, not measurements**: memory limit = 2 × body
   budget + 512 MiB, `GOMEMLIMIT` ≈ 90% of the limit, PVC ~1 GiB, drain timeout
   example 600 s for 10-minute requests.
4. **Third-party defaults cited from general knowledge, not verified here**:
   ingress-nginx `proxy-read-timeout` 60 s and `proxy-body-size` 1 MiB; Go sizing
   `GOMAXPROCS` from the CPU limit (Go ≥ 1.25).
5. **Alert thresholds** (15 min ≈ the default outage grace, spool > 100, flapping > 3
   opens in 30 min, failure rate 5%) are starter values. The "totals for another
   config" alert is conditioned on acknowledged batches, since totals only move with
   traffic.
6. **Flush time** (contradiction 5): whether the drain should reserve a few seconds
   for the flush is a design question left open; the guide documents today's
   behavior.
