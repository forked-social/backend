# forked-social PoS Validators — launch-day runbook

Operational docs for running the three forked-social Proof-of-Stake validators
(fork mainnet: `FS1…` keys, PoS cutover at block 300).

## Port layout — one server hosts everything

The seed node **and all three validators run on the SAME server**. Each node
therefore binds (and publishes) its own port pair:

| Node | P2P (public) | API (loopback only) | Registered domain (mesh dials) |
|---|---|---|---|
| Seed (`backend` container) | 42000 | 42001 | `node.forked.social:42000` |
| Validator 1 | 42100 | 42101 | `node.forked.social:42100` |
| Validator 2 | 42200 | 42201 | `node.forked.social:42200` |
| Validator 3 | 42300 | 42301 | `node.forked.social:42300` |

All four nodes share the single `node.forked.social` DNS record (one host,
pointing at that server IP) — the distinct ports keep the P2P listeners and
API endpoints apart. Validators dial the seed via
`CONNECT_IPS=node.forked.social:42000` (unchanged); the mesh dials each
validator at its registered domain/port above.

## File map (exact names — referenced by other docs)

| File | Purpose |
|---|---|
| `backend/validators/register-validators.sh` | **one-command launch**: preflight + height guard + builder container + bootstrap (see Step 3) |
| `backend/validators/docker-compose.validator1.yml` | validator 1 podman-compose (host `/opt/validator1/…`) |
| `backend/validators/docker-compose.validator2.yml` | validator 2 podman-compose (host `/opt/validator2/…`) |
| `backend/validators/docker-compose.validator3.yml` | validator 3 podman-compose (host `/opt/validator3/…`) |
| `backend/validators/validator{1,2,3}.env.example` | templates for `/opt/validatorN/validatorN.env` (the only secrets file, 0600) |
| `backend/scripts/pos/validator_bootstrap/main.go` | launch-day bootstrap tool (fund → register → stake, `--dry-run` supported) |
| `backend/scripts/pos/validator_registration.go` | single-validator manual tool for re-registrations / day-2 ops (env-driven, see below) |

Why the bootstrap tool lives in a sub-package: the sibling
`backend/scripts/pos/validator_registration.go` already declares
`package main` with its own `main()`; Go does not allow two per directory.

## Identity model — read this before touching any key

Each validator N∈{1,2,3} has **two mnemonics**, both runtime-injected, never
committed (the repo carries **public keys only**):

| Mnemonic | Env var used by the bootstrap script | Where it ends up on launch day |
|---|---|---|
| DeSo mnemonic (identity: signs register + stake txns) | `VALIDATORN_SEED` | nowhere on the validator host (operator vault) |
| BLS mnemonic (voting: signs PoS votes/blocks after cutover) | `VALIDATORN_BLS_SEED` | `/opt/validatorN/validatorN.env` → `POS_VALIDATOR_SEED` |

### ⚠️ THE IDENTITY REQUIREMENT (the #1 launch-day failure mode)

The mnemonic written into validator N's **`POS_VALIDATOR_SEED`** at container
start **MUST BE THE EXACT SAME MNEMONIC** the bootstrap script consumed as
**`VALIDATORN_BLS_SEED`** when it registered the validator on-chain. The BLS
voting public key registered in the `register-as-validator` transaction is
derived from `VALIDATORN_BLS_SEED`; the container derives its signer from
`POS_VALIDATOR_SEED`. If they differ, the node will not be able to sign
votes/proposals for its registered identity — the mesh will treat it as
inactive and the chain can stall at the cutover.

The script, the node, and `validator-key-generator/` all derive BLS keys with
the same lib calls (`lib.NewBLSKeystore` +
`lib.CreateValidatorVotingAuthorizationPayload`), so identical mnemonics are
*guaranteed* to produce identical keys everywhere.

### Launch identities (public keys — reference)

