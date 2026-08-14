# Caravan reliability issues & improvement backlog

Running log of reliability problems found operating caravan (primarily the
MacBook → Mac-mini `wrinkles` + `secondironhand` syncs over Tailscale). Written
so a future session can pick up the open items. Newest investigation first.

Status legend: **[FIXED]** shipped · **[OPEN]** not yet addressed · **[IDEA]** improvement worth considering.

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

- **[IDEA] Single ControlMaster mux is a single point of failure.** One dead
  master hangs every op. Keepalive now recycles it, but consider a per-sync
  master (distinct `ControlPath` per entry) or a periodic master health-ping so
  one wedged connection can't stall unrelated syncs.
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
- **[IDEA] `kickstart`/startup should reap a stale master.** On daemon start,
  `ssh -O exit` (or remove the `ControlPath` socket) for its hosts before the
  first op, so a restart never inherits a dead master. Would make the manual
  recovery (and the external watchdog) unnecessary.
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
# build/install locally, restart the daemon on the new binary (recycle the mux!)
make install
pkill -f 'ssh: /tmp/caravan-ssh-.*<host>'; rm -f /tmp/caravan-ssh-*<host>*
launchctl kickstart -k gui/$(id -u)/dev.caravan.sync.<name>

# the running binary really passes keepalive:
ps aux | grep '[c]aravan scan' | grep -o 'ServerAliveInterval=[0-9]*'

# end-to-end: a change auto-propagates
echo t-$(date +%s) > ~/<synced>/.synctest; sleep 30
ssh <host> cat '~/<synced>/.synctest'; rm ~/<synced>/.synctest
```
