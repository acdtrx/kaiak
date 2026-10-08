# Step 11 — docs and live checks

**Status:** not started

## Intent

Operators can find, configure and read the metrics; the names hold against real
OpenTelemetry and Prometheus software.

## Files likely touched

- `docs/DEPLOYMENT.md` — metric export setup (the variables, one endpoint for both
  signals, opting out), alerts and dashboard queries in the new names, Upgrades:
  the 0.12 note with every renamed metric and label (old → new, from the spec's
  "Was" column).
- `README.md`, `docs/ARCHITECTURE.md`, `docs/architecture/gateway.html`,
  `docs/testing/LIVE-BACKENDS.md` — wherever metrics or the telemetry layout
  appear.
- `scripts/live/` — any check reading metrics follows the names (done in step 6;
  re-run here live).

## Decisions made during planning

- Live checks run on the user's test setup (`docs/testing/LIVE-BACKENDS.md`), with
  containers removed afterwards.

## Acceptance criteria

- Live: an `otel/opentelemetry-collector` (OTLP/HTTP receiver, `debug` exporter)
  shows every family with its unit and attributes under the resource.
- Live: a Prometheus 3.x with `--web.enable-otlp-receiver` ingests the gateway's push
  while scraping its `/metrics`: for every family, the pushed series' name and label
  names equal the scraped ones (`job`/`instance` aside).
- Live: the live-test kit's DGX checks pass in the new names.
- `scripts/check-all.sh` green. Suite recorded.

## Result

