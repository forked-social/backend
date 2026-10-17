# Chain operations & troubleshooting (post-launch)

Day-2 runbook for the forked-1 chain host: restarts, image deploys, reboots,
and the failure modes that have actually been seen. Launch-day procedure
lives in [README.md](README.md); host hardening in
[`.env/setup/deploy-user-setup.md`](../../.env/setup/deploy-user-setup.md).

## 1. Service map & canonical runner

| Unit | Dir | Compose file | Ports (P2P/API) | Role |
|---|---|---|---|---|
| `backend.service` | `/opt/backend` | `podman-compose.yml` | 42000 / 42001 | **Seed** — non-consensus follower since the block-300 PoS cutover; API on 42001 is the canonical health/height oracle for all gates |
| `validator1.service` | `/opt/validator1` | `docker-compose.validator1.yml` | 42100 / 42101 | Consensus validator |
| `validator2.service` | `/opt/validator2` | `docker-compose.validator2.yml` | 42200 / 42201 | Consensus validator |
| `validator3.service` | `/opt/validator3` | `docker-compose.validator3.yml` | 42300 / 42301 | Consensus validator |

- **systemd units are the canonical runner** for boot and every restart:
  `sudo systemctl restart <unit>` runs the same compose `down` → `up -d`
  deploy.sh performs (minus the pull), as `deploy`, with `${IMAGE}` from
  `/opt/<svc>/.deploy`.
- **deploy.sh is the release orchestrator** — the only thing that ever pulls
  images. On this host (`SERVICE=backend` + `/opt/validator1` present) it
  runs the gated 4-service rollout of §3.
- Don't drive compose by hand around the units; a manual `up` bakes whatever
  `IMAGE`/env happens to be in the shell and desyncs `.deploy` (§6, stale-tag
  trap).

## 2. Restart policy

| Node | Restart cost | Rule |
|---|---|---|
| Seed | None (follower; no votes to miss) | Restart anytime |
| Any validator | Chain **pauses** until it rejoins — quorum is 3/3 (3·Cq ≥ 2N+1) | **ONE AT A TIME, gated** |

Per-validator gate sequence (all five must pass before touching the next
node):

```sh
systemctl is-active validator1.service                 # 1. unit active
sudo -u deploy podman ps --filter name=validator1      # 2. container Up
podman port validator1                                  # 3. BOTH mappings:
        #   0.0.0.0:42100 -> 42100/tcp  AND  127.0.0.1:42101 -> 42101/tcp
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:42101/api/v0/health-check
        # 4. -> 200   (ssh -L 42101:127.0.0.1:42101 <server> if remote)
curl -s -X POST http://127.0.0.1:42001/api/v0/get-app-state \
  -H 'Content-Type: application/json' -d '{}'          # 5. BlockHeight climbing
        #   = quorum restored, production live
```

- **Never restart two validator units concurrently** — two down = no quorum,
  no backoff can rescue view progress.
- Expected pause per gated validator restart: **~1–3 min** (view-timeout
  backoff + rejoin).
- A restarting validator is protected from eviction by the **48-epoch jail
  grace** — a brief outage costs availability, not the validator's seat.
- **Reboot = all three validators restart together.** Expect a minutes-long
  production pause; the chain self-heals as they rejoin (they retry
  `CONNECT_IPS` forever). Acceptable, but never *choose* it for a deploy —
  §3 exists to avoid exactly this.

## 3. Deploy procedure (image rollout)

1. CI: `build-and-publish.yml` pushes `ghcr.io/forked-social/backend:<tag>`.
2. Operator: GitHub → **Deploy backend** workflow (`deploy.yml`,
   `workflow_dispatch`) with the tag. CI ssh-es the two tokens
   `"<tag> backend"` to the deploy account; the wrapper execs
   `/opt/backend/deploy.sh`.
3. Server-side, deploy.sh then: chain pre-flight (all 3 validators running,
   height climbing) → seed rollout + **canary gates** (ports, health,
   height) → **validator1..3 ONE AT A TIME**: write `.deploy`, down/up,
   gates (running, ports, log env-assert `Protocol listening on port 421x0`,
   health 200, seed height climbing = quorum restored) → on any gate
   failure, roll THAT validator back to its previous ref and abort.
