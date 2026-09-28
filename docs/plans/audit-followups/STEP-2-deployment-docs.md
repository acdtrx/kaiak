# Step 2 — deployment guide for stateless gateways

**Status:** done (2026-09-25) — **phase 1 complete** (steps 1–2 committed, suite green)

## Items

- `docs/DEPLOYMENT.md` rewritten for E1/E2: Deployment (not StatefulSet), no volumes,
  `readOnlyRootFilesystem: true`, the seed as a ConfigMap of free local models, what a
  pod loses when it dies without draining, drain sizing with the reserve.
- **N-O2** `USER 65532:65532` (gateway) / `USER 1000:1000` (sample) in the Dockerfiles;
  full `securityContext` in the guide.
- **N-O3** Azure hidden-reasoning deployments: a separate backend entry with long
  first-event/stall timeouts; verify on the real resource.
- **N-O5** alert thresholds with lead time (~300 s); **N-O6** body cap vs Azure
  vision/long context; **N-O8** guide gaps (PodDisruptionBudget, topology spread, LB
  idle timeout vs 120 s, private CAs `SSL_CERT_FILE`, UTC month boundary, sample
  probes/resources).
- **N-O7** version stamping via `-ldflags -X` into `kaiak_build_info`.

## Acceptance

Every env var/metric/field in the guide verified against the code; images build and
the smoke test runs the gateway read-only with no volume.

## Result

- **Images** (`gateway/Dockerfile`, `control/sample/Dockerfile`):
  - Gateway: `ENV KAIAK_DATA_DIR` and the `/data` directory removed; `USER
    65532:65532`; no `VOLUME`. The fake backend builds in its own stage (no longer
    `FROM build`), so the version argument does not rebuild it.
  - Sample: `USER 1000:1000` — verified `id node` in `node:26-slim` on `dev`:
    `uid=1000(node) gid=1000(node)`.
  - **N-O7**: `var version string` in `internal/metrics`, set by `-ldflags "-X
    kaiak/internal/metrics.version=${VERSION}"` (the build script's `git describe`
    version, already passed as `VERSION`); `buildVersion` falls back to Go's module
    version, then `(devel)`. `TestBuildVersion` covers the three cases; a local
    `go build -ldflags -X …=9.9.9-test` binary served
    `kaiak_build_info{version="9.9.9-test",go_version="go1.27.1"} 1`. Reasoning
    recorded in TECH-STACK (Container images: version stamping; Go rules).
- **Smoke** (`scripts/smoke-images.sh`): both gateways `--read-only`, no volume.
  File mode mounts only the config (read-only); control-plane mode gets only
  `KAIAK_CONTROL_URL` + `KAIAK_CONTROL_TOKEN` (no instance ID, no data dir) and must
  exit 0 logging `usage flushed` on `docker stop`; `kaiak_build_info` must equal the
  image's `org.opencontainers.image.version` label (not empty, not `dev`). The data
  volumes and `/data` listings are gone.
- **Build + smoke on `dev`** (commit `23c59b6`, version `0.3.1-23c59b6`):
  `scripts/build-images.sh` then `scripts/smoke-images.sh --gateway …:0.3.1-23c59b6
  --sample …:0.3.1-23c59b6` → `kaiak_build_info{version="0.3.1-23c59b6",…} 1`, both
  chats 200, `gw-file: exit 0, logged "kaiak stopped"`, `gw-control: exit 0, logged
  "usage flushed"`, `smoke passed`. Image sizes: **kaiak 18.8 MB** unpacked (4 MB
  compressed), **kaiak-sample 373 MB** unpacked (87 MB compressed). Images show
  `User 65532:65532` / `1000:1000`, `Volumes null`, no `KAIAK_DATA_DIR` in `Env`.
  Separately checked: the sample image runs `--read-only --cap-drop ALL
  --security-opt no-new-privileges` and serves `GET /` 200 (so the guide gives it
  `readOnlyRootFilesystem: true`). All built images and smoke leftovers removed from
  `dev`.
