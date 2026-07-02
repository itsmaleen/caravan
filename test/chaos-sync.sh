#!/bin/bash
# Caravan chaos/adversarial sync test.
# Probes kill-mid-transfer, network drops, corrupt state, lock contention, and scale.
# Run from the caravan repo root:
#   CARAVAN_BIN=$(pwd)/caravan ./test/chaos-sync.sh
set -u

MINI="${CARAVAN_TEST_REMOTE:-user@example-host}"
LOCAL_DIR="$HOME/caravan-chaos-sync"
REMOTE_DIR="caravan-chaos-sync"
BIN="${CARAVAN_BIN:-$(pwd)/caravan}"
MANIFEST="$(mktemp -d)/caravan-chaos.toml"
STATE_DIR="$HOME/.config/caravan/sync-state"
CONFLICT_DIR="$HOME/.config/caravan/conflicts"

PASS=0; FAIL=0
TIMINGS=""

ok()   { PASS=$((PASS+1)); echo "  PASS: $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  FAIL: $1"; }
info() { echo "  INFO: $1"; }

header() {
  echo ""
  echo "======================================================="
  echo "  $1"
  echo "======================================================="
}

# Kill any orphaned caravan/tar/rsync processes from our test dir, then wait for
# the advisory lock to be released. This prevents "sync already running" errors
# from orphaned child processes that inherited the flock fd.
kill_orphans() {
  # Kill any caravan, tar (from caravan-push temp files), rsync touching our test dirs
  pkill -9 -f "caravan sync.*chaos" 2>/dev/null || true
  pkill -9 -f "tar.*caravan-push" 2>/dev/null || true
  pkill -9 -f "rsync.*caravan-chaos-sync" 2>/dev/null || true
  # Give processes a moment to die and release fds
  sleep 0.5
  # Wait up to 5s for the lock to be released
  for i in $(seq 1 10); do
    if python3 -c "
import fcntl, os, sys
p = '$STATE_DIR/chaos.lock'
if not os.path.exists(p):
    sys.exit(0)
fd = os.open(p, os.O_RDWR)
try:
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    fcntl.flock(fd, fcntl.LOCK_UN)
    sys.exit(0)
except BlockingIOError:
    sys.exit(1)
finally:
    os.close(fd)
" 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  info "WARNING: lock still held after kill_orphans; proceeding anyway"
}

# ---- cleanup helpers ----
clean_state() {
  rm -f "$STATE_DIR/chaos.json"
  rm -f "$STATE_DIR/chaos.lock"
  rm -rf "$CONFLICT_DIR/chaos"
}

clean_local() {
  rm -rf "$LOCAL_DIR"
  mkdir -p "$LOCAL_DIR"
}

clean_remote() {
  ssh -o BatchMode=yes "$MINI" "rm -rf \"\$HOME/$REMOTE_DIR\"" 2>/dev/null || true
}

full_clean() {
  kill_orphans
  clean_state
  clean_local
  clean_remote
}

# ---- manifest ----
write_manifest() {
  cat > "$MANIFEST" <<EOF
version = 1
[workspace]
root = "~/code"
[[sync]]
name   = "chaos"
local  = "~/caravan-chaos-sync"
remote = "$MINI:~/caravan-chaos-sync"
EOF
}

# ---- tree helpers ----
local_tree()  { (cd "$LOCAL_DIR" && find . -type f ! -name .DS_Store -print0 | sort -z | xargs -0 shasum -a 256 2>/dev/null | awk '{print $2, $1}'); }
remote_tree() { ssh -o BatchMode=yes "$MINI" "cd \"\$HOME/$REMOTE_DIR\" && find . -type f ! -name .DS_Store -print0 2>/dev/null | sort -z | xargs -0 shasum -a 256 2>/dev/null | awk '{print \$2, \$1}'"; }

trees_match() {
  local l r
  l="$(local_tree 2>/dev/null)" || { info "local_tree failed"; return 1; }
  r="$(remote_tree 2>/dev/null)" || { info "remote_tree failed"; return 1; }
  if [ "$l" = "$r" ]; then return 0; fi
  echo "    --- local tree (first 20) ---"
  echo "$l" | head -20
  echo "    --- remote tree (first 20) ---"
  echo "$r" | head -20
  return 1
}

sync_once() {
  "$BIN" sync -f "$MANIFEST" chaos
}

sha256_file() {
  shasum -a 256 "$1" 2>/dev/null | awk '{print $1}'
}

remote_sha256() {
  ssh -o BatchMode=yes "$MINI" "shasum -a 256 \"\$HOME/$REMOTE_DIR/$1\" 2>/dev/null | awk '{print \$1}'" 2>/dev/null
}

