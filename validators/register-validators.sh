#!/usr/bin/env bash
#
# register-validators.sh — one-command launch-day registration for the
# forked-social PoS validator set (validators 1-3).
#
# Self-contained launcher for backend/scripts/pos/validator_bootstrap, run in
# the forked-builder container (host-native `go run` is impossible — libvips,
# and CGO_CFLAGS="-std=gnu11" is required for the bundled BLS C sources).
# Per validator it funds (5,125,000 coins), registers, and stakes (5,000,000
# coins) BEFORE the block-~145 epoch-1 snapshot (PoS cutover at block 300).
#
# Secrets come ONLY from the environment. They are never echoed and this
# script NEVER enables `set -x`. Full runbook: backend/validators/README.md
#
# Exit codes: 0 success; 1 usage error, missing env, declined confirmation,
#   height >= 145 hard abort, or bootstrap failure.

set -euo pipefail

SCRIPT_NAME="$(basename "$0")"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"

BUILDER_IMAGE="forked-builder"
DEFAULT_API_URL="http://127.0.0.1:42001"

# Fork timing: epoch-1 validator-set snapshot at block ~145 (DefaultEpoch
# DurationNumBlocks = 144); PoS cutover at block 300; 2s blocks.
DEADLINE_BLOCK_HEIGHT=120
EPOCH_SNAPSHOT_BLOCK_HEIGHT=145
CUTOVER_BLOCK_HEIGHT=300

# Launch identities (PUBLIC keys only — mnemonics are env-injected, never in
# this file; see backend/validators/README.md).
VALIDATOR_PUBKEYS=(
  "FS13wSsnnLhYeVfjNyvc8HE6f7iWGG6bHJx6Kb5wKR513QJrNQdq5W"
  "FS13vueAs3CmmPBjm5PnsyjbiUHegNHyNXmCcLdu1A6hh9tc2qHB74"
  "FS13w9xhKm1vbvDWQKSvZPCqNkQMLFDurJ2TRNw5zjsod4kM1DS1wE"
)

REQUIRED_ENV=(
  MINER_SEED
  VALIDATOR1_SEED
  VALIDATOR1_BLS_SEED
  VALIDATOR2_SEED
  VALIDATOR2_BLS_SEED
  VALIDATOR3_SEED
  VALIDATOR3_BLS_SEED
)

# Everything forwarded into the container with explicit -e NAME (unset vars
# are skipped by podman, so the optional DOMAIN overrides can be listed
# unconditionally).
FORWARDED_ENV=(
  "${REQUIRED_ENV[@]}"
  VALIDATOR1_DOMAIN
  VALIDATOR2_DOMAIN
  VALIDATOR3_DOMAIN
)

usage() {
  cat <<EOF
Usage: ./$SCRIPT_NAME [--dry-run] [-y|--yes]

Registers + stakes forked-social validators 1-3 before the block-~145
epoch-1 snapshot, in the $BUILDER_IMAGE container (built on first use if
missing). Per validator: fund 5,125,000 coins from the miner, register-as-
validator, stake 5,000,000 coins. Runbook: backend/validators/README.md.

Required environment variables (values are NEVER printed):
  MINER_SEED              fork miner mnemonic (funds every validator)
  VALIDATOR1_SEED         validator 1 DeSo mnemonic (signs register + stake)
  VALIDATOR1_BLS_SEED     validator 1 BLS mnemonic (voting key)
  VALIDATOR2_SEED         validator 2 DeSo mnemonic
  VALIDATOR2_BLS_SEED     validator 2 BLS mnemonic
  VALIDATOR3_SEED         validator 3 DeSo mnemonic
  VALIDATOR3_BLS_SEED     validator 3 BLS mnemonic

Optional environment variables:
  VALIDATOR{1,2,3}_DOMAIN registration domain overrides
                          (defaults: node.forked.social:42100,
                           node.forked.social:42200,
                           node.forked.social:42300)
  API_URL                 backend API base URL
                          (default: $DEFAULT_API_URL — the seed-host loopback)

Flags:
  --dry-run               pass --dry-run to the bootstrap tool: derive keys
                          and print the planned transactions, submit NOTHING
  -y, --yes               non-interactive: pre-approve the warnings that
                          would otherwise prompt (unreachable API, height
                          past deadline 120). Height >= 145 ALWAYS aborts.
  -h, --help              print this usage and exit

Example (on the seed host, after Step 1 of the runbook):
  export MINER_SEED='...' VALIDATOR1_SEED='...' VALIDATOR1_BLS_SEED='...' \\
         VALIDATOR2_SEED='...' VALIDATOR2_BLS_SEED='...' \\
         VALIDATOR3_SEED='...' VALIDATOR3_BLS_SEED='...'
  ./backend/validators/$SCRIPT_NAME            # or --dry-run first
EOF
}

say() { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# confirm <prompt> — assumes yes under -y/--yes; a failed read (EOF,
# non-interactive stdin) counts as NO.
confirm() {
  if [ "$ASSUME_YES" = true ]; then
    return 0
  fi
  local reply=""
  read -r -p "$1 [y/N]: " reply || reply=""
  case "$reply" in
    [yY] | [yY][eE][sS]) return 0 ;;
    *) return 1 ;;
  esac
}

# ---------------------------------------------------------------- flags ------

DRY_RUN=false
ASSUME_YES=false

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=true ;;
    -y | --yes) ASSUME_YES=true ;;
    -h | --help) usage; exit 0 ;;
    *) printf 'unknown option: %s\n\n' "$1" >&2; usage >&2; exit 1 ;;
  esac
  shift
done

API_URL="${API_URL:-$DEFAULT_API_URL}"

# ------------------------------------------------------------ preflight ------

command -v podman >/dev/null 2>&1 || die "podman not found in PATH"
[ -d "$REPO_ROOT/backend" ] || die "could not derive the repo root from $SCRIPT_DIR (expected $REPO_ROOT/backend to exist)"

missing=()
for var in "${REQUIRED_ENV[@]}"; do
  if [ -z "${!var:-}" ]; then
    missing+=("$var")
  fi
done
if [ "${#missing[@]}" -gt 0 ]; then
  {
    say "Missing required environment variables (names only — values never printed):"
    printf '  %s\n' "${missing[@]}"
    say ""
    usage
  } >&2
  exit 1
fi

# ------------------------------------------------ height guard (API check) ----

# fetch_block_height — echoes the node's current BlockHeight from
# POST /api/v0/get-app-state (field "BlockHeight", a uint32); fails (empty /
# non-zero) if the API is unreachable or the response is not parseable.
fetch_block_height() {
  local body digits
  if ! body="$(curl -sS -m 10 -X POST "$API_URL/api/v0/get-app-state" \
    -H 'Content-Type: application/json' -d '{}' 2>/dev/null)"; then
    return 1
  fi
  digits="$(printf '%s' "$body" | grep -oE '"BlockHeight"[[:space:]]*:[[:space:]]*[0-9]+' | head -n 1 | tr -cd '0-9')"
  [ -n "$digits" ] || return 1
  printf '%s' "$digits"
}

