# kaiak

A self-contained LLM gateway. One static Go binary sits between your clients and your
model backends: it speaks the **OpenAI API** to clients, routes to self-hosted model
servers (vLLM, llama-server, SGLang) and cloud providers (OpenAI, Azure OpenAI), and
enforces **access keys, rate limits and budgets** while counting every token and
dollar.

kaiak has no UI and no admin API of its own. Its config comes either from a file or
from a **control plane** that pushes changes live and receives usage and status back.
You build that control plane with **`kaiak-control`**, the Node library in this repo;
a small sample control plane shows how.

- **Gateway** (`gateway/`): Go, standard library only, zero third-party dependencies.
  Stateless by default, and it keeps serving when the control plane is down.
- **`kaiak-control`** (`control/kaiak-control/`): the control-plane side of the
  protocol, for Node. It serves config, takes in usage exactly once, keeps totals,
  pushes budgets and tracks gateways. Storage is pluggable.
- **Sample control plane** (`control/sample/`): config from a file, state in memory,
  and a read-only status page. For local runs, demos and as a reference.
- **The contract** (`protocol/`): JSON Schemas and shared fixtures both halves test
  against.

> **Status:** pre-1.0. Config format and protocol are at version 3. There is no
> backwards compatibility between versions yet: gateways and control plane upgrade
> together.

## Documentation map

| If you want to… | Read |
|---|---|
| Understand what kaiak is and the principles behind it | [`docs/kaiak.md`](docs/kaiak.md) |
| See how the gateway works inside, with diagrams | [`docs/architecture/gateway.html`](docs/architecture/gateway.html) ¹ |
| See how gateways and a control plane work together, with diagrams | [`docs/architecture/control-plane.html`](docs/architecture/control-plane.html) ¹ |
| **Build your own control plane** on `kaiak-control` | [`control/kaiak-control/GUIDE.md`](control/kaiak-control/GUIDE.md) — start here (written for humans and coding agents) |
| Know the exact protocol and config contract | [`docs/specs/CONTROL-PROTOCOL.md`](docs/specs/CONTROL-PROTOCOL.md), schemas in [`protocol/schema/`](protocol/schema/) |
| Know exactly what the gateway does (API, limits, routing, lifecycle) | [`docs/specs/GATEWAY.md`](docs/specs/GATEWAY.md) |
| Run it in production (Kubernetes, sizing, alerts, secrets) | [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) |
| Find your way around the code | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md), [`docs/TECH-STACK.md`](docs/TECH-STACK.md) |
| Test against a real vLLM / llama-server / Azure OpenAI / OpenAI backend | [`docs/testing/LIVE-BACKENDS.md`](docs/testing/LIVE-BACKENDS.md) |
| See what is deliberately not built yet, and when it would be | [`docs/BACKLOG.md`](docs/BACKLOG.md) |
| Contribute (or point a coding agent at the project) | [`AGENTS.md`](AGENTS.md), [`docs/CODING-RULES.md`](docs/CODING-RULES.md) |

¹ Self-contained HTML pages. GitHub shows their source, so open them in a browser
from a clone, or serve `docs/` with GitHub Pages.

## Concepts in one minute

- **Backends and models.** A backend is a serving endpoint. A model is the public
  name clients send, served by one or more deployments (backend + backend-side model
  name). Requests are load-balanced across deployments, retried on another deployment
  before the first byte, and kept away from failing deployments by a circuit breaker.
- **Groups.** Who may use which models, and under which limits, is a tree of groups
  of any depth: team → project → env → workload, or whatever shape your organization
  has. A key belongs to one group. A request must pass the limits of every group on
  its key's path, plus the global limits, and its usage counts toward each of them.
- **Limits.** requests/min and tokens/min (enforced per gateway on a share),
  tokens/hour and USD/month (from totals the control plane pushes to every gateway),
  each over all models or a set of models.
- **Usage.** Every routed request settles into a usage record: tokens by kind and cost
  in nano-USD. Records go to the control plane in batches, delivered at least once and
  counted exactly once.

The config format: [`examples/config.json`](examples/config.json) (a realistic tree),
the schema, and CONTROL-PROTOCOL.md → Config.

## Quick start: the gateway alone

Needs Go (the version in `gateway/go.mod`) and, for minting keys, Node (current
stable).

1. **Build**

   ```sh
   cd gateway && CGO_ENABLED=0 go build -trimpath -o kaiak ./cmd/kaiak
   ```