# ---- seed base sync ----
seed_base() {
  full_clean
  write_manifest
  echo "seed1" > "$LOCAL_DIR/seed1.txt"
  echo "seed2" > "$LOCAL_DIR/seed2.txt"
  mkdir -p "$LOCAL_DIR/subdir"
  echo "nested" > "$LOCAL_DIR/subdir/n.txt"
  sync_once > /dev/null 2>&1
}

# ===================================================================
# SSH reachability check
# ===================================================================
header "SSH REACHABILITY CHECK"
if ! ssh -o BatchMode=yes "$MINI" "echo ok" >/dev/null 2>&1; then
  echo "FATAL: ssh to $MINI unreachable — aborting chaos tests"
  exit 1
fi
info "SSH to $MINI: OK"

# ===================================================================
# SCENARIO 1: SIGKILL mid-push of a large file
# ===================================================================
header "SCENARIO 1: SIGKILL mid-push of a large file"
S1_START=$(date +%s)

seed_base

# Create a 200MB file locally (use full absolute path, not ~/)
dd if=/dev/urandom of="$HOME/caravan-chaos-sync/bigfile.bin" bs=1048576 count=200 2>/dev/null
SOURCE_CKSUM=$(sha256_file "$LOCAL_DIR/bigfile.bin")
LMTIME=$(stat -f%m "$LOCAL_DIR/bigfile.bin")
info "Source checksum: $SOURCE_CKSUM"
info "Local mtime: $LMTIME"
info "Starting push, will kill mid-transfer..."

# Start caravan in background and track its PGID for full group kill
sync_once > /tmp/chaos-s1-sync.log 2>&1 &
CARAVAN_PID=$!

# Poll until remote file appears and is partially transferred
KILL_DONE=0
KILL_SIZE=0
for i in $(seq 1 60); do
  sleep 0.5
  RSIZE=$(ssh -o BatchMode=yes "$MINI" "stat -f%z \"\$HOME/$REMOTE_DIR/bigfile.bin\" 2>/dev/null || echo 0" 2>/dev/null)
  RSIZE=${RSIZE:-0}
  # Kill when between 5MB and 150MB transferred (confirming mid-transfer)
  if [ "$RSIZE" -gt 5242880 ] 2>/dev/null && [ "$RSIZE" -lt 157286400 ] 2>/dev/null; then
    KILL_SIZE=$RSIZE
    info "Remote partial size at kill: ${RSIZE} bytes — killing now (mid-transfer confirmed)"
    KILL_DONE=1
    break
  fi
  if ! kill -0 $CARAVAN_PID 2>/dev/null; then
    info "Caravan finished before mid-transfer kill could land"
    break
  fi
done

# Kill caravan AND any children (tar, rsync, ssh) that might hold the flock fd
kill -9 $CARAVAN_PID 2>/dev/null || true
# Kill tar processes created by this caravan invocation (they inherit flock fd)
pkill -9 -f "tar.*caravan-push" 2>/dev/null || true
pkill -9 -f "rsync.*bigfile\.bin" 2>/dev/null || true
wait $CARAVAN_PID 2>/dev/null || true
# Wait for lock to be released
kill_orphans

# (a) Inspect remote partial state
RSIZE_AFTER=$(ssh -o BatchMode=yes "$MINI" "stat -f%z \"\$HOME/$REMOTE_DIR/bigfile.bin\" 2>/dev/null || echo MISSING" 2>/dev/null)
RMTIME_AFTER=$(ssh -o BatchMode=yes "$MINI" "stat -f%m \"\$HOME/$REMOTE_DIR/bigfile.bin\" 2>/dev/null || echo MISSING" 2>/dev/null)
RCKSUM_PARTIAL=$(ssh -o BatchMode=yes "$MINI" "shasum -a 256 \"\$HOME/$REMOTE_DIR/bigfile.bin\" 2>/dev/null | awk '{print \$1}'" 2>/dev/null)
info "Remote bigfile.bin size after kill: $RSIZE_AFTER (full would be 209715200)"
info "Remote bigfile.bin mtime: ${RMTIME_AFTER} | local mtime: $LMTIME"
info "Remote bigfile.bin cksum: ${RCKSUM_PARTIAL:-MISSING}"
info "Caravan log: $(cat /tmp/chaos-s1-sync.log 2>/dev/null | head -6)"

if [ "$KILL_DONE" -eq 1 ]; then
  info "Kill landed mid-transfer at ${KILL_SIZE} bytes"
else
  info "Kill landed after transfer completed (transfer was too fast to interrupt mid-flight)"
fi

# (b) Re-run sync and check convergence
info "Re-running sync for convergence..."
SYNC2_OUT=$(sync_once 2>&1)
SYNC2_EXIT=$?
info "Sync exit code: $SYNC2_EXIT"
info "Sync output: $(echo "$SYNC2_OUT" | head -6)"