say "==> Height guard (API: $API_URL)"
height="$(fetch_block_height)" || height=""
if [ -n "$height" ]; then
  say "Seed node reports block height $height (epoch-1 snapshot at ~$EPOCH_SNAPSHOT_BLOCK_HEIGHT, PoS cutover at $CUTOVER_BLOCK_HEIGHT)."
  if [ "$height" -ge "$EPOCH_SNAPSHOT_BLOCK_HEIGHT" ]; then
    {
      banner_line() { printf '* %-70s *\n' "$1"; }
      say ""
      say "**************************************************************************"
      banner_line "           !!! REGISTRATION WINDOW ALREADY CLOSED !!!"
      banner_line ""
      banner_line "Block height $height is at/past the epoch-1 snapshot (block"
      banner_line "~$EPOCH_SNAPSHOT_BLOCK_HEIGHT): registrations submitted now will NOT"
      banner_line "make it into the first PoS validator set, and the chain can stall"
      banner_line "at the block-$CUTOVER_BLOCK_HEIGHT cutover."
      banner_line ""
      banner_line "Recommended action: wipe and relaunch per the runbook"
      banner_line "(backend/validators/README.md): stop the seed, clear its data"
      banner_line "dir, restart from genesis, and register while the height is low."
      say "**************************************************************************"
    } >&2
    exit 1
  fi
  if [ "$height" -gt "$DEADLINE_BLOCK_HEIGHT" ]; then
    warn "block height $height is past deadline $DEADLINE_BLOCK_HEIGHT — only $((EPOCH_SNAPSHOT_BLOCK_HEIGHT - height)) blocks (~$((2 * (EPOCH_SNAPSHOT_BLOCK_HEIGHT - height)))s at 2s/block) remain before the epoch-1 snapshot at ~$EPOCH_SNAPSHOT_BLOCK_HEIGHT; registrations may be too late."
    confirm "Register anyway?" || die "aborted at height $height (past deadline $DEADLINE_BLOCK_HEIGHT; re-launch the chain per the runbook instead)"
  fi
else
  warn "could not fetch the block height from $API_URL — is the seed node up?"
  confirm "Continue without a height check?" || die "aborted: API unreachable at $API_URL"
fi

# --------------------------------------------------------- builder image -----

say "==> Builder image"
if podman image exists "$BUILDER_IMAGE" >/dev/null 2>&1; then
  say "Using existing '$BUILDER_IMAGE' image."
else
  say "Builder image '$BUILDER_IMAGE' not found — building it from $REPO_ROOT (one-time, a few minutes)."
  podman build --target backend -f "$REPO_ROOT/backend/Dockerfile" -t "$BUILDER_IMAGE" "$REPO_ROOT"
fi

# ------------------------------------------------------------- bootstrap -----

env_args=()
for var in "${FORWARDED_ENV[@]}"; do
  env_args+=(-e "$var")
done

bootstrap_args=(--api-url "$API_URL")
if [ "$DRY_RUN" = true ]; then
  bootstrap_args+=(--dry-run)
  say "==> Running validator bootstrap (DRY-RUN: nothing will be submitted)"
else
  say "==> Running validator bootstrap (fund -> register -> stake, validators 1-3)"
fi

podman run --rm --network=host \
  -e CGO_CFLAGS="-std=gnu11" \
  "${env_args[@]}" \
  -v "$REPO_ROOT":/src -w /src/backend \
  "$BUILDER_IMAGE" \
  go run ./scripts/pos/validator_bootstrap \
  "${bootstrap_args[@]}"

# ---------------------------------------------------------- post-run info ----

say "==> Post-run"
height="$(fetch_block_height)" || height=""
if [ -n "$height" ]; then
  say "Current block height: $height — PoS cutover happens at block $CUTOVER_BLOCK_HEIGHT; make sure the validator containers are up before then."
else
  warn "could not re-query the block height at $API_URL"
fi
say ""
say "Verification curls (once registered + staked, expect Status ACTIVE and"
say "TotalStakeAmountNanos = 5000000000000000 per validator):"
for pubkey in "${VALIDATOR_PUBKEYS[@]}"; do
  say "  curl -s $API_URL/api/v0/validators/$pubkey"
done
say "  curl -s $API_URL/api/v0/current-epoch-progress"
say ""
say "Next: put each validator's BLS mnemonic (the SAME one injected here as"
say "VALIDATOR{N}_BLS_SEED) into /opt/validatorN/validatorN.env as"
say "POS_VALIDATOR_SEED, then start the containers — Step 4 of"
say "backend/validators/README.md."