4. Prefer **explicit tags** (`v1.2.3`, git SHA) over mutable `latest` — a
   pinned tag makes `podman inspect` vs `.deploy` audits and rollbacks exact.
5. Attach a **required reviewer** to the GitHub `deploy` environment so the
   workflow cannot be dispatched unilaterally.

## 4. Reboot verification gates

After any host reboot, in order:

```sh
systemctl is-active backend.service validator{1,2,3}.service   # all active
sudo -u deploy podman ps                                       # all four Up
for c in backend validator1 validator2 validator3; do podman port $c; done
        # each: BOTH the 0.0.0.0:<p2p> and 127.0.0.1:<api> mappings
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:42001/api/v0/health-check  # 200
curl -s -X POST http://127.0.0.1:42001/api/v0/get-app-state \
  -H 'Content-Type: application/json' -d '{}'   # BlockHeight climbing = quorum back
```

**Rehearse the first reboot as a supervised drill** — an operator watching
the gates live, with the §2 sequence ready — before ever relying on unattended
reboots. The pause while all three validators rejoin is expected
(minutes, self-healing); what the drill proves is that nothing *stays* down.

## 5. Migration appendix (manual containers → units; ONE-TIME)

The chain was launched with ad-hoc `IMAGE=… podman-compose up -d` commands.
Handing each stack over to its unit (do once, then never again):

1. **Sync `.deploy` to what actually runs** — for each of
   backend/validator1/2/3: read the running ref, write it into `.deploy`
   (in place; the file is deploy-owned but root writes preserve that):
   ```sh
   podman inspect backend --format '{{.ImageName}}'   # per container
   sudo tee /opt/backend/.deploy >/dev/null <<<"IMAGE=$(podman inspect backend --format '{{.ImageName}}')"
   ```
2. **Install the four units** (root:root 0644 → `/etc/systemd/system/`,
   `daemon-reload`) and verify linger: `loginctl show-user deploy -p Linger`
   must say `yes`.
3. **Seed handover**: `podman rm -f backend` then
   `sudo systemctl enable --now backend.service` (fresh creation under the
   unit; data survives in `/opt/backend/data/…`).
4. **Validators one at a time**, each with the full §2 gate set before the
   next handover.
5. One-time only; afterwards prune leftovers (`podman image prune -f`,
   dangling images from the manual era) and treat systemd as the only
   runner (§1).

## 6. Troubleshooting — the nine durable lessons