| # | DeSo pubkey (from `VALIDATORN_SEED`) | Registered domain | Default DNS host |
|---|---|---|---|
| 1 | `FS13wSsnnLhYeVfjNyvc8HE6f7iWGG6bHJx6Kb5wKR513QJrNQdq5W` | `node.forked.social:42100` | `node.forked.social` |
| 2 | `FS13vueAs3CmmPBjm5PnsyjbiUHegNHyNXmCcLdu1A6hh9tc2qHB74` | `node.forked.social:42200` | `node.forked.social` |
| 3 | `FS13w9xhKm1vbvDWQKSvZPCqNkQMLFDurJ2TRNw5zjsod4kM1DS1wE` | `node.forked.social:42300` | `node.forked.social` |

Miner (funder, holds the entire genesis supply): `FS13xm5oxJvGKd7194u5f2paA9WaaQpZSvz7Tzu8uXDngxromi3Lnv`.

## Timing constraints (why the order below matters)

- The seed mines 2s blocks; **epoch 1's validator-set snapshot is taken at
  block ~145 — about 5 minutes after the seed starts**.
- Validators must be funded + registered + staked **before block 145** to be
  in the first PoS validator set; the PoS cutover happens at block 300.
- Miss the window and the chain stalls at the cutover (no active validators).
- The bootstrap script enforces this with a deadline guard (default: abort
  past height 120; override with `--continue-past-deadline`), and
  `register-validators.sh` adds its own shell-level guard around it (warn +
  confirm past 120, hard-abort at ≥ 145) before the container is even
  started — but the guards cannot create time — start the bootstrap
  IMMEDIATELY after the seed is up, using the local-API tip below.

## Launch-day order of operations

### Step 0 — pre-flight (do all of this BEFORE Step 1; none of it is time-boxed)

1. **DNS**: a single A record for `node.forked.social`, pointing at the
   server IP (all four containers share that one host). The validator mesh
   dials the **registered domains** — the shared host on their distinct P2P
   ports (42100 / 42200 / 42300).
2. **Firewall**: inbound TCP **42000, 42100, 42200, 42300 open to the
   internet** on the server (P2P for seed + validators 1-3). The API ports
   42001 / 42101 / 42201 / 42301 stay loopback-only.
3. **Image**: CI (`build-and-publish.yml`) built + pushed
   `ghcr.io/forked-social/backend:<tag>`; `podman pull` it once on the server —
   the seed and all three validators run the same image.
4. **Bootstrap builder image** (Go toolchain + libvips; host-native builds are
   impossible — libvips), from the repo root once:
   ```sh
   cd ~/forked-social            # repo root that contains backend/ and core/
   podman build --target backend -f backend/Dockerfile -t forked-builder .
   ```
   (Step 3's `register-validators.sh` builds this image automatically on
   first use if it is missing — this manual step is just a head start.)
   NOTE: any ad-hoc `go build`/`go run` inside that container MUST set
   `CGO_CFLAGS="-std=gnu11"` (same as the Dockerfile) — the bundled onflow
   BLS C sources do not compile under the gcc-14+ default (`-std=gnu23`).
5. **Same server, per-validator dirs**: install the compose + env files, create
   data dirs (all under /opt on the one server):
   ```sh
   sudo install -d -o deploy -g deploy -m 0755 /opt/validator1/data/chain /opt/validator1/data/logs
   sudo install -m 0644 backend/validators/docker-compose.validator1.yml /opt/validator1/
   sudo install -m 0600 backend/validators/validator1.env.example /opt/validator1/validator1.env
   # (repeat with validator2/validator3 — same server, /opt/validatorN/... paths)
   ```
   (`POS_VALIDATOR_SEED` is still EMPTY at this point — filled in Step 4.)
6. **Rehearse**: run the bootstrap in dry-run mode (Step 3 with `--dry-run`)
   against any reachable API and confirm the derived DeSo pubkeys match the
   launch identities above with **no ⚠️ WARNING lines**.

### Step 1 — secrets into env (operator shell on the seed host)

