# Step 3 — label lists, key labels, build version

**Status:** not started

## Intent

The structure findings that change no output: each closed label vocabulary owned by
the code that produces it (F3), one key-label helper (F4), the build version owned by
`main` (F8), the log-export count mirror gone (F10). Label values and names unchanged
here — step 6 renames.

## Files likely touched

- `gateway/internal/control/usage.go`, `client.go` — export `BatchResults`,
  `DropReasons` as typed lists; `Trigger*` with startup and sighup
  (`config.LoadTriggers`, or wherever the trigger type best lives — the step decides
  and says why).
- `gateway/internal/limits/limits.go` — `Scopes` (the scope kinds).
- `gateway/internal/config/` — the load results (applied, rejected), if the load
  owns them.
- `gateway/internal/metrics/{ops,delivery}.go` — pre-create from those lists; typed
  parameters; the six `slices.Contains … panic` guards and the HELP strings that
  enumerate values go (HELP says what the label means; the values live in the type).
- `gateway/internal/metrics/{usage,ops}.go` — one `keyLabels` used by the usage
  metrics and `kaiak_request_errors_total` (F4).
- `gateway/cmd/kaiak/main.go`, `gateway/Dockerfile` — `-X main.version`; `main`
  computes the version (stamped, else the module's, else `(devel)`) and passes it
  to `metrics.RegisterBuildInfo` and the OTLP resource; `buildVersion` and its test
  move to `main` (F8).
- `gateway/internal/metrics/logexport.go` — reads the exporter's counts directly; no
  mirror struct (F10). Step 6 replaces the family itself.
- `scripts/build-images.sh` if it names the ldflags path.

## Decisions made during planning

- Typed vocabularies: a named string type per list with its values as constants and
  an exported list; `metrics` takes the type, so a wrong value does not compile.
- `metrics` may import `control` and `limits` (neither imports `metrics`); `go vet`
  catches a cycle should one appear.

## Acceptance criteria

- `/metrics` output byte-identical before and after on a fixed scenario (a golden
  test over a registry fed the same calls, or the e2e scrape compared).
- `git grep -n "slices.Contains" gateway/internal/metrics` finds no guard.
- `git grep -n "internal/metrics.version"` finds nothing; the image build's
  `kaiak_build_info` still reports the `git describe` version (`smoke-images.sh`).
- `scripts/check-all.sh` green. Suite recorded.

## Result

