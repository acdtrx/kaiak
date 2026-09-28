#!/usr/bin/env bash
# Smoke test of the two container images on a Docker context's daemon: the gateway in
# file mode, then the sample control plane with a second gateway in control-plane mode,
# each answering a chat completion from the fake backend (the gateway Dockerfile's
# fakebackend target, built here and never pushed). Both gateways run the stateless
# default: a read-only root filesystem and no volume (file mode mounts only its config,
# read-only); the control-plane one gets only KAIAK_CONTROL_URL and KAIAK_CONTROL_TOKEN
# and must deliver its usage on docker stop. kaiak_build_info must report the image's
# version label. Everything it creates carries the label kaiak-smoke=<run> and is
# removed on every exit path. Needs only docker locally: the key is minted by the sample
# image, and requests are made from a container on the test network, so no port is
# published. Works from any directory.
#
#   scripts/smoke-images.sh --gateway <image> --sample <image>
#                           [--context <docker-context>] [--builder <buildx-builder>]
#                           [--platform <os/arch>]
#
# The platform (default linux/amd64) is the images' and the test backend's: the images
# run on the daemon's own architecture, so it matches the daemon.
#
# Context and builder default as in scripts/build-images.sh (KAIAK_DOCKER_CONTEXT,
# else the current Docker context; KAIAK_BUILDX_BUILDER, else the context's default
# builder).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

context="${KAIAK_DOCKER_CONTEXT:-$(docker context show)}"
builder="${KAIAK_BUILDX_BUILDER:-}"
platform=linux/amd64
gateway_image=""
sample_image=""

usage() {
	echo "usage: scripts/smoke-images.sh --gateway <image> --sample <image> [--context <docker-context>] [--builder <buildx-builder>] [--platform <os/arch>]" >&2
	exit 2
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--gateway) gateway_image="${2:?}"; shift ;;
	--sample) sample_image="${2:?}"; shift ;;
	--context) context="${2:?}"; shift ;;
	--builder) builder="${2:?}"; shift ;;
	--platform) platform="${2:?}"; shift ;;
	-h | --help) usage ;;
	*) echo "smoke-images: unknown argument $1" >&2; usage ;;
	esac
	shift
done
[[ -n "$gateway_image" && -n "$sample_image" ]] || usage

docker=(docker --context "$context")
run="kaiak-smoke-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
label="kaiak-smoke=$run"
net="$run"
fake_image="kaiak-smoke-fakebackend:$run"
token="smoke-token"
tmp="$(mktemp -d)"

cleanup() {
	local status=$?
	set +e
	if [[ $status -ne 0 ]]; then
		for id in $("${docker[@]}" ps -aq --filter "label=$label"); do
			echo "--- logs of $("${docker[@]}" inspect --format '{{.Name}}' "$id")" >&2
			"${docker[@]}" logs --tail 40 "$id" >&2
		done
	fi
	echo "==> cleanup ($run)"
	"${docker[@]}" ps -aq --filter "label=$label" | xargs -r "${docker[@]}" rm -f >/dev/null
	"${docker[@]}" network ls -q --filter "label=$label" | xargs -r "${docker[@]}" network rm >/dev/null
	"${docker[@]}" volume ls -q --filter "label=$label" | xargs -r "${docker[@]}" volume rm >/dev/null
	"${docker[@]}" image rm "$fake_image" >/dev/null 2>&1
	rm -rf "$tmp"
	exit "$status"
}
trap cleanup EXIT

# Runs a Node script in a throwaway container of the sample image on the test network.
node_on_net() {
	"${docker[@]}" run --rm --label "$label" --network "$net" "$sample_image" node --input-type=module -e "$@"
}

# wait_ok <url>: polls until the URL answers 200 (30 s).
wait_ok() {
	node_on_net '
		const url = process.argv[1];
		const deadline = Date.now() + 30000;
		for (;;) {
			try { if ((await fetch(url)).status === 200) break; } catch {}
			if (Date.now() > deadline) { console.error(`no 200 from ${url} in 30 s`); process.exit(1); }
			await new Promise((r) => setTimeout(r, 250));
		}
		console.log(`200 ${url}`);
	' "$1"
}

# chat <gateway host>: one chat completion with the minted key; fails unless it is a 200
# carrying the fake backend's answer.
chat() {
	node_on_net '
		const [host, key] = process.argv.slice(1);
		const res = await fetch(`http://${host}:8080/v1/chat/completions`, {
			method: "POST",
			headers: { authorization: `Bearer ${key}`, "content-type": "application/json" },
			body: JSON.stringify({ model: "demo", messages: [{ role: "user", content: "Hello" }] }),
		});
		const body = await res.text();
		if (res.status !== 200 || !body.includes("Hello from the fake backend")) {
			console.error(`chat via ${host}: ${res.status} ${body}`);
			process.exit(1);
		}
		const { usage } = JSON.parse(body);
		console.log(`chat via ${host}: 200, usage ${JSON.stringify(usage)}`);
	' "$1" "$key"
}

