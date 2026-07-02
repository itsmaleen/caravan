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
caravan sync --watch  # continuous sync (polling)
caravan secrets ...   # init | set | show | add-machine
```

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
name    = "notes"
local   = "~/notes"
remote  = "me@other-mac.tailnet.ts.net:~/notes"   # or "local:/Volumes/usb/notes"
exclude = ["*.tmp"]        # .git, node_modules, .DS_Store etc. always excluded
```

## Sync model

Snapshot-based three-way sync (a small Mutagen): after each successful pass the
observed state of both sides is stored as the base under
`~/.config/caravan/sync-state/`. Each run scans both sides, diffs against the
base, and plans per path: new files copy across, deletions propagate,
modification beats deletion, and when both sides changed the newer mtime wins
(tie → local). `.git` and dependency dirs are hard-excluded.

The remote side is scanned by running `caravan scan` over ssh. If the binary is
missing on the remote and the platform matches, caravan copies itself to the
remote's `~/.local/bin` automatically — a fresh machine needs nothing but ssh
access.

Flags come before positional args (stdlib flag parsing): `caravan sync -f m.toml name`.

## Secrets

One age key per machine at `~/.config/caravan/age.key`. The encrypted store
carries its own recipient list, so adding a machine is: run `caravan secrets
init` there, then `caravan secrets add-machine <its-pubkey>` on any machine
that already has access, then sync/copy the `.enc.json` file. `caravan up`
materializes per-repo `.env` files (managed keys win, unmanaged lines are
preserved).

## Testing

```
go test ./...                                   # 71 unit/integration tests
CARAVAN_BIN=$(pwd)/caravan ./test/e2e-sync.sh   # cross-device e2e (needs ssh to the mini)
```

The e2e script exercises the real two-machine loop: initial push, edits in both
directions, deletion propagation, exclude handling, and both conflict
orientations, asserting sha256 tree parity after every round.

## Status / roadmap

v0.1 (this) = Tier 0 provisioning + Tier 1 folder sync from PLAN.md, validated
across a MacBook ↔ Mac mini pair. Not built (deliberately): FUSE/VFS anything,
conflict UI, delta transfer, daemonization beyond `--watch`. See `../PLAN.md`
for tiers and kill criteria.
