#!/usr/bin/env bash
# Builds the two container images for linux/amd64 — the gateway (gateway/Dockerfile)
# and the sample control plane (control/sample/Dockerfile) — into the daemon of a Docker
# context, and prints their references and sizes. With --push it then runs
# scripts/smoke-images.sh against the fresh images and pushes them only if it passes.
# Works from any directory.
#
#   scripts/build-images.sh [--push] [--context <docker-context>] [--builder <buildx-builder>]
#                           [--repo <registry/namespace>]
#
# Defaults (flags win over the environment): context KAIAK_DOCKER_CONTEXT or the
# current Docker context, builder KAIAK_BUILDX_BUILDER or the context's default
# builder. The repo (KAIAK_IMAGE_REPO or --repo) has no default: it names the
# registry and namespace the images are tagged for. Images: <repo>/kaiak and
# <repo>/kaiak-sample.
#
# Tags come from `git describe --tags`, a release tag's leading "v" dropped (v0.7.4 →
# 0.7.4): on a tag, <tag>; otherwise <tag>-<short sha> and <short sha>; always latest. A tree with uncommitted changes builds under
# <describe>-dirty and cannot be pushed. The version (the first tag) is also linked into
# the gateway binary, where kaiak_build_info reports it, and set as both images'
# org.opencontainers.image.version label. Registry login is the Docker context's own.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

context="${KAIAK_DOCKER_CONTEXT:-$(docker context show)}"
builder="${KAIAK_BUILDX_BUILDER:-}"
repo="${KAIAK_IMAGE_REPO:-}"
push=false
platform=linux/amd64

usage() {
	echo "usage: scripts/build-images.sh [--push] [--context <docker-context>] [--builder <buildx-builder>] [--repo <registry/namespace>]" >&2
	exit 2
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--push) push=true ;;
	--context) context="${2:?}"; shift ;;
	--builder) builder="${2:?}"; shift ;;
	--repo) repo="${2:?}"; shift ;;
	-h | --help) usage ;;
	*) echo "build-images: unknown argument $1" >&2; usage ;;
	esac
	shift
done

if [[ -z "$repo" ]]; then
	echo "build-images: name the registry and namespace with --repo or KAIAK_IMAGE_REPO" >&2
	usage
fi
builder_args=()
[[ -n "$builder" ]] && builder_args=(--builder "$builder")

cd "$root"

describe="$(git describe --tags --dirty)"
describe="${describe#v}"
sha="$(git rev-parse --short HEAD)"
tags=()
if [[ "$describe" == *-dirty ]]; then
	if $push; then
		echo "build-images: the tree has uncommitted changes; commit before --push" >&2
		exit 1
	fi
	version="$describe"
	tags=("$version")
elif git describe --tags --exact-match >/dev/null 2>&1; then
	version="$describe"
	tags=("$version" latest)
else
	version="$(git describe --tags --abbrev=0)"
	version="${version#v}-$sha"
	tags=("$version" "$sha" latest)
fi

go_version="$(awk '$1 == "go" { print $2; exit }' gateway/go.mod)"

docker=(docker --context "$context")

# build <image> <build context> [extra buildx args...]
build() {
	local image="$1" dir="$2"
	shift 2
	local args=()
	for tag in "${tags[@]}"; do args+=(-t "$repo/$image:$tag"); done
	echo "==> build $repo/$image:$version ($platform, context $context, builder ${builder:-default})"
	"${docker[@]}" buildx build ${builder_args[@]+"${builder_args[@]}"} --platform "$platform" --load \
		--build-arg VERSION="$version" "${args[@]}" "$@" "$dir"
}

build kaiak gateway --build-arg GO_VERSION="$go_version"
build kaiak-sample . -f control/sample/Dockerfile

gateway_image="$repo/kaiak:$version"
sample_image="$repo/kaiak-sample:$version"

echo
echo "images (context $context):"
for image in kaiak kaiak-sample; do
	for tag in "${tags[@]}"; do
		# image ls reports the unpacked size on both image stores; image inspect's .Size
		# is the compressed content on the containerd store.
		size="$("${docker[@]}" image ls --format '{{.Size}}' "$repo/$image:$tag")"
		printf '  %s:%s  %s unpacked\n' "$repo/$image" "$tag" "$size"
	done
done

if ! $push; then
	exit 0
fi

echo
"$root/scripts/smoke-images.sh" --context "$context" ${builder_args[@]+"${builder_args[@]}"} \
	--gateway "$gateway_image" --sample "$sample_image"

echo
for image in kaiak kaiak-sample; do
	for tag in "${tags[@]}"; do
		echo "==> push $repo/$image:$tag"
		"${docker[@]}" push --quiet "$repo/$image:$tag"
	done
done

echo
echo "pushed:"
for image in kaiak kaiak-sample; do
	digest="$("${docker[@]}" image inspect --format '{{join .RepoDigests " "}}' "$repo/$image:$version")"
	printf '  %s  tags: %s\n    %s\n' "$repo/$image" "${tags[*]}" "$digest"
done