# (c) Check convergence checksums
LCKSUM_AFTER=$(sha256_file "$LOCAL_DIR/bigfile.bin")
RCKSUM_AFTER=$(remote_sha256 "bigfile.bin")
info "Local  checksum after re-sync: $LCKSUM_AFTER"
info "Remote checksum after re-sync: $RCKSUM_AFTER"
info "Source checksum (reference):   $SOURCE_CKSUM"

S1_CONVERGED=1
if [ "$LCKSUM_AFTER" = "$SOURCE_CKSUM" ]; then
  ok "S1: Local bigfile matches source after re-sync"
else
  bad "S1: LOCAL bigfile DOES NOT match source after re-sync (got $LCKSUM_AFTER, want $SOURCE_CKSUM)"
  S1_CONVERGED=0
fi

if [ "$RCKSUM_AFTER" = "$SOURCE_CKSUM" ]; then
  ok "S1: Remote bigfile matches source after re-sync"
else
  bad "S1: REMOTE bigfile DOES NOT match source after re-sync (got $RCKSUM_AFTER, want $SOURCE_CKSUM)"
  S1_CONVERGED=0
  echo "  EVIDENCE: remote_size_at_kill=${RSIZE_AFTER} remote_cksum_partial=${RCKSUM_PARTIAL}"
  echo "  re-sync output: $SYNC2_OUT"
  if echo "$SYNC2_OUT" | grep -q "in sync"; then
    echo "  CRITICAL: re-sync reported 'in sync' but checksums DIVERGE — caravan thinks"
    echo "  both sides agree but remote has wrong/partial bytes."
    echo ""
    echo "  ROOT CAUSE (sync.go:341-348): After applyActions completes (even if rsync"
    echo "  was killed mid-transfer and the tar fallback then ALSO wrote partial bytes),"
    echo "  caravan ALWAYS rescans both sides and saves the result as the new base."
    echo "  The rescan sees the partial remote file and records its (wrong) size/mtime"
    echo "  as the authoritative base. Next sync sees local=base and remote=base → 'in sync'."
    echo "  File: internal/syncengine/sync.go:341 (rescan always runs after apply)"
    echo "  File: internal/syncengine/sync.go:778 (buildBase records partial remote entry)"
    echo "  File: internal/syncengine/remote.go:524-530 (tar fallback after rsync kill)"
    echo "  SEVERITY: DATA-LOSS GRADE — remote permanently diverged, no self-repair."
  fi
  if echo "$SYNC2_OUT" | grep -q "conflict"; then
    echo "  Conflict resolution ran on re-sync."
    if [ -n "${RMTIME_AFTER:-}" ] && [ "${RMTIME_AFTER}" != "MISSING" ]; then
      if [ "${RMTIME_AFTER}" -ge "$LMTIME" ] 2>/dev/null; then
        echo "  MTIME: Remote partial mtime ($RMTIME_AFTER) >= local mtime ($LMTIME)."
        echo "  ROOT CAUSE (plan.go:173): No base entry after kill → both-new conflict."
        echo "  Remote partial file has newer mtime (written during transfer) → remote WINS."
        echo "  Partial/corrupt bytes on remote are chosen as the winner."
        echo "  File: internal/syncengine/plan.go:173"
        echo "  SEVERITY: DATA-LOSS GRADE — corrupt remote content wins, pushed back to local."
      fi
    fi
  fi
fi

# Check conflict dir
S1_CONFLICTS=$(ls "$CONFLICT_DIR/chaos/" 2>/dev/null | grep -i bigfile || echo "none")
info "Conflict backups for bigfile: $S1_CONFLICTS"

S1_END=$(date +%s)
TIMINGS="${TIMINGS}Scenario 1: $((S1_END - S1_START))s\n"

# ===================================================================
# SCENARIO 2: SIGKILL mid-pull (200MB created on remote)
# ===================================================================
header "SCENARIO 2: SIGKILL mid-pull (large file from remote)"
S2_START=$(date +%s)

seed_base

# Create 200MB file on remote — escape $HOME so it expands on the REMOTE shell
REMOTE_BIG="bigfile_remote.bin"
ssh -o BatchMode=yes "$MINI" "dd if=/dev/urandom of=\"\$HOME/$REMOTE_DIR/$REMOTE_BIG\" bs=1048576 count=200 2>/dev/null && echo dd_ok" 2>/dev/null | grep -q "dd_ok"
DD_OK=$?
if [ "$DD_OK" -ne 0 ]; then
  bad "S2: Remote dd failed (check $MINI:\$HOME/$REMOTE_DIR exists) — skipping"
  S2_END=$(date +%s)
  TIMINGS="${TIMINGS}Scenario 2: SKIPPED\n"
else

