#!/usr/bin/env bash
# Runs every gateway check: formatting, vet, staticcheck, tests with the race detector
# (the end-to-end test in gateway/e2e included), then the same lint and the self-test
# for the live-test kit in scripts/live (its own module). Works from any directory.
# staticcheck is fetched by pinned version into the module cache and never enters
# go.mod (docs/TECH-STACK.md, Lint). vet and staticcheck also cover the cross-half
# e2e (build tag crosshalf), which only scripts/check-all.sh runs: it needs Node.
set -euo pipefail

STATICCHECK_VERSION=2026.2.1

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

lint() {
	echo "==> gofmt ($1)"
	unformatted=$(gofmt -l .)
	if [[ -n "$unformatted" ]]; then
		echo "gofmt: these files need formatting:" >&2
		echo "$unformatted" >&2
		exit 1
	fi

	echo "==> go vet ($1)"
	go vet -tags crosshalf ./...

	echo "==> staticcheck ${STATICCHECK_VERSION} ($1)"
	go run "honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}" -tags crosshalf ./...
}

cd "$root/gateway"
lint gateway

# Uncached: the shared fixtures (protocol/) live outside the gateway module, so Go's
# test cache does not see them change and would report old results.
echo "==> go test -race -count=1 (gateway)"
go test -race -count=1 ./...

cd "$root/scripts/live"
lint "live-test kit"

echo "==> live-test kit self-test"
go run . -self-test

echo "gateway checks passed"