2. **Mint a client key.** Config holds only the key's SHA-256 hash; the key itself is
   shown once and kept by the client. From `control/` (run `npm install` there once):

   ```sh
   npm run -s keygen -w sample -- --id k-me --group alice
   ```

   It prints the key, its ID, its hash and the entry to paste into the config's `keys`.

3. **Write a config.** Start from [`examples/config.json`](examples/config.json): two
   vLLM hosts, a vLLM embeddings host, an Azure OpenAI resource with priced models, a
   group tree (a team with a project split into prod and dev, another team, and a
   `users` group whose `child_defaults` give every person the same models and
   limits), and example limits. Replace the backend URLs and the placeholder key
   hashes. A backend's `type` names its server — `vllm`, `llama-server`, `openai`,
   `azure-openai`, or `openai-compatible` for any other OpenAI-format server
   (`docs/DEPLOYMENT.md` → Config for many hosts). Backend credentials are named by
   environment variable (`api_key_env`), never written in the file.

4. **Run**

   ```sh
   export AZURE_SWEDENCENTRAL_API_KEY=... VLLM_EMBED_API_KEY=...   # every api_key_env the config names
   KAIAK_CONFIG_FILE=../examples/config.json KAIAK_LOG_FORMAT=text ./kaiak
   ```

   API on `:8080`, admin on `:9090` (`/healthz`, `/readyz`, `/metrics`).

   ```sh
   curl -s localhost:8080/v1/chat/completions -H "Authorization: Bearer $key" \
     -H 'Content-Type: application/json' \
     -d '{"model": "qwen3-32b", "messages": [{"role": "user", "content": "Hello"}]}'
   ```

   `kill -HUP` reloads the config file (a bad one is rejected and the running one
   kept); SIGTERM or Ctrl-C drains in-flight requests and exits. Nothing is written to
   disk unless `KAIAK_DATA_DIR` is set. All environment variables:
   GATEWAY.md → Configuration sources.

## Quick start: with the sample control plane

The whole loop runs locally, with a fake backend standing in for a model server.

1. **A fake backend** on `127.0.0.1:8000` (what `examples/local-config.json` points at):

   ```sh
   cd gateway && go run ./internal/fakebackend/cmd/fakebackend -models demo
   ```

2. **A config and a key.** `data/` is gitignored; from the repo root:

   ```sh
   mkdir -p data && cp examples/local-config.json data/sample-config.json
   cd control && npm install && npm run -s keygen -w sample -- --id k-demo --group demo-app
   ```

   Paste the printed `keys` entry into `data/sample-config.json` and keep the key.

3. **The sample control plane**, from `control/`:

   ```sh
   KAIAK_SAMPLE_CONFIG=../data/sample-config.json KAIAK_CONTROL_TOKEN=dev-token npm run dev -w sample
   ```

   Every save of the config file is validated, published as a new version and pushed
   to the gateways; an invalid file is rejected and the current version stays.
   Optional: `KAIAK_SAMPLE_LISTEN` (default `127.0.0.1:8090`), `KAIAK_LOG_FORMAT`.

4. **A gateway in control-plane mode**, from `gateway/`:

   ```sh
   KAIAK_CONTROL_URL=http://127.0.0.1:8090 KAIAK_CONTROL_TOKEN=dev-token \
     KAIAK_INSTANCE_ID=gw-1 KAIAK_LOG_FORMAT=text go run ./cmd/kaiak
   ```

5. **A request** with the key from step 2, model `demo`, as above. Then edit
   `data/sample-config.json` (add a limit, another key) and watch the sample publish
   version 2 and the gateway apply it.

6. **The status page** at <http://127.0.0.1:8090/>: gateways and their backends and
   circuits, the config and its group tree, totals against every limit, and recent
   usage, all updated live. It has **no authentication**: it is for local and demo use.

## Building your own control plane

The sample is a demo: its state is in memory and resets on restart. A real control
plane (with your UI, your users and your database) is a separate application that
imports `kaiak-control`. The library does the whole protocol side. You supply:

- **a store**: an implementation of the `ControlPlaneStore` interface on your
  database. Its contract covers exactly-once usage counting, a lease that keeps one
  control-plane process per store, and a config history;
- **a config source**: your UI or API builds whole config documents and publishes
  them through the library, which validates them and assigns versions;
- **everything a human touches**: login, roles, audit, reports and key management.

What to read, in order:

1. [`docs/architecture/control-plane.html`](docs/architecture/control-plane.html):
   the picture of how gateways and a control plane interact.
