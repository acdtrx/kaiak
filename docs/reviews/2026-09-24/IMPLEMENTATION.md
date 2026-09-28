# Audit follow-up — implementation sketch

Items from the merged `AUDIT.md`, grouped by severity, each with a fix sketch. Work
graduates to `docs/plans/audit-fixes/` once the pending decision is made.

## Decisions (user, 2026-09-24/25)

- **D1 agreed (H6).** Once the upstream request was fully sent, any end before the
  first event — client gone, drain cut, gateway timeout — records estimated input
  (`estimated` + `partial`); zero only when the request never reached the backend.
- **D2 agreed (H1, H7).** Three timeouts: stream first-event (default 60 s), non-stream
  whole response (default 30 min; not retried, not a circuit failure), between events
  once streaming (default 120 s; a circuit failure).
- **D3 agreed (M1): automatic split.** `max_in_flight` stays per backend (what the host
  can take); in control-plane mode each gateway enforces `ceil(cap ÷ live gateways)`,
  using the live count already pushed with totals; file mode unchanged. Known limit:
  a gateway with longer requests queues while others' shares sit idle — the
  demand-weighted extension is in `docs/BACKLOG.md`.
- **D4 agreed (M6).** Uncounted usage belongs to its own window; the control plane
  counts a record into its `gateway_time` window when that is the current or previous
  one, otherwise the current.
- **D5 agreed (M8).** A limit whose model list changes keeps its spend (carry-over by
  scope + owner + type), both halves.
- **D6 agreed (M9).** Unpriced models are never refused for budget reasons.
- **D7 agreed (H12).** One control-plane process per store, stated and enforced (store
  lease); the de-dup check moves into the store's atomic write (conditional on the
  last batch ID) so a store can guarantee it; the sample says budgets reset on restart.
- **D8 agreed (M12).** A Dockerfile and a verified local build now; the user tests it on
  their network. A deployment guide follows.

## High

- **H1** between-event stall timer in the relay (D2); expiry = `upstream_failed`,
  circuit failure.
- **H2** `IdleTimeout` 120 s (API + admin); body-read deadline (e.g. 60 s) that also
  bounds the server's disposal of unread bodies; per-write deadline (60 s per event) =
  client gone.
- **H3** totals carry the config version they were computed under; the limiter applies
  a totals message only when that version equals the gateway's applied version;
  otherwise it keeps its bases and local usage and — while the mismatch lasts past the
  outage grace — treats USD limits as in outage. Status already reports the rejection,
  so the control plane sees why.
- **H4** settle limits before publishing the record, and make sealing tag by an
  explicit per-record generation assigned at publication (one lock covers "tag record
  + append to batch"); tests for the 500th record, interval seals and fast acks.
- **H5** clamp units and cost to 2^53−1 at settlement (flag + warn); the sender checks
  each record at seal time and sets aside only a bad record.
- **H6** → D1 (provider reports "request sent"; meter settles accordingly).
- **H7** → D2; fix `examples/config.json`.
- **H8** backend 404 naming the model = deployment failure (retry elsewhere, circuit);
  the probe checks the model is listed in `/models`; a background check warns at
  config apply.
- **H9** drop `backend` from usage metrics; `user` label switchable; correct guidance.
- **H10** `n`, `best_of` and completion prompt arrays become owned: reservation ×
  sequences; config ceiling `max_n` (default 8); above it → 400; overflow-checked.
- **H11** bound stream establishment (connect + response headers) before the idle
  timer; test header stalls.
- **H12** → D7.

## Medium

- **M1** → D3. **M2** global body-bytes budget before reading; drop `rq.body` and the
  edited copy after the first event; lower default body cap to 4 MiB; `GOMEMLIMIT` in
  the guide.
- **M3** cache `canServe` per model per dispatch round; benchmark before/after.
- **M4** `kaiak_upstream_attempts_total{backend,deployment_model,outcome}`,
  `kaiak_upstream_attempt_duration_seconds{backend}`, `backend` on retries.
- **M5** persist last applied totals + uncounted amounts (format-versioned) and restore
  at boot; until the first totals, USD limits count as in outage after grace.
- **M6** → D4. **M7** a reservation fitting the full limit is admitted when the share's
  window is empty; config-time warning when output default × live > tokens/min.
- **M8** → D5. **M9** → D6.
- **M10** `config_epoch` per control-plane store in snapshot and stream; different
  epoch → resync.
- **M11** data-dir lock file; guide: StatefulSet + PVC per pod; optional seed config for
  a cold boot with neither control plane nor LKG.
- **M12** → D8.
- **M13** track the stream's terminal state (finish reason / `[DONE]`); EOF before it =
  partial + upstream failure (no retry after bytes reached the client).
- **M14** reject userinfo in `base_url` (both halves + fixtures); page shows a
  credential-free URL.
- **M15** watch for the `..data` symlink swap (or any directory change, relying on the
  unchanged-content check); test symlink rotation.
- **M16** contact for money limits also requires a recent ack (ack age ≤ grace) when
  batches are pending; alerts on ack age and spool depth in the guide.

## Low

- **L1** retry budget per model; relay `retry-after-ms`; short jittered delay before a
  same-deployment retry. **L2** NetworkPolicy guidance; optional `/metrics` token.
- **L3** truncate path/model (256 bytes). **L4** prune `lastBatches` with the forget
  sweep; `bodyLimit` 64 KiB on `/status`; document the trust model.
- **L5** document; optionally replace backend error text for 5xx. **L6** schema
  `maximum` 2^53−1 + fixtures. **L7** reject non-finite `defaults`; document the bound.
- **L8** seal counter in the spool index. **L9** drop unreachable branches; app-level
  `Kaiak-Protocol` hook. **L10** prune in `Configure`. **L11** clamp the injected default
  to context − input estimate. **L12** half-open circuit. **L13** backlog.
- **L14** fixtures. **L15** emit one circuit sample per distinct deployment.
- **L16** bound in-memory sealed records during write failure (refuse new usage? no —
  keep, but alert; decide in the plan). **L17** fix TECH-STACK text. **L18** ship schemas
  inside the package (copy at build/pack time or embed).
