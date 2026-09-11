# Wingman goal engine — container image.
#
# Two stages: a full Go toolchain to compile, and a runtime image that contains
# the binary and nothing else. No shell, no package manager, no compiler — a
# service that runs autonomous agents should not carry the tools to install more.
#
# Build from this directory:
#   docker build -t wingman/goal-engine:dev .
#   docker build -t wingman/goal-engine:v0.1.0 --build-arg VERSION=v0.1.0 .

# ---- Build ---------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies resolve in their own layer, so editing source does not re-download
# the module graph on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION is stamped into the binary and logged at startup. It defaults to "dev"
# rather than to a guess: an image that claims a version it is not is worse than
# one that admits it does not know.
ARG VERSION=dev

# CGO is off so the result is a static binary that runs in an image with no libc.
# -trimpath keeps build paths out of it, which also keeps this machine's directory
# layout out of any stack trace shipped to a user.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/goal-engine ./cmd/goal-engine

# ---- Runtime -------------------------------------------------------------
# distroless/static carries CA certificates — needed to reach Wingman core and any
# https metric endpoint — and an unprivileged user, and little else. The zone
# database is compiled into the binary, so TIMEZONE still resolves here.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/goal-engine /app/goal-engine

# Migrations travel with the binary that applies them: a running image and its
# schema should never be two independently deployed things.
COPY migrations /app/migrations

# The shipped config is a working default, not a recommendation. Mount your own
# over /app/config once you have metrics and spending limits of your own:
#   -v ./config:/app/config:ro
COPY config /app/config

# Secrets are not baked in. Every credential arrives through the environment; see
# .env.example, which is deliberately not copied into this image.

USER nonroot:nonroot
EXPOSE 8080

# The image has no curl and no shell, so the binary probes itself. /readyz also
# checks the database, which is what makes this a readiness answer rather than a
# statement that the process exists.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/app/goal-engine", "healthcheck"]

ENTRYPOINT ["/app/goal-engine"]