# build_info <gateway host>: the kaiak_build_info line of the gateway's metrics.
build_info() {
	node_on_net '
		const text = await (await fetch(`http://${process.argv[1]}:9090/metrics`)).text();
		console.log(text.split("\n").find((line) => line.startsWith("kaiak_build_info")));
	' "$1"
}

# stopped_cleanly <container> <log message>: fails unless the container exited 0 and its
# log has a line with that message.
stopped_cleanly() {
	local code
	code="$("${docker[@]}" inspect --format '{{.State.ExitCode}}' "$1")"
	[[ "$code" == 0 ]] || { echo "smoke-images: $1 exited $code" >&2; exit 1; }
	# The whole log is read before matching: grep -q stops reading at its match, and a
	# log line after it would then fail the pipeline (SIGPIPE under pipefail).
	local log
	log="$("${docker[@]}" logs "$1" 2>&1)"
	grep -q "\"msg\":\"$2\"" <<<"$log" ||
		{ echo "smoke-images: no \"$2\" in the log of $1" >&2; exit 1; }
	echo "$1: exit 0, logged \"$2\""
}

echo "==> smoke $run on context $context"
echo "    gateway $gateway_image"
echo "    sample  $sample_image"

echo "==> build the fake backend (test only)"
builder_args=()
[[ -n "$builder" ]] && builder_args=(--builder "$builder")
"${docker[@]}" buildx build ${builder_args[@]+"${builder_args[@]}"} --platform "$platform" --load --quiet \
	--build-arg GO_VERSION="$(awk '$1 == "go" { print $2; exit }' "$root/gateway/go.mod")" \
	--target fakebackend -t "$fake_image" "$root/gateway" >/dev/null

"${docker[@]}" network create --label "$label" "$net" >/dev/null
"${docker[@]}" volume create --label "$label" "$run-config" >/dev/null

echo "==> mint a key and write the config"
keygen="$("${docker[@]}" run --rm --label "$label" "$sample_image" \
	node sample/src/keygen-cli.ts --id k-smoke --group demo-app)"
key="$(awk '$1 == "key:" { print $2 }' <<<"$keygen")"
entry="$(grep '^  "k-smoke": ' <<<"$keygen" | sed 's/^  //')"
[[ -n "$key" && -n "$entry" ]] || { echo "smoke-images: keygen printed no key: $keygen" >&2; exit 1; }
sed -e "s|http://127.0.0.1:8000/v1|http://$run-fake:8000/v1|" \
	-e "s|\"keys\": {}|\"keys\": { $entry }|" \
	"$root/examples/local-config.json" >"$tmp/config.json"
grep -q "k-smoke" "$tmp/config.json" || { echo "smoke-images: the key entry did not land in the config" >&2; exit 1; }
chmod 644 "$tmp/config.json"
"${docker[@]}" create --label "$label" --name "$run-seed" -v "$run-config:/config" "$gateway_image" >/dev/null
"${docker[@]}" cp "$tmp/config.json" "$run-seed:/config/config.json" >/dev/null
"${docker[@]}" rm "$run-seed" >/dev/null

"${docker[@]}" run -d --label "$label" --name "$run-fake" --network "$net" "$fake_image" -quiet >/dev/null

version="$("${docker[@]}" image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$gateway_image")"
[[ -n "$version" && "$version" != dev ]] || { echo "smoke-images: the gateway image has no version label" >&2; exit 1; }

echo "==> gateway, file mode (read-only root, only the config mounted)"
"${docker[@]}" run -d --label "$label" --name "$run-gw-file" --network "$net" --read-only \
	-v "$run-config:/config:ro" -e KAIAK_CONFIG_FILE=/config/config.json "$gateway_image" >/dev/null
wait_ok "http://$run-gw-file:9090/readyz"
chat "$run-gw-file"
info="$(build_info "$run-gw-file")"
echo "$info"
[[ "$info" == "kaiak_build_info{version=\"$version\","* ]] ||
	{ echo "smoke-images: kaiak_build_info does not report the image version $version" >&2; exit 1; }
"${docker[@]}" stop "$run-gw-file" >/dev/null
stopped_cleanly "$run-gw-file" "kaiak stopped"

echo "==> sample control plane + gateway, control-plane mode (read-only root, no volume)"
"${docker[@]}" run -d --label "$label" --name "$run-sample" --network "$net" \
	-v "$run-config:/config:ro" -e KAIAK_SAMPLE_CONFIG=/config/config.json \
	-e KAIAK_CONTROL_TOKEN="$token" "$sample_image" >/dev/null
wait_ok "http://$run-sample:8090/"
"${docker[@]}" run -d --label "$label" --name "$run-gw-control" --network "$net" --read-only \
	-e KAIAK_CONTROL_URL="http://$run-sample:8090" -e KAIAK_CONTROL_TOKEN="$token" "$gateway_image" >/dev/null
wait_ok "http://$run-gw-control:9090/readyz"
chat "$run-gw-control"
"${docker[@]}" stop "$run-gw-control" >/dev/null
stopped_cleanly "$run-gw-control" "usage flushed"

echo "smoke passed"