```sh
# Keep mnemonics out of shell history: prefix each export line with a space
# (HISTCONTROL=ignorespace) or use `read -s`.
export MINER_SEED='<miner mnemonic>'
export VALIDATOR1_SEED='<v1 DeSo mnemonic>'
export VALIDATOR1_BLS_SEED='<v1 BLS mnemonic>'
export VALIDATOR2_SEED='<v2 DeSo mnemonic>'
export VALIDATOR2_BLS_SEED='<v2 BLS mnemonic>'
export VALIDATOR3_SEED='<v3 DeSo mnemonic>'
export VALIDATOR3_BLS_SEED='<v3 BLS mnemonic>'
# OPTIONAL domain overrides (defaults: node.forked.social:42100/:42200/:42300):
# export VALIDATOR1_DOMAIN=host1.example.com:42100
```

Documented env surface of the tool (there is nothing else):
`MINER_SEED`, `VALIDATOR{N}_SEED`, `VALIDATOR{N}_BLS_SEED`,
`VALIDATOR{N}_DOMAIN` (optional), `API_URL` (optional; `--api-url` wins).
Amounts are fixed: fund **5,125,000 coins** (stake + 2.5% fee/burn buffer),
stake **5,000,000 coins**, per validator. Totals: 3 × 5,125,000 =
**15,375,000 funded** (**15,000,000 staked**), leaving ~**14,625,000** of the
30M genesis supply with the miner. The 5M-coin stakes are a launch-security
posture against 50%+ attacks and are intended to be lowered via normal
unstake transactions as the network grows.

### Step 2 — start the seed node (this starts the ~5-minute clock)

```sh
# Existing seed machinery at /opt/backend (FORKNET=true baked into
# /opt/backend/.env by deploy.env; MINER_PUBLIC_KEYS mines the bootstrap window):
IMAGE=ghcr.io/forked-social/backend:<tag> \
  podman-compose -f /opt/backend/podman-compose.yml up -d

# Wait for readiness + watch the height climb (2s blocks):
curl -s http://127.0.0.1:42001/api/v0/health-check        # -> 200
curl -s -X POST http://127.0.0.1:42001/api/v0/get-app-state -H 'Content-Type: application/json' -d '{}'
# "BlockHeight" should start advancing immediately.
```

### Step 3 — register the validators (ONE COMMAND; IMMEDIATELY; local-API tip)

Run `register-validators.sh` **on the seed host** — it is self-contained:

```sh
cd ~/forked-social
./backend/validators/register-validators.sh             # real run
# rehearsal first (submits NOTHING):  ./backend/validators/register-validators.sh --dry-run
```

What it does (in order):

1. **Env preflight** — requires `MINER_SEED` and
   `VALIDATOR{1,2,3}_SEED` + `VALIDATOR{1,2,3}_BLS_SEED` (from Step 1) to be
   set and non-empty; names exactly the missing ones and exits 1 otherwise.
   It never prints any secret value.
2. **Height guard** — queries `$API_URL` (default
   `http://127.0.0.1:42001`, the seed-host loopback API):
   - API unreachable → warns and asks for confirmation
     (`-y`/`--yes` pre-approves for non-interactive use);
   - height > 120 → prints the loud block-145 deadline warning and requires
     explicit confirmation (also pre-approved by `-y`);
   - height >= 145 → **hard abort**: registrations would miss the first PoS
     validator set; it recommends the wipe-and-relaunch path. Not bypassable
     with `-y`.