| # | Symptom | Cause | Fix |
|---|---|---|---|
| 1 | Env-file edit "doesn't take"; `up` errors `creating container storage: … already in use` then starts with OLD env | Compose bakes `env_file`/environment at container **creation** only; `up` reuses the existing container | Always recreate: compose `down` + `up -d` (units and deploy.sh already do) |
| 2 | Empty `podman port <c>` output / API unreachable though unit "active" | Debian 13/trixie podman-compose 1.3.0 default **pod mode silently drops port publishes** | `x-podman.in_pod: false` in the compose files (primary) + the unit `ExecStartPost` port gate (loud regression) |
| 3 | Validators stuck at ONE inbound peer each | `ONE_INBOUND_PER_IP` default (anti-sybil) vs single-server mesh: every peer hairpins through rootless podman ⇒ same source IP | `ONE_INBOUND_PER_IP=false` in validator env (by design on this topology) |
| 4 | Chain stalled at/just after block 300 | Registration missed the block-~145 validator-set snapshot (or `POS_VALIDATOR_SEED` ≠ registered BLS key) | Prevention only (README launch order). Genesis is fixed: wipe data dirs on ALL nodes + relaunch from genesis |
| 5 | No `backend.INFO` files / logs "lost" | glog writes where `LOG_DIR` pointed at first emit; must be set before the first log line | `LOG_DIR=/logs` via env + bind mount `/opt/<svc>/data/logs:/logs` (core commit 7d792fa) — files live at `/opt/*/data/logs` |
| 6 | The four post-cutover ERROR floods are FIXED AT SOURCE — no longer "classify-and-ignore"; alerting rules built off these exact strings must be updated per-item (do NOT keep paging on them at ERROR severity): `exchange rate from blockchain.com: Input must not be empty.` and the DeSoDex pair `Error fetching exchange rate from DeSoDex: …Could not find profile entry for dusdc_` · `Error proposing block: … Stale view NNN` · `processBlockPoS: Block failed spam prevention check: : RuleErrorFailedSpamPreventionsCheck` · `updateCheckpointBlockInfo: … Get "…/api/v0/get-committed-tip-block-info…": connection refused` | Neither $DESO price source exists on the fork (no `dusdc_` DAOCoin profile; CLOUT-USD is a delisted pre-rename symbol with no listing); the stale-view proposal race is a normal steady-state race; spam-check fires on peer-received blocks when we're not the leader; the checkpoint providers pointed at `https://node.forked.social` (no reverse proxy serving it) and the FastHotStuff transition-check timer (`FastHotStuffConsensusTransitionCheckDuration`, default 60s) retried `tryTransitionToFastHotStuffConsensus` → `FastHotStuffConsensus.Start()` → `updateCheckpointBlockInfo` every tick, turning a transient startup failure into a sustained flood | **Rate oracles:** gated OFF by default; `BLOCKCHAIN_COM_RATE_ENABLED` / `DESO_DEX_RATE_ENABLED` env vars (committed) AND first-class `--blockchain-com-rate-enabled` / `--deso-dex-rate-enabled` flags, flag wins over env, the usual bool spellings (true/1/t/TRUE, case-insensitive) work and anything unparseable = disabled (flag wiring PENDING COMMIT). **Stale view:** no longer ERROR at default verbosity — logs at `glog.V(2)` with byte-identical message text, still greppable with `-v=2` (FIXED & COMMITTED core `61d1020`; effective once that core lands in a deployed image; genuine proposal failures stay at ERROR). **Spam-check:** now WARNING, not ERROR, for peer-received leader-schedule/QC rejections — message text unchanged, peer still disconnected; occasional WARNINGs are normal not-leader/view noise, but a **sustained** run is NOT benign: grep them and check for snapshot/leader-schedule divergence against the active leader — the WARNING count is the signal to watch (PENDING COMMIT core `lib/server.go`). **Checkpoint:** root-caused by config, not severity — providers now point at each node's own loopback API (seed `http://127.0.0.1:42001`; validator1/2/3 `http://127.0.0.1:42101`/`42201`/`42301`; PENDING COMMIT deploy.env + validator composes; apply per row 1 `down`+`up -d`); a transient first-start connection-refused remains expected because the blockchain makes its first checkpoint update before the API server is listening. `node.forked.social` becomes a usable *join-node* provider only once a properly secured host reverse proxy exists; `scripts/nodes/{n1,n1_test,hypersync}` still hard-code `--checkpoint-syncing-providers=https://node.forked.social` (remaining drift to fix) |
| 7 | Height frozen after "just a quick restart" of two validators | Quorum is 3/3 — two concurrent validator restarts = no quorum, production paused | §2: one at a time, gated; wait for height to resume before the next |
| 8 | Peers can't connect from the internet | Firewall: only SSH allowed | `ufw allow 22123` + P2P: `ufw allow 42000/tcp 42100/tcp 42200/tcp 42300/tcp`; API ports 42001/42101/42201/42301 stay loopback-only |
| 9 | After a unit restart a node runs an OLD image | `.deploy` tag drifted from what ran (manual `IMAGE=…` up) — the unit restarted the *tag*, not the image | `.deploy` is the invariant: never hand-set `IMAGE`, only deploy.sh writes it; audit `podman inspect <c> --format '{{.ImageName}}'` vs `.deploy` |

## 7. Kicksecure host notes

- **sdwdate, not NTP.** Timestamp anomalies (view/vote timestamps, TLS,
  journal correlation) → check `sdwdate-status` first; consensus misbehavior
  from clock skew shows up here.
- **permission-hardener** strips setuid from `newuidmap`/`newgidmap`, which
  kills rootless podman ID mapping. The `exactwhitelist` exceptions in
  `/etc/permission-hardener.d/` are already in place — verify after Kicksecure
  upgrades (`stat -c '%n %a' /usr/bin/newuidmap` → `4755`).
- **`--no-install-recommends` policy** means podman's Recommends are NOT
  auto-installed: `uidmap`, `passt` (pasta), `aardvark-dns` must be — and stay —
  explicitly installed; missing pasta = container creates but fails to start
  while compose still exits 0 (trust `podman ps`, not exit codes).
