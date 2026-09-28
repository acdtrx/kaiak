# syntax=docker/dockerfile:1
#
# The sample control plane's image, for demos and validation. Build context: the
# repository root (the control/ workspace; kaiak-control carries its schemas).
#
#   docker buildx build --platform linux/amd64 -f control/sample/Dockerfile -t kaiak-sample .
#
# Only runtime dependencies are installed, so logs are JSON: KAIAK_LOG_FORMAT=text
# needs pino-pretty, a development dependency. Mount a directory holding the config
# and name the file with KAIAK_SAMPLE_CONFIG: the sample watches the file's
# directory, which a single-file bind mount would hide from edits made by renaming.

FROM node:26-slim
ARG VERSION=dev
LABEL org.opencontainers.image.title="kaiak-sample" \
      org.opencontainers.image.description="kaiak sample control plane" \
      org.opencontainers.image.version="${VERSION}"
ENV NODE_ENV=production \
    KAIAK_LOG_FORMAT=json \
    KAIAK_SAMPLE_LISTEN=0.0.0.0:8090
WORKDIR /app/control
COPY control/package.json control/package-lock.json ./
COPY control/kaiak-control/package.json kaiak-control/
COPY control/sample/package.json sample/
RUN npm ci --omit=dev --no-audit --no-fund && npm cache clean --force
COPY control/kaiak-control/src kaiak-control/src
COPY control/kaiak-control/schema kaiak-control/schema
COPY control/sample/src sample/src
# Numeric, so Kubernetes can verify runAsNonRoot without a runAsUser (1000 is the base
# image's node user).
USER 1000:1000
EXPOSE 8090
CMD ["node", "sample/src/main.ts"]