- **Guide** (`docs/DEPLOYMENT.md`, rewritten): Topology · The minimal gateway (field
  table: Deployment, env, no volumes, pod/container securityContext, probes,
  resources, grace period; instance ID, rollouts, probes) · Boot (exit + backoff,
  what exits vs seeds) · Control-plane outages (memory, grace, 10 000-record bound
  with a rate example) · Draining and what a pod loses (reserve, sizing example
  610 s / 625 s, loss table) · Resources (memory formula, `GOMEMLIMIT` from the
  downward API vs ~90%) · Availability (N-O8: PDB, topology spread, LB idle timeout
  vs 120 s, private CAs via `SSL_CERT_DIR`, UTC months, the sample's TCP probes,
  resources, `replicas: 1`) · Environment · Config for many hosts (+ N-O6 body size)
  · Azure OpenAI (+ N-O3 reasoning backend entry, verify on the real resource) ·
  Observability (version, N-O5 alerts at 300 s, memory-bound alert,
  `budget_unavailable` with its two causes, crash-loop alert) · Secrets and trust ·
  Optional: the seed config · Optional: the data directory (PVC per pod,
  `fsGroup`, `emptyDir`, never shared) · Upgrades · Images. README container/run
  sections, TECH-STACK image notes and GATEWAY.md's `kaiak_build_info` row updated.
- **Verification method**:
  - Env vars: script — every `KAIAK_*` in the guide found in `gateway/cmd`,
    `gateway/internal`, `control/sample/src`, `control/kaiak-control/src`, the
    Dockerfiles and `scripts/`; every name read there appears in the guide (both
    lists empty). Defaults from `GATEWAY.md` rows re-checked against
    `config/snapshot.go` (timeouts, 4 MiB cap, 15 min grace), `server/listener.go`
    (120 s idle), `control/spool.go` (`maxSealedRecords = 10_000`).
  - Metrics: script — every `kaiak_*` in the guide is a quoted name in
    `internal/metrics/*.go` (none missing); alert label values
    (`budget_unavailable`, `no_healthy_deployment`, `server_busy`, the attempt
    outcomes, `full`, `memory_bound`, `acked`, `rejected`, `to="open"`) found in
    `metrics`/`routing`/`control`.
  - Texts: `/readyz` `ready`/`draining` and `/healthz` `ok` (`server/admin.go`); the
    boot exit lines (`control/client.go`, `transport.go`); `usage flushed` /
    `usage not flushed: lost at exit (no data directory)` (`control/usage.go`).
  - `SSL_CERT_FILE` set by the base image: `docker image inspect`; file + directory
    both loaded: Go 1.27.1 `crypto/x509/root.go` `loadOnDiskRoots`.
  - The Azure reasoning snippet: `examples/config.json` plus that backend entry and
    a model on it, run in file mode → `config applied` (two backends on one
    `base_url` are accepted).
- **Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (every
  gateway package ok incl. e2e, control 443 pass / 0 fail, lint, cross-half e2e ok).

## Decisions for the user to confirm

1. **No YAML kept** (the audit-fixes step 10 ruling): the minimal setup is a field
   table naming the securityContext, probes and env; the only snippet is the Azure
   backend's config JSON.
2. **`GOMEMLIMIT` from the downward API** sets it at 100% of the memory limit
   (`resourceFieldRef` cannot scale); the guide offers a literal ~90% value as the
   tighter alternative.
3. **Alert lead time 300 s** for the control-plane rows (grace 900 s); a new
   "Usage near the memory bound" alert at 5 000 records (half the bound) and a
   `CrashLoopBackOff` alert (kube-state-metrics) for gateways that cannot boot.
4. **Azure reasoning timeouts 600 s** (first event and stall) as the starting point,
   unmeasured — to be verified on the real resource.
5. **The sample runs with `readOnlyRootFilesystem: true`** too (verified in Docker),
   and gets TCP probes on 8090 rather than `GET /`.
6. **Smoke no longer checks persistence** (the data-dir files); the opt-in data
   directory stays covered by the Go tests (`cmd/kaiak`, `gateway/e2e`).
7. **`budget_unavailable` from a config mismatch** is documented as today's
   behavior (`kaiak_control_outage` stays 0); step 5 (N-P3) may add it to the gauge
   and should update the guide's alert rows.
