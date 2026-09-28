# Step 1 — container image (D8)

**Status:** done (2026-09-25) — images built from `73701de`, smoke-tested and pushed

## Intent

A Dockerfile for the gateway (and one for the sample control plane), built and run
locally, so the user can test on their network.

## Files likely touched

- `gateway/Dockerfile`, `.dockerignore`; `control/sample/Dockerfile` (or one under
  `control/`); `scripts/build-images.sh`; README; `docs/BACKLOG.md` (remove the
  Container image entry — resolved); `docs/TECH-STACK.md` (base image versions)

## Decisions made during planning

- Gateway: multi-stage, Go version from `go.mod`, `CGO_ENABLED=0`, `-trimpath`, static
  distroless non-root final image with only the binary; `KAIAK_DATA_DIR=/data` as a
  volume writable by the non-root user; `EXPOSE 8080 9090`; no config baked in.
- Sample: Node current stable slim base, `npm ci --omit=dev` in the workspace, runs
  `node control/sample/src/main.ts` (type stripping), non-root; the schemas from
  `protocol/` copied in (see L18 in step 9 — here: copy the directory so it resolves).
- ~~Multi-arch build (amd64 + arm64)~~ — superseded by the user: `linux/amd64` only,
  built on the remote Docker context `dev` (buildx builder `dev`), pushed to the registry.
- Verification: build both, run gateway (file mode, mounted config + data volume)
  against the fake backend, run sample + gateway in control mode, curl a request.

## Acceptance criteria

- Both images build (host arch; arm64 + amd64 via buildx if available); a scripted
  local smoke run passes and cleans up containers; image sizes recorded.

## Result

- **Files**: `gateway/Dockerfile` + `gateway/.dockerignore` (context `gateway/`);
  `control/sample/Dockerfile` + `control/sample/Dockerfile.dockerignore` (context the
  repo root; allowlist: the three package manifests, the lockfile, both `src/`, and
  `protocol/schema`); `scripts/build-images.sh`, `scripts/smoke-images.sh`; README
  (Container images), `docs/TECH-STACK.md` (Container images, settled 2026-09-25),
  AGENTS.md Commands; the Delivery → Container image backlog entry removed.
- **Bases**: `golang:1.27.1` (from go.mod) → `gcr.io/distroless/static-debian13:nonroot`
  (debian13 is the current distroless line); `node:26-slim` (26.10.0 current).
- **Commands**: `scripts/check-all.sh` → `all checks passed` (380 control tests, lint,
  gateway checks, cross-half e2e 38 s). `scripts/build-images.sh --push` → build,
  `scripts/smoke-images.sh` (fake backend; gateway file mode: readyz 200, chat 200
  with usage 7/49, `limits.json` written to `/data` as uid 65532; sample + gateway
  control-plane mode: readyz 200, chat 200, `last-known-good.json` and
  `usage-spool.json` in `/data`; `smoke passed`; every container, network and volume
  removed), then push. The failure path was also exercised (a wrong image as gateway):
  logs dumped, everything removed, non-zero exit.
- **Images** (tags `0.3.1-73701de`, `73701de`, `latest`; `curl
  https://registry.example.com/v2/kaiak/<name>/tags/list` lists all three):
  - `registry.example.com/kaiak/kaiak@sha256:d4038bdaf065115ed1eb3e851d37cef85945048dc22892e495f118fa88dc9125`
    — 18.5 MB unpacked, 4.4 MB compressed.
  - `registry.example.com/kaiak/kaiak-sample@sha256:242714dcaec2d43baac39bed3f9b364eb6fbe6151c4112416bfc26cb438ec826`
    — 373 MB unpacked, 88 MB compressed.
- **Decisions**:
  - `kaiak_build_info` shows `version="(devel)"`: the build context has no `.git`, so
    Go stamps no module version, and Go offers no flag to set it. The version is the
    image tag and the `org.opencontainers.image.version` label (`--build-arg VERSION`).
  - The sample image installs runtime dependencies only and logs JSON
    (`KAIAK_LOG_FORMAT=json` set); pino-pretty stays a development dependency.
  - No `VOLUME` instruction (it litters anonymous volumes); `/data` exists owned by
    `nonroot` so a named volume mounted there starts writable. No `HEALTHCHECK`
    (distroless has no client; orchestrators probe `/readyz` on 9090).
  - The smoke test needs only docker locally: the key is minted by the sample image
    (`node sample/src/keygen-cli.ts`), the config is seeded into a volume with
    `docker cp` through a created, never started container, and requests run from a
    throwaway sample-image container on the test network (Node's `fetch`) — no ports
    published, no extra image pulled. The fake backend is the gateway Dockerfile's
    `fakebackend` target, built per run and removed.
  - Tags: a dirty tree builds under `<describe>-dirty` and refuses `--push`. The
    result is recorded in a second commit so that the pushed images' sha tag
    (`73701de`) names a commit on the branch.