RCKSUM_SOURCE=$(ssh -o BatchMode=yes "$MINI" "shasum -a 256 \"\$HOME/$REMOTE_DIR/$REMOTE_BIG\" 2>/dev/null | awk '{print \$1}'" 2>/dev/null)
RMTIME_SOURCE=$(ssh -o BatchMode=yes "$MINI" "stat -f%m \"\$HOME/$REMOTE_DIR/$REMOTE_BIG\" 2>/dev/null || echo MISSING" 2>/dev/null)
info "Remote source checksum: $RCKSUM_SOURCE"
info "Remote source mtime: $RMTIME_SOURCE"

sleep 1
info "Starting pull, will kill mid-transfer..."
sync_once > /tmp/chaos-s2-sync.log 2>&1 &
CARAVAN_PID2=$!

KILL_DONE2=0
KILL_SIZE2=0
for i in $(seq 1 60); do
  sleep 0.5
  LSIZE=$(stat -f%z "$LOCAL_DIR/$REMOTE_BIG" 2>/dev/null || echo 0)
  LSIZE=${LSIZE:-0}
  if [ "$LSIZE" -gt 5242880 ] 2>/dev/null && [ "$LSIZE" -lt 157286400 ] 2>/dev/null; then
    KILL_SIZE2=$LSIZE
    info "Local partial size at kill: ${LSIZE} bytes — killing now"
    KILL_DONE2=1
    break
  fi
  if ! kill -0 $CARAVAN_PID2 2>/dev/null; then
    info "Caravan finished before mid-pull kill could land"
    break
  fi
done

kill -9 $CARAVAN_PID2 2>/dev/null || true
pkill -9 -f "tar.*caravan-push" 2>/dev/null || true
pkill -9 -f "rsync.*$REMOTE_BIG" 2>/dev/null || true
wait $CARAVAN_PID2 2>/dev/null || true
kill_orphans

LSIZE_AFTER=$(stat -f%z "$LOCAL_DIR/$REMOTE_BIG" 2>/dev/null || echo "MISSING")
LMTIME_AFTER2=$(stat -f%m "$LOCAL_DIR/$REMOTE_BIG" 2>/dev/null || echo "MISSING")
LCKSUM_PARTIAL=$(sha256_file "$LOCAL_DIR/$REMOTE_BIG" 2>/dev/null || echo "MISSING")
info "Local partial size: $LSIZE_AFTER (full would be 209715200)"
info "Local partial mtime: $LMTIME_AFTER2  |  Remote source mtime: $RMTIME_SOURCE"
info "Local partial checksum: $LCKSUM_PARTIAL"
info "Caravan log: $(cat /tmp/chaos-s2-sync.log 2>/dev/null | head -6)"

if [ "$KILL_DONE2" -eq 1 ]; then
  info "Kill landed mid-pull at ${KILL_SIZE2} bytes"
else
  info "Kill landed after pull completed or transfer was too fast"
fi

info "Re-running sync for convergence..."
SYNC2B_OUT=$(sync_once 2>&1)
SYNC2B_EXIT=$?
info "Sync exit code: $SYNC2B_EXIT"
info "Sync output: $(echo "$SYNC2B_OUT" | head -6)"

LCKSUM_S2=$(sha256_file "$LOCAL_DIR/$REMOTE_BIG" 2>/dev/null || echo "MISSING")
RCKSUM_S2=$(remote_sha256 "$REMOTE_BIG")
info "Local  checksum after re-sync: $LCKSUM_S2"
info "Remote checksum after re-sync: $RCKSUM_S2"
info "Remote source (reference):     $RCKSUM_SOURCE"

if [ "$LCKSUM_S2" = "$RCKSUM_SOURCE" ]; then
  ok "S2: Local bigfile_remote.bin matches remote source after re-sync"
else
  bad "S2: LOCAL bigfile_remote.bin DOES NOT match remote source (got $LCKSUM_S2, want $RCKSUM_SOURCE)"
  echo "  EVIDENCE: local_partial=$LCKSUM_PARTIAL local_partial_mtime=$LMTIME_AFTER2"
  echo "  re-sync output: $SYNC2B_OUT"
  if echo "$SYNC2B_OUT" | grep -q "in sync"; then
    echo "  CRITICAL: re-sync said 'in sync' but local diverges from remote source."
    echo "  Same root cause as S1: sync.go:341 rescan records partial local as base."
    echo "  SEVERITY: DATA-LOSS GRADE."
  fi
  if [ -n "$LMTIME_AFTER2" ] && [ "$LMTIME_AFTER2" != "MISSING" ] && [ -n "$RMTIME_SOURCE" ] && [ "$RMTIME_SOURCE" != "MISSING" ]; then
    if [ "$LMTIME_AFTER2" -gt "$RMTIME_SOURCE" ] 2>/dev/null; then
      echo "  MTIME: Local partial mtime ($LMTIME_AFTER2) > remote source mtime ($RMTIME_SOURCE)"
      echo "  ROOT CAUSE: plan.go:173 — partial local tar write has newer mtime than remote."
      echo "  On re-sync with no base: both-new conflict, local wins → pushes corrupt data back."
      echo "  File: internal/syncengine/plan.go:173"
      echo "  SEVERITY: DATA-LOSS GRADE."
    fi
  fi
