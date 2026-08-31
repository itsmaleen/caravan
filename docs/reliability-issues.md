# Caravan reliability issues & improvement backlog

Running log of reliability problems found operating caravan (primarily the
MacBook → Mac-mini `wrinkles` + `secondironhand` syncs over Tailscale). Written
so a future session can pick up the open items. Newest investigation first.

Status legend: **[FIXED]** shipped · **[OPEN]** not yet addressed · **[IDEA]** improvement worth considering.

---

## 2026-08-22 — AppleDouble `._*` files accumulate on receivers and spread [OPEN]

**Tracked as [#5](https://github.com/itsmaleen/caravan/issues/5).**

**Symptom.** The receiving side of a sync accumulates 163-byte AppleDouble
sidecar files (`._name` next to `name`). The source machine has **zero**. They
are ordinary files as far as caravan is concerned, so `scan` reports them and
they propagate to every other machine in the topology. Measured 2026-08-22 07:53
EDT, immediately after adding the framework laptop as a second spoke:

| tree | MacBook (source) | mini (hub) | framework (new spoke) |
|---|---|---|---|
| `~/wrinkles` | **0** | 2585 | 2585 |
| `~/agent-config` | **0** | 518 | 522 |

The mini's own `caravan scan -json ~/wrinkles` reports 5249 entries, **2585 of
them `._*`** — i.e. roughly a third of the entry list is junk, and a brand-new
machine faithfully pulls all of it on first sync.

**Not historical — actively reproducing.** Oldest sidecar on the mini is dated
2026-04-03, newest 2026-08-22 (same day as this entry), so it has been running
for ~4.5 months. Confirmed live during this session: editing
`~/agent-config/nixos/framework.nix` on the MacBook and letting the daemon push
it produced `._framework.nix` on the mini, which framework then pulled.

**Impact.**
- Every new machine inherits ~3100 junk files on first sync.
- ~1/3 of the scanned entry list for `wrinkles` is noise, on every scan, forever.
- `~/wrinkles/output` is served tailnet-wide, so the sidecars are served too.

**Ruled out** (each tested directly against the real MacBook + mini, not reasoned
about):
1. *macOS `tar -c` emitting AppleDouble members.* It does not — it emits **pax
   extended headers** (`SCHILY.xattr.*` / `LIBARCHIVE.xattr.*`). Verified by
   piping `tar -cf -` of a file carrying `com.apple.provenance` through
   `tar -tvf -`: one member, no `._`. This is also why GNU tar on the Linux
   spoke prints `Ignoring unknown extended header keyword
   'LIBARCHIVE.xattr.com.apple.provenance'` — it discards them harmlessly.
2. *`stagedExtractScript`'s `tar -xpf -` materialising the sidecar.* It does not
   — extracting that same stream into `.caravan-staging` **on the mini** yielded
   only `probe.txt`, with its xattrs restored natively (`ls -l@` shows `@`).
3. *A filesystem without native xattr support on the mini.* `~/wrinkles` is on
   APFS (`/dev/disk3s5`, `apfs, local, journaled`), and an `xattr -w` probe there
   stored the attribute natively and created no sidecar.
4. *caravan filtering `._*` somewhere.* No such filter exists in `scan.go` or
   `DefaultExcludes`, which is consistent with the scan reporting all 2585.

**[CONFIRMED 2026-08-22] It is a permanent pull loop, not cosmetic junk.** The
MacBook's `wrinkles.log` carries the same summary on *every* pass:

```
pushed 13, pulled 2586, deleted 0 local/0 remote, conflicts 0
pushed 13, pulled 2591, deleted 0 local/0 remote, conflicts 0
```

All ~2,586 pulls are `._*` files logged as `←local  new remote`. The cycle:
pull the sidecar → **macOS absorbs it into the parent's xattrs**, so it stops
existing as a file → next scan sees it absent locally, present remotely → "new
on remote" → pull again. Forever. Three facts confirm the absorption:

- The MacBook's base state for `wrinkles` holds **2,665 pairs**; the mini's scan
  reports **5,249** entries of which **2,585** are `._*`. `5249 - 2585 = 2664` —
  the base holds **exactly the non-sidecar files**, because a sidecar never
  survives long enough to be recorded in it.
- The MacBook has **zero** `._*` files on disk moments after pulling 2,586.
- Deletion therefore can never propagate outward: a sidecar is always classified
  "new on remote", never "deleted on local".

**Cost.** `~/Library/Logs/caravan/wrinkles.log` is **1,463,562,426 bytes (1.4 GB)**
created 2026-07-02, with **10.85M** `._` mentions — the loop wrote it.
`secondironhand.log` is 33 MB. On the fresh Linux spoke,
`~/.local/state/caravan/logs/wrinkles.log` hit 13 MB within hours. Every pass
also moves ~2,586 files across the tailnet for nothing. Related gap: **caravan
has no log rotation**.

**Origin: still unknown.** The asymmetry is now explained (absorption, above). What is
still unexplained is what *writes* a `._name` onto the mini in the first place —
caravan's own transfer path was cleared by direct test. One plausible complete
story: a one-time event around 2026-04-03 (oldest sidecar) minted them and the
loop has kept them alive since, because nothing can ever delete them. Against
that, the newest sidecar is dated 2026-08-22. Worth resolving before choosing a
remedy: if the origin is one-off, a bulk clean plus an exclude ends it for good.

**A fix was attempted and reverted.** `COPYFILE_DISABLE=1` was added to both
`tar -c` call sites (local push at `remote.go`, remote pull script) on hypothesis
(1). It was reverted once (1) was disproved: the flag suppresses AppleDouble
*members*, which macOS tar was never producing here, so it fixed nothing while
carrying a comment asserting a mechanism that had been tested false. Do not
re-apply it without first reproducing the sidecar creation.

**Mitigation not yet applied.** Adding `._*` to the `exclude` list of the
affected `[[sync]]` entries stops both the spread and the pull loop immediately,
but leaves the ~3100 existing files in place (and, being excluded, caravan would
no longer delete them either). A bulk clean is what actually ends it. Either way
the 1.4 GB log wants truncating.

**Update 2026-08-30.** The log reached **9,750,630,729 bytes (9.75 GB)** and was
the largest single reclaimable item in a full-disk cleanup that began with 2.1 GB
free on a 460 GB volume. Truncate **in place** (`: > wrinkles.log`) — the daemon
holds the fd, so `rm` leaves it writing to an unlinked inode.

Growth is **bursty and tracks sync-pass frequency, not wall-clock**, so a GB/day
figure misleads: after truncation the log reached 10,360 lines and then sat at
**0 bytes of growth over a 120 s idle sample**. The stable metric is the ratio —
**10,344 of 10,360 lines contain `._`, i.e. 99.8 % of the log is sidecar churn**.

Direction reconfirmed: `fd -Hu '^\._' ~/wrinkles` on the MacBook returns **0**,
so the laptop is a clean source *pulling* sidecars down each cycle, never a
producer. Related gap restated: **caravan has no log rotation**, which is what
turns this from noise into a disk-space incident.

---

## 2026-08-13 — the watch loop hangs for hours after the Mac sleeps

**Symptom.** `caravan sync --watch` silently stops syncing for hours. The process
stays alive, its log freezes at the last line, and no local changes propagate.
Observed frozen **13:51 → 20:36** (~7h) across a laptop sleep; a whole work
session of edits never reached the mini and had to be pushed by hand with rsync.
Both the `wrinkles` and `secondironhand` daemons were independently hung.

**Root cause (a chain, each link necessary).**
1. `sshBaseArgs()` had **no `ServerAliveInterval`** → a half-open TCP connection
   (the normal aftermath of sleep / tailscale re-key) leaves an ssh `read()`
   blocking forever.
2. `waitScanSSH` runs the remote long-poll (`caravan scan --wait 20s`) with a
   plain `cmd.Run()` and **no timeout** → the watch loop blocks on it.
3. Every op (scan, rsync push, delete) multiplexes over **one shared
   ControlMaster mux** → a single half-open master freezes *everything*, and
   `ControlPersist=60s` never reaps it because a hung client keeps it alive.
4. launchd `KeepAlive` **cannot catch a hang** — it restarts a process that
   *exits*; a hung process is still alive.
5. `launchctl kickstart -k` alone **reuses the stale master socket** (fixed
   `ControlPath`), so a restarted daemon re-hangs within one cycle. Confirmed: a
   20:36 restart re-hung immediately on the 10:42 master.

**[FIXED] in 0.6.1.**
- `sshBaseArgs()` (internal/syncengine/remote.go) now adds
  `ServerAliveInterval=15`, `ServerAliveCountMax=3`, `ConnectTimeout=10`. A dead
  peer is detected in ~45s and ssh exits with an error the daemon's existing
  backoff/retry handles; because the option applies to the master connection, a
  dead master self-recycles. This one change covers scan, push (rsync via
  `sshCarrier`), and deletes.
- `waitScanSSH` now wraps the long-poll in `context.WithTimeout(window + 60s)`
  via a new `sshCommandContext` — a hard backstop so a hung read can never freeze
  the loop indefinitely even if keepalive misses.
- `internal/doctor/doctor.go` `sshDoctorArgs()` kept in sync (it duplicates the
  list).

**Client-side mitigations added outside this repo** (documented in the wrinkles
vault, article `caravan_sync_reliability`): the same keepalive in `~/.ssh/config`
for the mini host, and a launchd **watchdog** (`~/.local/bin/caravan-watchdog.py`,
`dev.caravan.watchdog`, every 120s) that force-recovers any hang keepalive misses
by detecting a `caravan scan` ssh alive > 90s (healthy long-polls return in ≤20s;
an offline mini fails fast on ConnectTimeout, so it never churn-restarts a real
outage), recycling the mux, and kickstarting the affected agent(s).

---

## 2026-08-13 — bootstrap-pushed binary is SIGKILLed on first exec (macOS)

**Symptom.** After bumping the version (0.6.0 → 0.6.1) the daemon's version
handshake bootstrapped the new binary to the mini, but the **first** `caravan`
exec on the mini returned **exit 137 (SIGKILL)** and remote scans failed with
`exit status 255` for a few minutes. A moment later the same binary ran fine
(`codesign valid on disk`, version 0.6.1, exit 0).

**Root cause (likely).** macOS AMFI/Gatekeeper evaluates a freshly-copied
ad-hoc/linker-signed Mach-O on its **first** execution and can SIGKILL that first
run while it evaluates, then caches the result so subsequent runs succeed. During
that window the remote `caravan scan` dies → the initiating side sees ssh exit
255 → the sync appears broken. It self-heals once the binary has run once, but
that is a confusing several-minute outage triggered *by an upgrade*.

**[OPEN] / [IDEA].** After `bootstrap()` pushes the binary, **warm it up**: run
`~/.local/bin/caravan version` over the same ssh once (ignore the result) so the
AMFI evaluation happens deliberately, before the daemon relies on it. Optionally
`codesign -v` it and log a clear message if the remote refuses to run it. Cheap,
removes the upgrade-time outage. (Also consider: the version handshake re-pushes
on *any* mismatch including a downgrade — fine, but the warm-up matters there too.)

---

## Standing issues & improvement ideas (found along the way)

- **[FIXED, #2] Single ControlMaster mux as a SPOF.** One dead master hangs every
  op sharing it. Keepalive recycles it, and the `ControlPath` is now scoped by
  process *and* by entry (below), so neither unrelated processes nor sibling
  entries in one process share a master. Remaining idea: a periodic master
  health-ping would catch a wedge faster than the long-poll timeout.
- **[IDEA] The daemon emits no idle heartbeat.** When there are no changes it
  logs nothing, so log-staleness is useless for external liveness monitoring
  (the client watchdog had to detect a stuck *ssh* instead). A cheap periodic
  `watching …` heartbeat line (say every N cycles) would make "is it alive?"
  trivial to check and alert on.
- **[IDEA] Self-heal without an external watchdog.** launchd `KeepAlive` can't
  see a hang. If the watch loop wrapped each remote op in a context timeout
  (this change does it for the long-poll; extend to push/scan) and *exited* on
  repeated consecutive failures, `KeepAlive` would restart it — but the restart
  must **recycle the master first** (see below), or it re-hangs.
- **[FIXED, 0.6.1] Reap a stale master at startup.**
  `RemoteConn.closeMaster()` (`ssh -O exit`) runs at the start of each watch entry,
  so a restart can't inherit a half-open master and re-hang — something
  `exec.CommandContext` alone could not do (it kills the passenger ssh, not the
  persistent master). It is deliberately NOT run on a long-poll timeout: a
  superseded WaitScan shares its entry's master with the sync that replaced it, so
  tearing it down mid-transfer would abort that sync; keepalive (~45s) handles a
  genuinely half-open master instead.
- **[FIXED, #2] Private, per-process, per-entry `ControlPath`.** The socket lives
  at `/tmp/caravan-<uid>/s-<pid>-<sha256(entry-name)[:12]>-%r@%h-%p`, in a `0700`
  dir the user owns. `EnsureSecureSockDir` is **fail-closed** — called before any
  ssh op in both one-shot and watch sync (and in `doctor`), it refuses to proceed
  if the dir isn't a private directory we own at 0700. This (a) closes the
  world-writable-`/tmp` mux-hijack (the socket speaks an unauthenticated protocol,
  so another local user who pre-binds a predictable path could MITM it — MkdirAll
  accepts a pre-created dir and Chmod can't tighten one we don't own, so a warning
  wasn't enough); (b) isolates unrelated processes via the pid; and (c) isolates
  entries via a **SHA-256 digest of the entry's unique name** — keyed by name, not
  host+root, because the manifest allows two entries to share a remote, and a
  32-bit hash collides (both defects were caught by cross-review). One entry's
  `closeMaster` therefore can't abort another's transfer. Also dropped the
  on-timeout `closeMaster`: a superseded WaitScan shares its entry's master with
  the in-flight sync, so tearing it down mid-transfer was wrong; keepalive (~45s)
  and the startup reap cover the wedged/stale cases. Kept under `/tmp` (not macOS's
  long `$TMPDIR`) to stay within the ~104-char unix-socket path limit.
- **[IDEA] Conflict backups accumulate silently.** `~/.config/caravan/conflicts/`
  had 11 stale backups (a rapidly-rewritten `signal/.state/domains.json` churned
  many; plus one from a hand-rsync racing the daemon). Consider pruning old
  conflict backups, and surfacing a count in `caravan status`.
- **[KNOWN] Bidirectional revert on rapid local appends.** A long series of quick
  local edits can be reverted on the laptop by a conflicting sync tick while the
  mini keeps the authoritative copy (seen 2026-07-18). Treat the mini as source
  of truth for served artifacts; a "local is authoritative for N seconds after a
  write" debounce would help.
- **[NIT] Version is a hand-edited const.** `buildinfo.Version` is edited by hand
  with no `-ldflags`/git-describe stamping, so two different builds can share a
  version (a 0.6.0 with and without a fix coexisted today). Consider stamping
  version+commit at build time so the handshake and `caravan version` are honest.

---

## Verifying a fix (quick recipe)

```bash
# Set these first — every value is a placeholder, replace before running:
HOST='m@mini.example.ts.net'   # ssh target of the sync
NAME='wrinkles'                # launchd agent suffix: dev.caravan.sync.$NAME
LOCAL="$HOME/wrinkles"         # local synced dir
REMOTE='~/wrinkles'            # same dir on the remote (tilde expands there)

# build/install locally, restart the daemon on the new binary (recycle the mux!).
# NOTE: this pkill/rm is host-wide — it resets EVERY caravan master to $HOST, not
# just $NAME's; each reconnects on its next cycle, which is fine for recovery.
make install
pkill -f "ssh: /tmp/caravan-ssh-.*${HOST##*@}"; rm -f /tmp/caravan-ssh-*"${HOST##*@}"*
launchctl kickstart -k "gui/$(id -u)/dev.caravan.sync.$NAME"

# the running binary really passes ALL the keepalive options (fails if any is missing):
cmdline="$(ps aux | grep '[c]aravan scan' || true)"
for opt in ServerAliveInterval=15 ServerAliveCountMax=3 ConnectTimeout=10; do
  grep -q -- "$opt" <<<"$cmdline" || { echo "missing $opt" >&2; exit 1; }
done && echo "keepalive options present"

# end-to-end: a change auto-propagates
echo "t-$(date +%s)" > "$LOCAL/.synctest"; sleep 30
ssh "$HOST" "cat $REMOTE/.synctest"; rm "$LOCAL/.synctest"
```
