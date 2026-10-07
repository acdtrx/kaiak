# syntax=docker/dockerfile:1
#
# The gateway image: one static binary on a distroless non-root base
# (docs/TECH-STACK.md, Build & image). Build context: gateway/.
#
#   docker buildx build --platform linux/amd64 -t kaiak gateway
#
# No config is baked in: set KAIAK_CONTROL_URL + KAIAK_CONTROL_TOKEN (control-plane
# mode), or mount one read-only and name it with KAIAK_CONFIG_FILE (file mode). The
# gateway writes nothing, so it runs on a read-only root filesystem with no volume.
#
# VERSION (git describe, from scripts/build-images.sh) is linked into the binary and
# reported by kaiak_build_info.
#
# The fakebackend target is test tooling for scripts/smoke-images.sh, never published.

# Keep in step with the go line of go.mod; scripts/build-images.sh passes go.mod's.
ARG GO_VERSION=1.27.1

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X kaiak/internal/metrics.version=${VERSION}" \
      -o /out/kaiak ./cmd/kaiak

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS fakebackend-build
WORKDIR /src
COPY . .
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/fakebackend ./internal/fakebackend/cmd/fakebackend

FROM gcr.io/distroless/static-debian13:nonroot AS fakebackend
COPY --from=fakebackend-build /out/fakebackend /fakebackend
USER 65532:65532
EXPOSE 8000
ENTRYPOINT ["/fakebackend", "-addr", "0.0.0.0:8000"]

FROM gcr.io/distroless/static-debian13:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="kaiak" \
      org.opencontainers.image.description="kaiak LLM gateway" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/kaiak /kaiak
# Numeric, so Kubernetes can verify runAsNonRoot without a runAsUser (65532 is the
# base image's nonroot user).
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/kaiak"]
