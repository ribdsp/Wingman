#!/usr/bin/env bash
# Provision Wingman core: the Rakazo fork this project builds on.
#
# Wingman is two services. This repository holds the goal engine, the part that
# decides when a goal has fallen behind. Core — the agent runtime, the channels,
# the tool sandbox — is Rakazo, Apache-2.0, used as a fork rather than vendored:
# it moves faster than this repo does, and its licence and history should stay
# intact and attributable.
#
# Usage:
#   scripts/fork-core.sh                        # clone upstream, track main
#   scripts/fork-core.sh --repo you/wingman-core --ref v1.4.0
#   scripts/fork-core.sh --dir core --ref 9f3a1c2
#
# Options:
#   --repo <owner/name>  GitHub repository to clone. Default: the upstream below.
#                        Use your own fork if you intend to push changes.
#   --ref  <tag|sha>     What to check out. Default: main, with a warning.
#   --dir  <path>        Where to put it. Default: core (gitignored on purpose —
#                        it is its own repository with its own history).
#   --https              Clone over HTTPS instead of SSH.
#
# See docs/fork-and-rebrand.md for what to change once it is here.

set -euo pipefail

UPSTREAM_REPO="elie222/rakazo"

REPO="$UPSTREAM_REPO"
REF="main"
DIR="core"
USE_HTTPS=0

die() {
  printf 'fork-core: %s\n' "$1" >&2
  exit 1
}

info() { printf '\033[36m==>\033[0m %s\n' "$1"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$1" >&2; }

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="${2:-}"; shift 2 || die "--repo needs a value" ;;
    --ref)  REF="${2:-}";  shift 2 || die "--ref needs a value" ;;
    --dir)  DIR="${2:-}";  shift 2 || die "--dir needs a value" ;;
    --https) USE_HTTPS=1; shift ;;
    -h|--help) awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0"; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[ -n "$REPO" ] || die "--repo cannot be empty"
[ -n "$REF" ] || die "--ref cannot be empty"
[ -n "$DIR" ] || die "--dir cannot be empty"

command -v git >/dev/null 2>&1 || die "git is not installed"

# Run from the repository root whichever directory the script was called from, so
# --dir is always relative to the same place.
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

# An existing core/ is left alone. It may hold your rebrand, and re-cloning over
# it would discard work that is not in this repository's history.
if [ -e "$DIR" ]; then
  die "$DIR already exists. Update it in place (cd $DIR && git fetch && git checkout <ref>), or remove it first if you are sure."
fi

if [ $USE_HTTPS -eq 1 ]; then
  REMOTE="https://github.com/${REPO}.git"
else
  REMOTE="git@github.com:${REPO}.git"
fi

if [ "$REPO" = "$UPSTREAM_REPO" ]; then
  warn "cloning upstream directly. Fork it on GitHub and pass --repo <you>/<name> if you plan to push your rebrand."
fi
if [ "$REF" = "main" ]; then
  warn "tracking main. Upstream is a live project: pass --ref <tag> to pin a release, so an upstream change cannot arrive in your deployment unreviewed."
fi

info "cloning ${REPO} into ${DIR}"
# A shallow clone of one ref: this is a deployment artifact, not a place to do
# upstream archaeology. `git fetch --unshallow` later if you need the history.
git clone --depth 1 --branch "$REF" "$REMOTE" "$DIR" 2>/dev/null || {
  # --branch does not accept a commit SHA, so fall back to fetching it directly.
  info "$REF is not a branch or tag; fetching it as a commit"
  git init --quiet "$DIR"
  git -C "$DIR" remote add origin "$REMOTE"
  git -C "$DIR" fetch --depth 1 origin "$REF" || die "cannot fetch $REF from $REPO"
  git -C "$DIR" checkout --quiet FETCH_HEAD
}

RESOLVED="$(git -C "$DIR" rev-parse HEAD)"

# What is actually deployed, written down. A tag can move; this cannot.
cat > "$DIR/.wingman-core-ref" <<EOF
# Provisioned by scripts/fork-core.sh. Not read by any code — this file exists so
# that "which core is running" has an answer that does not depend on a tag.
repo=$REPO
ref=$REF
commit=$RESOLVED
provisionedAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF

info "core is at ${DIR} (${RESOLVED})"
cat <<'EOF'

Next:
  1. Read docs/fork-and-rebrand.md. It lists what to rename and what to leave
     alone — upstream's LICENSE and copyright headers stay.
  2. Configure core from its own documentation, then bring it up.
  3. Point the goal engine at it: WINGMAN_CORE_BASE_URL in deploy/.env.
  4. Leave TRIGGER_DRY_RUN=true until you have read a few days of what the goal
     engine wanted to do. Nothing acts on your business until you turn it off.
EOF