3. **Builder image** — if `forked-builder` is missing it builds it
   (`podman build --target backend -f backend/Dockerfile -t forked-builder .`
   from the repo root, derived from the script's own location).
4. **Bootstrap run** — runs `podman run --rm --network=host` with `CGO_CFLAGS="-std=gnu11"`, all
   seeds forwarded via explicit `-e` names, the repo bind-mounted at `/src`,
   and `--api-url` from above; `--dry-run` passes through.
5. **Post-run** — re-queries and prints the current block height, reminds
   about the block-300 cutover, and prints the verification curls
   (`/api/v0/validators/<pk>` for the three launch pubkeys +
   `/api/v0/current-epoch-progress`).

Flags: `--dry-run`, `-y`/`--yes`, `-h`/`--help`. Optional env:
`VALIDATOR{1,2,3}_DOMAIN` overrides are forwarded when set; `API_URL` for a
different endpoint.

Notes:
- `--network=host` is what makes `http://127.0.0.1:42001` (the seed's
  loopback-published API) reachable from inside the container.
- Per validator the tool submits **funding → registration → stake** as three
  separate signed transactions, waiting for a fresh block between them; the
  three validators are processed sequentially. Expect the whole run to take
  ~1–2 minutes.
- Every step prints txids and pubkeys only — never mnemonics.
- From anywhere else, use `API_URL=https://node.forked.social
  ./backend/validators/register-validators.sh` (slower; TLS).

The final summary block lists, per validator: DeSo pubkey, BLS voting pubkey,
funding/registration/stake txids.

<details><summary>Fallback: the manual builder-container invocation
(equivalent to what <code>register-validators.sh</code> runs)</summary>

```sh
# Optional: print the plan first (submits NOTHING):
podman run --rm --network=host \
  -e CGO_CFLAGS="-std=gnu11" \
  -v "$HOME/forked-social":/src -w /src/backend \
  -e MINER_SEED -e VALIDATOR1_SEED -e VALIDATOR1_BLS_SEED \
  -e VALIDATOR2_SEED -e VALIDATOR2_BLS_SEED \
  -e VALIDATOR3_SEED -e VALIDATOR3_BLS_SEED \
  forked-builder go run ./scripts/pos/validator_bootstrap \
  --dry-run --api-url http://127.0.0.1:42001

# The real run — same command minus --dry-run:
podman run --rm --network=host \
  -e CGO_CFLAGS="-std=gnu11" \
  -v "$HOME/forked-social":/src -w /src/backend \
  -e MINER_SEED -e VALIDATOR1_SEED -e VALIDATOR1_BLS_SEED \
  -e VALIDATOR2_SEED -e VALIDATOR2_BLS_SEED \
  -e VALIDATOR3_SEED -e VALIDATOR3_BLS_SEED \
  forked-builder go run ./scripts/pos/validator_bootstrap \
  --api-url http://127.0.0.1:42001
```

The bootstrap tool's own deadline guard (abort past height 120, override with
`--continue-past-deadline`) still applies inside the container.
</details>

### Step 4 — start the validators (can start syncing any time before cutover)

On the server (all three validators run there — repeat per validator N),
first satisfy the identity requirement:

```sh
# /opt/validatorN/validatorN.env MUST get EXACTLY the mnemonic that the
# bootstrap consumed as VALIDATORN_BLS_SEED (v1 shown):
sudoedit /opt/validator1/validator1.env     # POS_VALIDATOR_SEED=<v1 BLS mnemonic>
sudo chmod 0600 /opt/validator1/validator1.env
```

Then start the containers (same image tag as the seed, one per validator):

```sh
podman pull ghcr.io/forked-social/backend:<tag>
IMAGE=ghcr.io/forked-social/backend:<tag> \
  podman-compose -f /opt/validator1/docker-compose.validator1.yml up -d
# (repeat with the validator2/validator3 compose files — same server)

# Sanity checks (validator 1 shown; ports 42100 / 42101 are v1's pair):
podman logs validator1 | less        # should log: fork mainnet selection, Protocol listening on port 42100, peer connection
curl -s http://127.0.0.1:42101/api/v0/health-check   # via ssh -L 42101:127.0.0.1:42101 <server>
ls /opt/validator1/data/logs        # glog output: backend.INFO etc.
```

The container wiring (already encoded in the compose files, do not change):
`FORKNET=true`, `CONNECT_IPS=node.forked.social:42000` (persistent link to the
seed — the fork has **no DNS seeds by design**),
`CHECKPOINT_SYNCING_PROVIDERS=https://node.forked.social`, per-validator
`PROTOCOL_PORT`/`API_PORT` (42100/42101, 42200/42201, 42300/42301 — matching
the registered domains) with P2P public and API loopback-only for admin. Each
validator uses its own host data dir (`/opt/validatorN/data/chain`,
`/opt/validatorN/data/logs`).

### Step 5 — what to check at the cutover (block 300)

