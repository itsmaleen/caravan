# caravan

Drive for devs: one manifest, identical dev workspaces on every machine.
Single static Go binary — the only things it shells out to are `git`, `ssh`,
and `tar`. Secrets are age-encrypted natively (no sops/age install needed);
`mise`/`direnv` are used when present, skipped when not.

```
caravan init          # walk ~/code, discover git repos, draft caravan.toml
caravan up            # clone/pull every repo, decrypt secrets → .env, mise install
caravan status        # branch, ahead/behind, dirty, .env, toolchain per repo
caravan sync          # bidirectional folder sync to another machine over ssh
caravan sync --watch  # continuous sync: ~1s reaction to changes on either side
caravan daemon ...    # install | uninstall | status — sync as a launchd service
caravan secrets ...   # init | set | show | add-machine
caravan doctor        # diagnose environment, repos, sync pairs, secrets
```

## Install

**Build from source (recommended for now):**
```
git clone git@github.com:itsmaleen/caravan.git && cd caravan
make build          # outputs ./caravan
make install        # builds + installs to ~/.local/bin/caravan (override: PREFIX=/usr/local)
```

**Local install from a built release:**
```
make release                  # produces dist/ tarballs + checksums
bash scripts/install.sh       # installs from dist/ → ~/.local/bin/caravan
```
`install.sh` also accepts `--url URL` or `$CARAVAN_BASE_URL` to fetch from a
hosted release once one exists. See `scripts/install.sh` header for details.

**Homebrew:** coming once the project has a public GitHub home.
See `packaging/homebrew/caravan.rb` for the formula template and instructions.

## Setup wizard

New to caravan? Let an AI coding agent walk you through configuration:

```
caravan setup
```

`caravan setup` detects an AI agent already installed on your machine (Claude
Code, Gemini CLI, Codex, opencode, or cursor-agent), gathers machine context
(OS, hostname, manifest path, `caravan doctor` output, tool inventory), and
hands the agent a crafted prompt so it interviews you and drives every command
— from `caravan init` through `caravan doctor` all-green. Inference runs on
your existing subscription; it costs the caravan project nothing.

Flags: `--agent NAME` (force a specific agent), `--headless` (non-interactive
one-shot), `--print-prompt` (print the assembled prompt and exit — paste into
any chat UI, or use to debug context gathering).

## Manifest

Resolution order: `-f` flag > `$CARAVAN_MANIFEST` > `~/.config/caravan/caravan.toml`.

```toml
version = 1

[workspace]
root = "~/code"

[[repos]]
name   = "myrepo"
url    = "git@github.com:me/myrepo.git"
path   = "myrepo"        # relative to workspace.root
branch = "main"
sparse = true            # git clone --filter=blob:none --sparse

[secrets]
file = "secrets.enc.json"  # age-encrypted, relative to manifest dir

[toolchain]
mise = true                # run `mise install` per repo when mise is on PATH

[[sync]]
name     = "notes"
local    = "~/notes"
remote   = "me@other-mac.tailnet.ts.net:~/notes"  # or "local:/Volumes/usb/notes"
exclude  = ["*.tmp"]       # .git, node_modules, .DS_Store etc. always excluded
checksum = false           # true: sha256 change detection (catches edits that
                           # preserve size+mtime, at the cost of hashing every scan)
delta_min_bytes = 0        # files >= this use rsync delta transfer instead of tar
                           # (0 = default 8 MiB, -1 = always whole-file)
```

## Continuous sync (daemon)

`caravan daemon install <name> --interval 5s` writes a LaunchAgent that runs
`caravan sync --watch` for that entry — survives logout/reboot, logs to
`~/Library/Logs/caravan/<name>.log`. `daemon status` shows plist/running/PID/
last-sync; `daemon uninstall` removes it. A per-entry lock means a daemon and a
manual `caravan sync` never race the same state (the loser skips politely).

## Sync model