fi

if [ "$RCKSUM_S2" = "$RCKSUM_SOURCE" ]; then
  ok "S2: Remote bigfile_remote.bin unchanged after re-sync"
else
  bad "S2: Remote bigfile_remote.bin changed (got $RCKSUM_S2, want $RCKSUM_SOURCE)"
fi

fi  # end DD_OK check

S2_END=$(date +%s)
TIMINGS="${TIMINGS}Scenario 2: $((S2_END - S2_START))s\n"

# ===================================================================
# SCENARIO 3: SSH ControlMaster drop mid-sync
# ===================================================================
header "SCENARIO 3: SSH ControlMaster drop mid-sync"
S3_START=$(date +%s)

seed_base

# Create a 100MB file to push so transfer takes several seconds
dd if=/dev/urandom of="$HOME/caravan-chaos-sync/bigfile_cm.bin" bs=1048576 count=100 2>/dev/null
CM_CKSUM=$(sha256_file "$LOCAL_DIR/bigfile_cm.bin")
info "Source checksum: $CM_CKSUM"
info "Starting sync in background, will drop SSH mux master ~2s in..."

sync_once > /tmp/chaos-s3-sync.log 2>&1 &
CARAVAN_PID3=$!
sleep 2.0

# Kill the SSH ControlMaster for caravan's mux socket
CM_SOCKET=$(ls /tmp/caravan-ssh-*ms-mac-mini* 2>/dev/null | head -1 || echo "")
if [ -n "$CM_SOCKET" ]; then
  info "Dropping mux master via: ssh -O exit ControlPath=$CM_SOCKET"
  ssh -o BatchMode=yes -o "ControlPath=$CM_SOCKET" -O exit "$MINI" 2>/dev/null || true
else
  info "No mux socket found; killing ssh ControlMaster processes directly"
  pkill -9 -f "ssh.*ControlMaster.*ms-mac-mini" 2>/dev/null || true
fi

# Wait for caravan to finish
wait $CARAVAN_PID3 2>/dev/null
CARAVAN_EXIT3=$?
kill_orphans

CARAVAN_OUTPUT3=$(cat /tmp/chaos-s3-sync.log 2>/dev/null)
info "Caravan exit code after network drop: $CARAVAN_EXIT3"
info "Caravan output: $(echo "$CARAVAN_OUTPUT3" | head -8)"

# Evaluate result
if [ "$CARAVAN_EXIT3" -ne 0 ]; then
  ok "S3: Caravan returned non-zero exit on network disruption (correctly signals error)"
elif echo "$CARAVAN_OUTPUT3" | grep -qi "error\|fail\|exit status"; then
  ok "S3: Caravan logged errors after network disruption (exit 0 OK if tar fallback succeeded)"
else
  bad "S3: Caravan returned 0 with no error output after SSH mux drop"
  echo "  ROOT CAUSE: rsync failed (SIGPIPE from mux drop) but tar fallback succeeded."
  echo "  sync.go:338 returns nil if applyActions returns nil (fallback succeeded)."
  echo "  This is expected behavior — but if tar ALSO failed (race), the error may be silenced."
  echo "  File: internal/syncengine/sync.go:338, remote.go:524-530"
fi

# Re-run sync and assert convergence
info "Re-running sync after network drop..."
SYNC3B_OUT=$(sync_once 2>&1)
SYNC3B_EXIT=$?
info "Re-sync exit code: $SYNC3B_EXIT"

LCKSUM_S3=$(sha256_file "$LOCAL_DIR/bigfile_cm.bin")
RCKSUM_S3=$(remote_sha256 "bigfile_cm.bin")
info "Local  checksum: $LCKSUM_S3"
info "Remote checksum: $RCKSUM_S3"
info "Source checksum: $CM_CKSUM"

if [ "$LCKSUM_S3" = "$CM_CKSUM" ] && [ "$RCKSUM_S3" = "$CM_CKSUM" ]; then
  ok "S3: Converged to correct content after network drop + re-sync"
else
  bad "S3: Failed to converge after network drop (local=$LCKSUM_S3 remote=$RCKSUM_S3 want=$CM_CKSUM)"
  echo "  ROOT CAUSE: sync.go:341-348 rescan records partial remote file as new base."
  echo "  Next sync sees local=base and remote=base → 'in sync' (diverged permanently)."
  echo "  File: internal/syncengine/sync.go:341"
  echo "  SEVERITY: DATA-LOSS GRADE if divergence is permanent."