2. [`control/kaiak-control/GUIDE.md`](control/kaiak-control/GUIDE.md): the build
   guide. It covers the minimal host app, the store contract method by method with a
   SQL sketch, publishing config, reading state for a UI, operations, testing, and
   mistakes that look reasonable. It is written so a coding agent can follow it as a
   spec.
3. [`docs/specs/CONTROL-PROTOCOL.md`](docs/specs/CONTROL-PROTOCOL.md): the contract,
   when you need the exact rule.
4. [`control/sample/`](control/sample/): a complete, small host app to copy from.

`kaiak-control` ships TypeScript source run by Node's type stripping, so consume it as
a workspace or `file:` dependency (GUIDE.md §3 explains why).

## Container images

Two images, for `linux/amd64` and `linux/arm64`, published with every release:

- `ghcr.io/acdtrx/kaiak`: the gateway, the static binary on a distroless base (about
  19 MB). It writes nothing by default, so it runs with a read-only root filesystem
  and no volume.
- `ghcr.io/acdtrx/kaiak-sample`: the sample control plane.

Tags are the release version (`0.8.0`) and `latest` for the newest stable release.
To build your own: `scripts/build-images.sh --repo <registry/namespace> [--push]`
(`--push` pushes only after `scripts/smoke-images.sh` has run both images end to end).

```sh
# the gateway in file mode: mount the config's directory read-only
docker run -d --name kaiak --read-only --stop-timeout 75 -p 8080:8080 -p 9090:9090 \
  -v "$PWD/kaiak-config:/config:ro" -e KAIAK_CONFIG_FILE=/config/config.json \
  -e AZURE_SWEDENCENTRAL_API_KEY -e VLLM_EMBED_API_KEY \
  ghcr.io/acdtrx/kaiak:latest

# the sample control plane and a gateway in control-plane mode
docker network create kaiak
docker run -d --name kaiak-sample --network kaiak -p 8090:8090 \
  -v "$PWD/kaiak-config:/config:ro" -e KAIAK_SAMPLE_CONFIG=/config/config.json \
  -e KAIAK_CONTROL_TOKEN=change-me ghcr.io/acdtrx/kaiak-sample:latest
docker run -d --name kaiak-gw-1 --read-only --stop-timeout 75 --network kaiak \
  -p 8080:8080 -p 9090:9090 \
  -e KAIAK_CONTROL_URL=http://kaiak-sample:8090 -e KAIAK_CONTROL_TOKEN=change-me \
  -e AZURE_SWEDENCENTRAL_API_KEY -e VLLM_EMBED_API_KEY \
  ghcr.io/acdtrx/kaiak:latest

# mint a key with the sample image, no local Node needed
docker run --rm ghcr.io/acdtrx/kaiak-sample:latest \
  node sample/src/keygen-cli.ts --id k-me --group alice
```

The gateway reads the backends' API keys from its own environment in both modes: the
control plane publishes only the variable names. `--stop-timeout 75` gives the drain
its time at the defaults — `docker stop` otherwise kills the gateway after 10 s,
cutting requests still running and losing their usage (`docs/DEPLOYMENT.md`,
Draining).

Production (replicas, probes, drain timing, sizing, alerts):
[`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md).

## Repository layout

```
gateway/              the kaiak binary (Go): cmd/kaiak, internal/<subsystem>, e2e tests
control/kaiak-control the control-plane library (Node, TypeScript)
control/sample        the sample control plane
protocol/             JSON Schemas and shared fixtures (the contract)
examples/             example configs
scripts/              check-all, image build and smoke, live-backend test kit
docs/                 philosophy, specs, architecture, deployment, backlog
docs/plans, docs/reviews   design plans and code reviews, kept as the project's record
```

## Checks

- `scripts/check-all.sh` runs everything that needs no real backend, from any
  directory: gateway lint and race tests (including end-to-end tests of the built
  binary), `npm test` and `npm run lint` in `control/`, and the cross-half end-to-end
  test (the sample control plane, two gateways and a fake backend).
- `scripts/check-gateway.sh` runs the gateway part alone.
- Against a real backend (opt-in): `go -C scripts/live run . -kind vllm -base-url
  http://vllm-host:8000/v1 -model <model>`; see
  [`docs/testing/LIVE-BACKENDS.md`](docs/testing/LIVE-BACKENDS.md).

## License

MIT — see [`LICENSE`](LICENSE).