Snapshot-based three-way sync (a small Mutagen): after each successful pass the
observed state of both sides is stored as the base under
`~/.config/caravan/sync-state/`. Each run scans both sides, diffs against the
base, and plans per path: new files copy across, deletions propagate,
modification beats deletion, and when both sides changed the newer mtime wins
(tie → local). Permission-only changes (chmod) propagate as their own cheap
action. Conflict losers are never destroyed: the overwritten side's
content is backed up to `~/.config/caravan/conflicts/<entry>/` on whichever
machine lost (pruned after 7 days). Files above `delta_min_bytes` transfer via
rsync so a small edit to a large file only moves changed blocks (40 MB file,
1 KB append: ~1.8 s vs ~12 s full push). Receives are atomic: tar transfers
land in a `.caravan-staging` dir and rename into place, so a killed transfer
can never leave a truncated file in the tree, and the base refuses to record
size-mismatched pairs. `.git` and dependency dirs are hard-excluded.

The remote side is scanned by running `caravan scan` over ssh. If the binary is
missing on the remote and the platform matches, caravan copies itself to the
remote's `~/.local/bin` automatically — a fresh machine needs nothing but ssh
access. A version handshake on every scan re-pushes the binary whenever the
remote is stale, so remotes never drift after the first bootstrap.

Every ssh call is multiplexed (ControlMaster, 60s persist), so a warm no-op
scan costs ~50 ms. Watch mode pairs a 1s local poll with a remote long-poll
(`scan --wait`): the remote caravan holds the connection open and returns the
moment its side changes, so either side's edits propagate in about a second
without per-tick scan traffic.

Measured (MacBook ↔ Mac mini over Tailscale): 1003 files / ~15 MB initial push
in ~9 s, warm no-op sync 0.05 s, watch-mode propagation 0.6–1.2 s either
direction.

## Secrets

One age key per machine at `~/.config/caravan/age.key`. The encrypted store
carries its own recipient list, so adding a machine is: run `caravan secrets
init` there, then `caravan secrets add-machine <its-pubkey>` on any machine
that already has access, then sync/copy the `.enc.json` file. `caravan up`
materializes per-repo `.env` files (managed keys win, unmanaged lines are
preserved).

## Testing

```
go test ./...                                      # unit/integration tests
CARAVAN_BIN=$(pwd)/caravan ./test/e2e-sync.sh      # cross-device e2e (needs ssh to the mini)
CARAVAN_BIN=$(pwd)/caravan ./test/edge-sync.sh     # 10-round edge-case probe (unicode, type flips, …)
CARAVAN_BIN=$(pwd)/caravan ./test/stress-sync.sh   # 1000-file cross-device stress/perf
CARAVAN_BIN=$(pwd)/caravan ./test/topology-sync.sh # 3-replica hub-and-spoke across both machines
CARAVAN_BIN=$(pwd)/caravan ./test/chaos-sync.sh    # kill -9 mid-transfer, mux drops, corrupt state, 10k files
CARAVAN_BIN=$(pwd)/caravan ./test/setup-wizard.sh  # agent drives `caravan setup` to a working config (needs claude)
```

The e2e script exercises the real two-machine loop: initial push, edits in both
directions, deletion propagation, exclude handling, and both conflict
orientations, asserting sha256 tree parity after every round.

## Status / roadmap

v0.5.0: Tier 0 provisioning + Tier 1 sync from PLAN.md fully built and
validated across a MacBook ↔ Mac mini pair — provisioning (partial clones,
age secrets, mise/direnv), agent-sandbox recipe (~1.3 s container cold start),
bidirectional sync with self-updating remotes, checksum mode, chmod
propagation, conflict-loser backups, rsync delta transfer, launchd daemon,
hub-and-spoke topologies, `doctor` diagnostics, and release tooling.
Deliberately not built (Tier 2+, see `../PLAN.md`): FUSE/VFS anything,
conflict UI, block-level custom delta, non-macOS daemons.