fi

S3_END=$(date +%s)
TIMINGS="${TIMINGS}Scenario 3: $((S3_END - S3_START))s\n"

# ===================================================================
# SCENARIO 4: Corrupt state file
# ===================================================================
header "SCENARIO 4: Corrupt state file"
S4_START=$(date +%s)

seed_base

# Capture checksums of a healthy pair
L_CKSUM_PRE=$(sha256_file "$LOCAL_DIR/seed1.txt")
R_CKSUM_PRE=$(remote_sha256 "seed1.txt")
info "Pre-corrupt seed1.txt checksums: local=$L_CKSUM_PRE remote=$R_CKSUM_PRE"

# 4a: Overwrite state with garbage
info "--- 4a: Overwrite state with garbage ---"
printf '{corrupt' > "$STATE_DIR/chaos.json"
SYNC4A_OUT=$(sync_once 2>&1)
SYNC4A_EXIT=$?
info "Sync exit code with corrupt state: $SYNC4A_EXIT"
info "Sync output: $SYNC4A_OUT"

if [ "$SYNC4A_EXIT" -ne 0 ]; then
  ok "S4a: Sync correctly failed with non-zero exit on corrupt state"
  if echo "$SYNC4A_OUT" | grep -qi "parse\|invalid\|corrupt\|json\|state"; then
    ok "S4a: Error message is actionable (mentions parse/state/json issue)"
  else
    bad "S4a: Error message not clearly actionable (got: $SYNC4A_OUT)"
    echo "  The error should guide the user to delete ~/.config/caravan/sync-state/chaos.json"
    echo "  File: internal/syncengine/state.go:83-88"
  fi
else
  bad "S4a: Sync returned 0 with corrupt state — silently ignores corruption"
  echo "  ROOT CAUSE: state.go:83 LoadState returns error on json.Unmarshal failure"
  echo "  but something is suppressing the error before it reaches CmdSync exit code."
  echo "  File: internal/syncengine/state.go:83-88, sync.go:302"
fi

# Verify no data loss from corrupt-state run
L_CKSUM_POST4A=$(sha256_file "$LOCAL_DIR/seed1.txt")
R_CKSUM_POST4A=$(remote_sha256 "seed1.txt")
if [ "$L_CKSUM_POST4A" = "$L_CKSUM_PRE" ] && [ "$R_CKSUM_POST4A" = "$R_CKSUM_PRE" ]; then
  ok "S4a: No data loss from corrupt-state sync attempt"
else
  bad "S4a: DATA LOSS detected after corrupt-state run"
  echo "  pre:  local=$L_CKSUM_PRE remote=$R_CKSUM_PRE"
  echo "  post: local=$L_CKSUM_POST4A remote=$R_CKSUM_POST4A"
fi

# 4b: Delete state file entirely, verify convergence on populated pair
info "--- 4b: Delete state, re-sync on already-synced pair ---"
rm -f "$STATE_DIR/chaos.json"

L_TREE_PRE=$(local_tree 2>/dev/null)
R_TREE_PRE=$(remote_tree 2>/dev/null)

SYNC4B_OUT=$(sync_once 2>&1)
SYNC4B_EXIT=$?
info "Sync exit code with no state: $SYNC4B_EXIT"
info "Sync output: $SYNC4B_OUT"

L_TREE_POST=$(local_tree 2>/dev/null)
R_TREE_POST=$(remote_tree 2>/dev/null)

if [ "$L_TREE_POST" = "$L_TREE_PRE" ] && [ "$R_TREE_POST" = "$R_TREE_PRE" ]; then
  ok "S4b: No data loss with missing state file on already-synced pair"
else
  bad "S4b: DATA LOSS or mutation detected with missing state on populated pair"
  echo "  Pre-local:   $L_TREE_PRE"
  echo "  Post-local:  $L_TREE_POST"
fi

if trees_match; then
  ok "S4b: Trees match after no-state sync"
else
  bad "S4b: Trees do not match after no-state sync"
fi

# Report whether mass re-transfer occurred
if echo "$SYNC4B_OUT" | grep -q "conflict"; then
  info "S4b: Conflict resolution triggered on no-state re-sync (expected)"
  info "     Both sides treated as 'new' since no base → conflict resolved by mtime"
fi

S4_END=$(date +%s)
TIMINGS="${TIMINGS}Scenario 4: $((S4_END - S4_START))s\n"

# ===================================================================
# SCENARIO 5: Lock contention under parallel fire
# ===================================================================
header "SCENARIO 5: Lock contention under parallel fire"
S5_START=$(date +%s)