```sh
# Epoch progress + leader schedule. ⚠️ the REAL route is
# /api/v0/current-epoch-progress (there is no get-current-epoch-progress):
curl -s https://node.forked.social/api/v0/current-epoch-progress | jq .

# Validator entries — expect the registered domain + voting pubkey and, once
# active, Status ACTIVE and TotalStakeAmountNanos = 5000000000000000:
curl -s https://node.forked.social/api/v0/validators/FS13wSsnnLhYeVfjNyvc8HE6f7iWGG6bHJx6Kb5wKR513QJrNQdq5W | jq .
# (same for the v2/v3 keys)

# Each validator's self-stake (5,000,000 coins = 5e15 nanos):
curl -s https://node.forked.social/api/v0/stake/FS13wSsnnLhYeVfjNyvc8HE6f7iWGG6bHJx6Kb5wKR513QJrNQdq5W/FS13wSsnnLhYeVfjNyvc8HE6f7iWGG6bHJx6Kb5wKR513QJrNQdq5W | jq .

# Height must keep climbing THROUGH and past 300 (PoS block production):
curl -s -X POST https://node.forked.social/api/v0/get-app-state -H 'Content-Type: application/json' -d '{}' | jq .BlockHeight
```

Healthy cutover signals: `CurrentView` in current-epoch-progress keeps
increasing; `CurrentLeader` rotates through the three validator pubkeys; the
seed's PoW miner stops producing at 300; each validator's log shows
Fast-HotStuff view advancement.

**If the chain stalls at/just after 300**: almost certainly a registration
missed block 145 (or a `POS_VALIDATOR_SEED` mismatch). Verify with the
`/api/v0/validators/…` calls; late registrations join at the next epoch
boundary — the chain remains stalled until an epoch snapshot picks up the
registered set, so prevention (the order above) is everything.

## Day-2 / single-validator ops (re-registration)

`backend/scripts/pos/validator_registration.go` is the SINGLE-validator
manual tool (fund → register → stake one validator — re-registrations,
replacing a key, day-2 fixes). It is **fully env-driven** — no code edits
are ever needed; it refuses to run (printing its env surface) when a
required variable is missing. Env vars (naming mirrors the bootstrap tool):

| Env var | Required | Purpose |
|---|---|---|
| `MINER_SEED` | **yes** | funded key paying for the registration (fork miner) |
| `VALIDATOR_SEED` | **yes** | the validator's DeSo mnemonic (signs register + stake) |
| `VALIDATOR_BLS_SEED` | **yes** | the validator's BLS mnemonic (voting key — same derivation as the bootstrap tool: `lib.NewBLSKeystore`; must match the container's `POS_VALIDATOR_SEED`) |
| `VALIDATOR_DOMAIN` | no | registration domain (default `node.forked.social:42100`) |
| `API_URL` | no | API base URL (default `https://node.forked.social`; `--api-url` wins) |

Amounts match the launch convention: fund **5,125,000 coins** (stake + 2.5%
fee/burn buffer), stake **5,000,000 coins**. Example (in the builder
container, from `backend/`):

```sh
MINER_SEED='<miner mnemonic>' \
VALIDATOR_SEED='<v2 DeSo mnemonic>' \
VALIDATOR_BLS_SEED='<v2 BLS mnemonic>' \
VALIDATOR_DOMAIN='node.forked.social:42200' \
podman run --rm --network=host -e CGO_CFLAGS="-std=gnu11" \
  -e MINER_SEED -e VALIDATOR_SEED -e VALIDATOR_BLS_SEED -e VALIDATOR_DOMAIN \
  -v "$HOME/forked-social":/src -w /src/backend \
  forked-builder go run ./scripts/pos --api-url http://127.0.0.1:42001
```

(For launch day use `register-validators.sh` — the tool above has NO
height-deadline guard of its own.)

## Security rules

- **Zero mnemonics in any repo file** — only public keys. All seeds are
  runtime-injected env vars (this repo's CI greps for them).
- `/opt/validatorN/validatorN.env` and the operator shell exports are the only
  places a mnemonic exists at rest/in transit; both 0600/manual.
- The bootstrap tool logs pubkeys, txids, heights, domains — nothing else.
