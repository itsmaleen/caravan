# Caravan v0.1 — Implementation Spec

Caravan is "drive for devs": one manifest (`caravan.toml`) that makes a developer's
workspace — repos, secrets, toolchains, and synced folders — appear identically on
every machine. Single static Go binary, zero runtime dependencies beyond git/ssh/tar.

Module: `caravan` (Go 1.23). External deps allowed: `github.com/BurntSushi/toml`,
`filippo.io/age`. Nothing else. No cobra — main.go (already written) dispatches to
package-level `Cmd*` functions.

## Global conventions

- Manifest path resolution (implemented in `internal/manifest`): `-f <path>` flag >
  `CARAVAN_MANIFEST` env > `~/.config/caravan/caravan.toml`.
- `~` expansion: every path read from the manifest or flags must go through
  `manifest.ExpandPath(s string) string` (expands leading `~/` via os.UserHomeDir).
- All commands accept `--dry-run` where they mutate anything: print what WOULD
  happen, touch nothing.
- Human output: aligned plain-text tables, no color libraries. Status glyphs:
  `✓` ok, `~` changed/pending, `✗` error, `-` skipped.
- Errors: fail soft per-item (one repo failing doesn't abort the fleet), fail hard
  on structural problems (missing manifest, unparseable TOML). Exit code 1 if any
  item failed.
- Logging: plain `fmt` to stdout; errors to stderr.

## caravan.toml schema

```toml
version = 1

[workspace]
root = "~/code"              # canonical workspace root

[[repos]]
name   = "hello"             # required, unique
url    = "https://github.com/octocat/Hello-World.git"  # required
path   = "hello"             # relative to workspace.root; default = name
branch = "master"            # default: remote HEAD default branch
sparse = false               # if true: clone with --filter=blob:none --sparse

[secrets]
file = "secrets.enc.json"    # relative to manifest dir; age-encrypted JSON
# recipients live inside the encrypted file's header handling — see secrets pkg

[toolchain]
mise = true                  # run `mise install` in each repo if mise binary found

[[sync]]
name     = "test-folder"     # required, unique
local    = "~/caravan-test-sync"
remote   = "m@host.example.ts.net:~/caravan-test-sync"   # user@host:path
exclude  = [".git", "node_modules", ".DS_Store", "dist", "target"]
```

Manifest structs live in `internal/manifest` with `Load(path)`, `Save(path, *Manifest)`,
`Default()` and validation (unique names, required fields).

## Commands (each implemented as `func Cmd<X>(args []string) int` in its package)

main.go dispatch (already written — do not modify):
- `caravan init [--root DIR] [-f manifest]` → manifest.CmdInit
- `caravan up [--dry-run] [--only repo1,repo2] [-f manifest]` → provision.CmdUp
- `caravan status [-f manifest]` → provision.CmdStatus
- `caravan secrets <init|set|show|add-machine> ...` → secrets.CmdSecrets
- `caravan sync [name] [--watch] [--interval 2s] [--dry-run] [--bootstrap] [-f manifest]` → syncengine.CmdSync
- `caravan scan --json <dir> [--exclude a,b,c]` → syncengine.CmdScan  (hidden; used over ssh)
- `caravan version` → handled in main.go

### AGENT A scope: internal/manifest, internal/provision, internal/secrets

**manifest.CmdInit**: walk `--root` (default `~/code`, don't recurse into a repo once
found, skip hidden dirs and node_modules), detect dirs containing `.git`, read
`git remote get-url origin` and current branch, write manifest draft to the resolved
manifest path (mkdir -p parent). If manifest exists: print it would overwrite, require
`--force`. Print discovered table.

**provision.CmdUp**: for each repo (or `--only` subset):
1. Target dir = root + path. If missing → clone. `sparse=true` → `git clone
   --filter=blob:none --sparse <url> <dir>` then `git sparse-checkout init --cone`;
   else plain clone. If branch set, `--branch <b>`.
2. If present and is a git repo → `git pull --ff-only` on current branch; if that
   fails (diverged/dirty) → report `✗ needs attention (reason)`, continue.
3. If present and NOT a git repo → `✗ path occupied`, continue.
4. Secrets: if secrets file exists and decryptable, write `<repoDir>/.env` from the
   decrypted map for that repo name (only if entries exist). Never overwrite an
   existing .env with fewer keys — merge: manifest-managed keys win, unknown
   pre-existing lines preserved.
5. Toolchain: if `[toolchain].mise` and `mise` binary in PATH and repo has
   `.mise.toml` or `mise.toml` → run `mise install` in the repo dir.
6. direnv: if `direnv` binary in PATH, write `.envrc` containing `dotenv` if a .env
   was written and no .envrc exists.
7. Print status table: name | action (cloned/updated/up-to-date/skipped/error) |
   branch | detail. Wall-clock total at the end.

**provision.CmdStatus**: table per repo: exists? branch, ahead/behind counts
(`git rev-list --left-right --count @{u}...HEAD`, tolerate no upstream), dirty
(`git status --porcelain` non-empty), .env present?, mise config present?
Plus per-sync-folder: last sync time read from state file (see sync pkg StateInfo
helper), or "never".

**secrets package**: age-encrypted JSON sidecar. File layout (plaintext form):
```json
{"recipients": ["age1..."], "repos": {"hello": {"API_KEY": "x"}}}
```
Stored on disk age-encrypted (binary age format is fine) to ALL recipients listed
inside. Machine identity: `~/.config/caravan/age.key` (X25519, 0600, created by
`secrets init` or lazily by any secrets write op; print pubkey after generation).
- `secrets init` — generate key if absent, create empty encrypted store with this
  machine as sole recipient, print pubkey.
- `secrets set <repo> <KEY> <VALUE>` — decrypt, set, re-encrypt.
- `secrets show [repo]` — decrypt, print (values masked unless `--reveal`).
- `secrets add-machine <age-pubkey>` — append recipient, re-encrypt. Print
  instructions for the other machine (copy age.key? no — the OTHER machine runs
  `secrets init`, prints its pubkey, user runs add-machine here, then syncs the
  encrypted file).
Decrypt errors (no key, not a recipient) → actionable message naming the fix.

Unit tests (Agent A): manifest load/save/validate round-trip; ExpandPath; init
discovery against a tmpdir with fake git repos (create real tiny git repos with
`git init` in t.TempDir()); up --dry-run output against fake repos; secrets
round-trip (init→set→show→add-machine→decrypt with second key). Use t.TempDir()
and set HOME/XDG overrides via env vars — read key path through a var that tests
can point elsewhere (`secrets.KeyPath` package var, default computed).

### AGENT B scope: internal/syncengine

Snapshot-based bidirectional folder sync between a local dir and a remote dir over
ssh. No rsync dependency — transfers use tar over ssh; remote scanning uses the
caravan binary on the remote (`caravan scan --json`), bootstrapped automatically.

**Scan** (`CmdScan` + `ScanDir(root string, excludes []string) (map[string]Entry, error)`):
walk root; Entry = `{Size int64, Mtime int64 /*unixnano*/, Mode uint32, IsDir bool}`;
paths are slash-separated relative; apply excludes (match against base name AND any
path segment; simple string match + `*.ext` glob via path.Match on base). Skip
symlinks (report count). `--json` prints `{"entries": {...}}` to stdout.

**Base state**: `~/.config/caravan/sync-state/<syncName>.json`:
`{"pairs": {path: {"hash":"", "lsize":N,"lmtime":N,"rsize":N,"rmtime":N,"dir":bool}},
"lastSync": unixnano}`. (hash unused v0.1, keep field for forward compat.)

**Remote ops** (`internal/syncengine/remote.go`): parse `user@host:path` remote spec.
- `RemoteScan`: `ssh user@host '~/.local/bin/caravan' scan --json <path> --exclude ...`
  (quote path; path may contain `~` — pass through sh expansion by NOT quoting the
  leading ~ segment, or use $HOME replacement: replace leading `~` with literal
  `$HOME` and wrap in double quotes).
- If remote caravan is missing (exit 127 / "not found" on stderr): if `--bootstrap`
  or interactive default yes → check `ssh host uname -sm` matches local
  `runtime.GOOS/GOARCH` equivalent (Darwin arm64), then
  `cat os.Executable() | ssh host 'mkdir -p ~/.local/bin && cat > ~/.local/bin/caravan && chmod +x ~/.local/bin/caravan'`,
  retry scan once.
- Remote mkdir of sync root if missing (part of bootstrap/scan path).

**Planner** (`Plan(base, local, remote) []Action` — pure function, the testing
centerpiece): three-way diff per path (union of keys):
- changed(side) = not in base, or size/mtime differs from base's side record.
- local-only new → push; remote-only new → pull.
- both new / both changed → conflict: newer mtime wins (tie → local wins); log line
  `conflict: <path> (local|remote) wins`.
- in base, missing local, remote unchanged → delete remote. Mirror for other side.
- deleted one side + changed other side → modification wins, recopy.
- dirs: create as needed implicitly via file copies + explicit empty-dir actions;
  dir deletions after file deletions, deepest first.
Action = `{Op: push|pull|deleteLocal|deleteRemote|mkdirLocal|mkdirRemote, Path, Reason}`.

**Apply**:
- push: local tar → remote: write file list to tmpfile, `tar -C localRoot -cf - -T list | ssh host "mkdir -p <root> && tar -C <root> -xpf -"`. Preserve mtimes (bsdtar does by default).
- pull: `ssh host "cd <root> && cat > /tmp/caravan-list && tar -cf - -T /tmp/caravan-list && rm /tmp/caravan-list"` with list piped to stdin, extract locally with `tar -xpf -`.
- deletes: batch `rm` / `ssh rm` (paths safely single-quoted; reject paths containing `'` or newline at scan time with a skip warning).
- After apply: rescan BOTH sides, new base = entries for the union of surviving
  paths using observed values from each side. Save state file atomically
  (tmp+rename).
- `--dry-run`: print the plan table, change nothing.
- Output: table of actions + summary `pushed N, pulled M, deleted K (local/remote), conflicts C, took T`.
- If plan empty: `✓ <name> in sync (N files)`.

**CmdSync**: no args → all `[[sync]]` entries; with name → that one. `--watch`:
loop scan+sync every `--interval` (default 2s, parse with time.ParseDuration);
Ctrl-C clean exit; only print when something changed (plus one `watching <name>
(interval 2s)` line at start).

Unit tests (Agent B): planner table-driven tests covering: fresh both-empty, fresh
push-all, modify local, modify remote, both modify (newer wins, tie), delete local
propagates, delete-vs-modify, new same path both sides, excludes, empty dirs,
nested dirs. Local-vs-local integration test: use a `remote` spec of the literal
form `local:<abs path>` (support this in remote.go: scheme "local:" → run scan/
apply/delete against the local FS directly, no ssh) — full sync round trip in
t.TempDir(): seed A, sync, mutate B, sync, assert trees identical (walk+compare).
The `local:` transport is also useful for users syncing to a mounted volume — 
document it in --help.

## Non-goals (do not build)

FUSE/VFS, conflict-resolution UI, ignore-file parsing (.gitignore), file locking,
partial-file transfer/delta, daemonization beyond --watch, Windows.