seed_base

# Touch files while 5 parallel syncs run
(
  for i in $(seq 1 10); do
    echo "mutate-$i" > "$LOCAL_DIR/mutate.txt"
    sleep 0.5
  done
) &
MUTATE_PID=$!

# Launch 5 parallel caravan sync processes
SYNC_PIDS=""
for j in $(seq 1 5); do
  sync_once > "/tmp/chaos-s5-sync-$j.log" 2>&1 &
  SYNC_PIDS="$SYNC_PIDS $!"
done

wait $MUTATE_PID 2>/dev/null || true
for pid in $SYNC_PIDS; do
  wait $pid 2>/dev/null || true
done
sleep 0.5

CRASH_COUNT=0
SKIP_COUNT=0
RAN_COUNT=0
for j in $(seq 1 5); do
  OUT=$(cat "/tmp/chaos-s5-sync-$j.log" 2>/dev/null || echo "")
  if echo "$OUT" | grep -qi "panic\|segfault"; then
    CRASH_COUNT=$((CRASH_COUNT+1))
    bad "S5: Process $j crashed: $OUT"
  fi
  if echo "$OUT" | grep -qi "skipped"; then
    SKIP_COUNT=$((SKIP_COUNT+1))
  fi
  if echo "$OUT" | grep -qi "pushed\|pulled\|in sync"; then
    RAN_COUNT=$((RAN_COUNT+1))
  fi
done

info "S5: $RAN_COUNT ran, $SKIP_COUNT skipped (out of 5 parallel syncs)"

if [ "$CRASH_COUNT" -eq 0 ]; then
  ok "S5: No crashes in 5 parallel sync processes"
else
  bad "S5: $CRASH_COUNT crash(es) in parallel sync"
fi

if [ "$SKIP_COUNT" -gt 0 ]; then
  ok "S5: At least one process correctly skipped due to lock ($SKIP_COUNT skipped)"
else
  bad "S5: No skip messages — lock may not be working or processes ran sequentially"
  echo "  ROOT CAUSE: AcquireSyncLock in sync.go:262 should return ErrSyncBusy"
  echo "  File: internal/syncengine/sync.go:262-270, internal/syncengine/lock.go"
fi

# Verify state JSON is parseable after parallel fire
if [ -f "$STATE_DIR/chaos.json" ]; then
  if python3 -c "import json,sys; json.load(open('$STATE_DIR/chaos.json'))" 2>/dev/null; then
    ok "S5: State JSON parses cleanly after parallel sync"
  else
    bad "S5: State JSON CORRUPT after parallel sync"
    echo "  ROOT CAUSE: SaveState (state.go:97-122) uses tmp+rename (atomic)."
    echo "  JSON corruption means two writes raced — lock may have failed."
  fi
else
  info "S5: No state file written (all 5 skipped; that is OK — settlement sync will write it)"
fi

# Final sync to settle, then check trees
info "Running settlement sync..."
sync_once > /dev/null 2>&1
if trees_match; then
  ok "S5: Trees match after parallel sync + settlement"
else
  bad "S5: Trees do not match after parallel sync + settlement"
  echo "  NOTE: mutate.txt was being written during sync — if the winner sync captured"
  echo "  an old version and state was recorded, the newer local version may not be pushed."
  echo "  A second settlement sync should fix this (eventual consistency)."
  sync_once > /dev/null 2>&1
  if trees_match; then
    ok "S5: Trees match after second settlement sync (eventual consistency OK)"
  else
    bad "S5: Trees STILL diverged after two settlement syncs — state integrity issue"
  fi
fi

S5_END=$(date +%s)
TIMINGS="${TIMINGS}Scenario 5: $((S5_END - S5_START))s\n"

# ===================================================================
# SCENARIO 6: 10k-file scale
# ===================================================================
header "SCENARIO 6: 10k-file scale"
S6_START=$(date +%s)

full_clean
write_manifest

info "Generating 10,000 small files (100 dirs x 100 files)..."
GENERATE_START=$(date +%s)
python3 - <<'PYEOF'
import os, random, string

base = os.path.expanduser('~/caravan-chaos-sync')
for d in range(100):
    dirpath = os.path.join(base, 'dir{:03d}'.format(d))
    os.makedirs(dirpath, exist_ok=True)
    for f in range(100):
        fpath = os.path.join(dirpath, 'file{:03d}.txt'.format(f))
        rnd = ''.join(random.choices(string.ascii_lowercase, k=32))
        with open(fpath, 'w') as fh:
            fh.write('dir={} file={} rand={}\n'.format(d, f, rnd))
PYEOF
GENERATE_EXIT=$?
GENERATE_END=$(date +%s)
GENERATE_TIME=$((GENERATE_END - GENERATE_START))
info "Generation time: ${GENERATE_TIME}s (exit $GENERATE_EXIT)"

