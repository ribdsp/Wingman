# Wingman core — container image.
#
# Two stages: a full Go toolchain to compile, and a small runtime image holding the
# binary, its migrations and its config.
#
# Build from this directory:
#   docker build -t wingman/core:dev .
#   docker build -t wingman/core:v0.1.0 --build-arg VERSION=v0.1.0 .
#
# Note the difference from goal-engine/Dockerfile, which is distroless: core executes
# tool calls, and both sandbox backends need a program that distroless does not carry —
# `sh` for the local backend, the `docker` CLI for the docker one. NewLocal and NewDocker
# both look their binary up at startup, so a distroless core would refuse to boot rather
# than fail mysteriously; the fix is an image that has what the configured backend needs,
# not a backend that asks for less.

# ---- Build ---------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies resolve in their own layer, so editing source does not re-download the
# module graph on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION is stamped into the binary, logged at startup, and sent as the client name in
# every MCP handshake. It defaults to "dev" rather than to a guess.
ARG VERSION=dev

# CGO off, so the result runs on alpine without linking against its libc.
# -trimpath keeps build paths out of the binary, which also keeps this machine's
# directory layout out of any stack trace shipped to a user.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/core ./cmd/core

# ---- Runtime -------------------------------------------------------------
FROM alpine:3.22

# ca-certificates: reaching Anthropic, OpenAI, an MCP server or a declared HTTP API is
# the service's whole job, and none of it works without a trust store.
#
# docker-cli: the sandbox backend to run in front of other people. It is the client
# only — there is no daemon in this image, and the socket is the operator's to mount.
#
# The zone database is compiled into the binary (time/tzdata), so TIMEZONE resolves
# without the tzdata package.
RUN apk add --no-cache ca-certificates docker-cli

# An unprivileged user with a fixed uid. Fixed because the docker backend passes this
# process's uid to `docker run --user`, so files the agent writes in a workspace belong
# to a predictable owner on the host rather than to root.
RUN addgroup -g 65532 -S core \
 && adduser -u 65532 -S -G core -h /app core

WORKDIR /app

COPY --from=build /out/core /app/core

# Migrations travel with the binary that applies them: a running image and its schema
# should never be two independently deployed things.
COPY migrations /app/migrations

# The shipped config grants no tool and declares no server — see the comments in each
# file for why that is the state to ship. Mount your own over /app/config once you have
# tools of your own:
#   -v ./config:/app/config:ro
COPY config /app/config

# The default workspace root, writable by the runtime user.
#
# It is an absolute path at the top level on purpose. With the docker backend, core asks
# the host's daemon to mount a workspace, and the daemon resolves that path on the HOST —
# so the workspace root must be a bind mount that exists at the SAME absolute path inside
# this container and outside it. A named volume, or a bind mount at a different path
# inside, produces a container whose /workspace is empty or whose start fails. See
# deploy/docker-compose.yml, which mounts one path at one path for exactly this reason.
RUN mkdir -p /workspaces && chown core:core /workspaces
ENV SANDBOX_WORKSPACE_ROOT=/workspaces

# Secrets are not baked in. Every credential arrives through the environment; see
# .env.example, which is deliberately not copied into this image.

USER core:core
EXPOSE 8081

# The image has a shell, but the binary still probes itself: /readyz also checks the
# database, which is what makes this a readiness answer rather than a statement that the
# process exists.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/app/core", "healthcheck"]

ENTRYPOINT ["/app/core"]
