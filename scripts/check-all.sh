#!/usr/bin/env bash
# Runs every check of both halves: the gateway's (scripts/check-gateway.sh), the
# control half's tests and lint, then the cross-half end-to-end test — the sample
# control plane and two kaiak processes, once with two control-plane cores over one
# store (gateway/e2e, build tag crosshalf). Installs
# control/'s dependencies first when they are missing. Works from any directory.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
	echo "check-all: Node.js and npm must be on PATH (docs/TECH-STACK.md)" >&2
	exit 1
fi

"$root/scripts/check-gateway.sh"

cd "$root/control"
if [[ ! -d node_modules ]]; then
	echo "==> npm ci (control)"
	npm ci
fi
echo "==> npm test (control)"
npm test
echo "==> npm run lint (control)"
npm run lint

cd "$root/gateway"
echo "==> cross-half e2e (sample control plane + two gateways; two cores over one store)"
# -count=1: the test cache does not see control/'s files.
go test -race -tags crosshalf -run '^TestAcrossHalves' -count=1 ./e2e

echo "all checks passed"