INITIAL_TIME=0; NOOP_TIME=0; INC_TIME=0

if [ "$GENERATE_EXIT" -ne 0 ]; then
  bad "S6: File generation failed"
  TIMINGS="${TIMINGS}Scenario 6: GENERATION FAILED\n"
else
  FILECOUNT=$(find "$LOCAL_DIR" -type f | wc -l | tr -d ' ')
  info "Files generated: $FILECOUNT"

  # Initial push
  INITIAL_START=$(date +%s)
  INITIAL_OUT=$(sync_once 2>&1)
  INITIAL_EXIT=$?
  INITIAL_END=$(date +%s)
  INITIAL_TIME=$((INITIAL_END - INITIAL_START))
  info "Initial push: ${INITIAL_TIME}s (exit $INITIAL_EXIT)"
  info "Output: $(echo "$INITIAL_OUT" | head -3)"
  TIMINGS="${TIMINGS}Scenario 6 initial push: ${INITIAL_TIME}s\n"

  if [ "$INITIAL_EXIT" -eq 0 ]; then
    ok "S6: Initial 10k-file push completed (${INITIAL_TIME}s)"
  else
    bad "S6: Initial 10k-file push failed (exit $INITIAL_EXIT): $INITIAL_OUT"
  fi

  # No-op sync
  NOOP_START=$(date +%s)
  NOOP_OUT=$(sync_once 2>&1)
  NOOP_EXIT=$?
  NOOP_END=$(date +%s)
  NOOP_TIME=$((NOOP_END - NOOP_START))
  info "No-op sync: ${NOOP_TIME}s (exit $NOOP_EXIT)"
  info "Output: $NOOP_OUT"
  TIMINGS="${TIMINGS}Scenario 6 no-op sync: ${NOOP_TIME}s\n"

  if echo "$NOOP_OUT" | grep -q "in sync"; then
    ok "S6: No-op sync reports 'in sync' (${NOOP_TIME}s)"
  else
    bad "S6: No-op sync did not report 'in sync' (got: $NOOP_OUT)"
  fi

  # Single-file incremental
  sleep 1
  echo "incremental-change-$(date +%s)" > "$LOCAL_DIR/dir000/file000.txt"
  INC_START=$(date +%s)
  INC_OUT=$(sync_once 2>&1)
  INC_EXIT=$?
  INC_END=$(date +%s)
  INC_TIME=$((INC_END - INC_START))
  info "Single-file incremental: ${INC_TIME}s (exit $INC_EXIT)"
  info "Output: $INC_OUT"
  TIMINGS="${TIMINGS}Scenario 6 single-file incremental: ${INC_TIME}s\n"

  if [ "$INC_EXIT" -eq 0 ]; then
    ok "S6: Single-file incremental sync completed (${INC_TIME}s)"
  else
    bad "S6: Single-file incremental sync failed"
  fi

  RCKSUM_INC=$(remote_sha256 "dir000/file000.txt")
  LCKSUM_INC=$(sha256_file "$LOCAL_DIR/dir000/file000.txt")
  if [ -n "$LCKSUM_INC" ] && [ "$RCKSUM_INC" = "$LCKSUM_INC" ]; then
    ok "S6: Incremental file correctly propagated to remote"
  else
    bad "S6: Incremental file not correctly propagated (local=$LCKSUM_INC remote=$RCKSUM_INC)"
  fi

  # Clean up 10k files thoroughly
  info "Cleaning up 10k files on both sides..."
  clean_state
  clean_local
  clean_remote
fi

S6_END=$(date +%s)
TIMINGS="${TIMINGS}Scenario 6 total: $((S6_END - S6_START))s\n"

# ===================================================================
# FINAL CLEANUP
# ===================================================================
header "FINAL CLEANUP"
full_clean
info "Cleaned up local dir, remote dir, state, lock, and conflict files."

# ===================================================================
# RESULTS SUMMARY
# ===================================================================
header "RESULTS SUMMARY"
echo ""
echo "PASS: $PASS"
echo "FAIL: $FAIL"
echo ""
echo "--- Timings ---"
printf "$TIMINGS"
echo ""
echo "Scale numbers (Scenario 6):"
echo "  File generation (10k):    ${GENERATE_TIME:-N/A}s"
echo "  Initial 10k-file push:    ${INITIAL_TIME}s"
echo "  No-op sync (10k files):   ${NOOP_TIME}s"
echo "  Single-file incremental:  ${INC_TIME}s"
echo ""
[ "$FAIL" -eq 0 ] && echo "ALL CHAOS SCENARIOS PASSED" || echo "$FAIL SCENARIO(S) FAILED — see above for evidence and root-cause"
[ "$FAIL" -eq 0 ]
