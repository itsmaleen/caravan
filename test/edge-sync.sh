#!/bin/bash
# Caravan edge-case sync probe.
# Exercises 10 edge cases against the real Mac mini over ssh.
# Usage: CARAVAN_BIN=$(pwd)/caravan ./test/edge-sync.sh
set -u

MINI="${CARAVAN_TEST_REMOTE:-user@example-host}"
LOCAL_DIR="$HOME/caravan-edge-sync"
REMOTE_DIR='caravan-edge-sync'
BIN="${CARAVAN_BIN:-$(pwd)/caravan}"
MANIFEST_DIR="$(mktemp -d)"
MANIFEST="$MANIFEST_DIR/caravan.toml"
STATE_DIR="$HOME/.config/caravan/sync-state"
STATE_FILE="$STATE_DIR/edge-folder.json"

PASS=0; FAIL=0
START_TIME=$(date +%s)

ok()  { PASS=$((PASS+1)); echo "  PASS: $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL: $1"; }

# Tree fingerprint helpers — files only, sorted, relative paths + sha256.
local_tree() {
  if [ ! -d "$LOCAL_DIR" ]; then echo ""; return; fi
  (cd "$LOCAL_DIR" && find . -type f ! -name .DS_Store -print0 2>/dev/null | LC_ALL=C sort -z | xargs -0 shasum -a 256 2>/dev/null | awk '{print $2, $1}' | iconv -f UTF-8-MAC -t UTF-8 | LC_ALL=C sort)
}
remote_tree() {
  ssh -o BatchMode=yes "$MINI" \
    "cd $REMOTE_DIR 2>/dev/null && find . -type f ! -name .DS_Store -print0 2>/dev/null | LC_ALL=C sort -z | xargs -0 shasum -a 256 2>/dev/null | awk '{print \$2, \$1}' | iconv -f UTF-8-MAC -t UTF-8 | LC_ALL=C sort" 2>/dev/null
}

trees_match() {
  local l r
  l="$(local_tree)"; r="$(remote_tree)"
  if [ "$l" = "$r" ]; then return 0; fi
  echo "    --- local ---"
  echo "$l" | sed 's/^/      /'
  echo "    --- remote ---"
  echo "$r" | sed 's/^/      /'
  return 1
}

dump_trees() {
  echo "    local_tree:  $(local_tree)"
  echo "    remote_tree: $(remote_tree)"
  echo "    state_file:  $(cat "$STATE_FILE" 2>/dev/null | head -5 | tr '\n' ' ')"
}

sync_once() { "$BIN" sync -f "$MANIFEST" edge-folder 2>&1; }

# Wipe both sides and the state file.
clean_all() {
  rm -rf "$LOCAL_DIR" "$STATE_FILE"
  ssh -o BatchMode=yes "$MINI" "rm -rf $REMOTE_DIR" 2>/dev/null
}

# Clean + create empty local root.
reseed() {
  clean_all
  mkdir -p "$LOCAL_DIR"
}

# ─────────────────────────────── preamble ────────────────────────────────────
echo "=== caravan edge-sync probe ==="
echo "  BIN=$BIN"
echo "  MINI=$MINI"

cat > "$MANIFEST" <<EOF
version = 1
[workspace]
root = "~/code"
[[sync]]
name   = "edge-folder"
local  = "~/caravan-edge-sync"
remote = "$MINI:~/caravan-edge-sync"
EOF

ssh -o BatchMode=yes -o ConnectTimeout=8 "$MINI" "echo ok" >/dev/null 2>&1 \
  || { echo "FATAL: ssh to $MINI unreachable"; exit 1; }

clean_all   # initial wipe

# ─────────────────────────────── round 1 ─────────────────────────────────────
echo
echo "== round 1: filenames with spaces =="
reseed
echo "hello spaces" > "$LOCAL_DIR/my file.txt"
mkdir -p "$LOCAL_DIR/dir with space"
echo "nested" > "$LOCAL_DIR/dir with space/nested file.txt"
OUT="$(sync_once)"; echo "$OUT"
if trees_match; then ok "r1: initial push with spaced filenames"; else
  bad "r1: initial push with spaced filenames"; dump_trees; fi

sleep 2
ssh -o BatchMode=yes "$MINI" \
  "echo 'edited remotely' >> \"$REMOTE_DIR/my file.txt\" && echo 'remote new' > \"$REMOTE_DIR/dir with space/remote file.txt\""
OUT="$(sync_once)"; echo "$OUT"
if trees_match; then ok "r1: trees match after remote edit pulled back"; else
  bad "r1: trees match after remote edit pulled back"; dump_trees; fi
if grep -q "edited remotely" "$LOCAL_DIR/my file.txt" 2>/dev/null; then
  ok "r1: remote edit to spaced filename pulled back"
else
  bad "r1: remote edit to spaced filename pulled back"
fi
if [ -f "$LOCAL_DIR/dir with space/remote file.txt" ]; then
  ok "r1: new spaced remote file pulled to local"
else
  bad "r1: new spaced remote file NOT pulled to local"
fi

# ─────────────────────────────── round 2 ─────────────────────────────────────
echo
echo "== round 2: unicode filenames =="
reseed
printf "unicode content 1\n" > "$LOCAL_DIR/héllo-wörld.txt"
printf "unicode content 2\n" > "$LOCAL_DIR/日本語.txt"
printf "unicode content 3\n" > "$LOCAL_DIR/🚀.txt"
OUT="$(sync_once)"; echo "$OUT"
if trees_match; then ok "r2: unicode filenames push and pull back identical"; else
  bad "r2: unicode filenames push and pull back identical"; dump_trees; fi
for f in "héllo-wörld.txt" "日本語.txt" "🚀.txt"; do
  if ssh -o BatchMode=yes "$MINI" "test -f \"$REMOTE_DIR/$f\"" 2>/dev/null; then
    ok "r2: '$f' present on remote"
  else
    bad "r2: '$f' NOT present on remote"
  fi
done

# ─────────────────────────────── round 3 ─────────────────────────────────────
echo
echo "== round 3: deep nesting (8 levels) =="
reseed
mkdir -p "$LOCAL_DIR/a/b/c/d/e/f/g/h"
echo "deep content" > "$LOCAL_DIR/a/b/c/d/e/f/g/h/file.txt"
OUT="$(sync_once)"; echo "$OUT"
if trees_match; then ok "r3: deep nesting synced correctly"; else
  bad "r3: deep nesting synced correctly"; dump_trees; fi
if ssh -o BatchMode=yes "$MINI" "test -f $REMOTE_DIR/a/b/c/d/e/f/g/h/file.txt"; then
  ok "r3: deep file present on remote"
else
  bad "r3: deep file NOT present on remote"
fi

# ─────────────────────────────── round 4 ─────────────────────────────────────
echo
echo "== round 4: empty directory propagation =="
reseed
echo "anchor" > "$LOCAL_DIR/anchor.txt"
OUT="$(sync_once)"; echo "$OUT"   # establish base with anchor
sleep 1
mkdir "$LOCAL_DIR/empty-dir"
OUT="$(sync_once)"; echo "$OUT"
if ssh -o BatchMode=yes "$MINI" "test -d $REMOTE_DIR/empty-dir"; then
  ok "r4: empty-dir propagated to remote"
else
  bad "r4: empty-dir NOT propagated to remote"
  echo "    remote ls: $(ssh -o BatchMode=yes "$MINI" "ls $REMOTE_DIR 2>&1")"
fi
# Confirm scan recorded the dir in state
if grep -q "empty-dir" "$STATE_FILE" 2>/dev/null; then
  ok "r4: empty-dir recorded in state file"
else
  bad "r4: empty-dir NOT in state file"
fi

# ─────────────────────────────── round 5 ─────────────────────────────────────
echo
echo "== round 5: type flip file→dir =="
reseed
echo "i am a file" > "$LOCAL_DIR/flip.txt"
OUT="$(sync_once)"; echo "$OUT"   # base: flip.txt is a file
sleep 1
rm "$LOCAL_DIR/flip.txt"
mkdir "$LOCAL_DIR/flip.txt"
echo "inner content" > "$LOCAL_DIR/flip.txt/inner.txt"
OUT5="$(sync_once)"; echo "$OUT5"
LOCAL5="$(local_tree)"; REMOTE5="$(remote_tree)"
echo "  local_tree:  $LOCAL5"
echo "  remote_tree: $REMOTE5"
if [ "$LOCAL5" = "$REMOTE5" ]; then
  ok "r5: trees match after file→dir flip"
else
  bad "r5: trees diverged after file→dir flip"
fi
# Is flip.txt on remote now a dir (not a plain file)?
R5_TYPE="$(ssh -o BatchMode=yes "$MINI" "stat -f '%HT' $REMOTE_DIR/flip.txt 2>/dev/null || echo unknown")"
echo "  remote flip.txt type: $R5_TYPE"
if echo "$R5_TYPE" | grep -qi "dir"; then
  ok "r5: remote flip.txt is a directory"
else
  bad "r5: remote flip.txt is NOT a directory (type=$R5_TYPE)"
fi

# ─────────────────────────────── round 6 ─────────────────────────────────────
echo
echo "== round 6: type flip dir→file =="
reseed
mkdir -p "$LOCAL_DIR/flipdir"
echo "dir content" > "$LOCAL_DIR/flipdir/f.txt"
OUT="$(sync_once)"; echo "$OUT"   # base: flipdir is a dir with f.txt
sleep 1
rm -rf "$LOCAL_DIR/flipdir"
echo "now a file" > "$LOCAL_DIR/flipdir"
OUT6="$(sync_once)"; echo "$OUT6"
LOCAL6="$(local_tree)"; REMOTE6="$(remote_tree)"
echo "  local_tree:  $LOCAL6"
echo "  remote_tree: $REMOTE6"
if [ "$LOCAL6" = "$REMOTE6" ]; then
  ok "r6: trees match after dir→file flip"
else
  bad "r6: trees diverged after dir→file flip"
fi
R6_TYPE="$(ssh -o BatchMode=yes "$MINI" "stat -f '%HT' $REMOTE_DIR/flipdir 2>/dev/null || echo unknown")"
echo "  remote flipdir type: $R6_TYPE"
if echo "$R6_TYPE" | grep -qi "regular"; then
  ok "r6: remote flipdir is now a regular file"
else
  bad "r6: remote flipdir is NOT a regular file (type=$R6_TYPE)"
fi

# ─────────────────────────────── round 7 ─────────────────────────────────────
echo
echo "== round 7: same-size conflict (new on both sides) =="
reseed
echo "anchor" > "$LOCAL_DIR/anchor.txt"
OUT="$(sync_once)"; echo "$OUT"   # establish base (no collision.txt in base)
# Write same-size content at both sides before next sync.
printf 'aaaa' > "$LOCAL_DIR/collision.txt"
ssh -o BatchMode=yes "$MINI" "printf 'bbbb' > $REMOTE_DIR/collision.txt"
LOCAL_MT="$(stat -f '%m' "$LOCAL_DIR/collision.txt" 2>/dev/null)"
REMOTE_MT="$(ssh -o BatchMode=yes "$MINI" "stat -f '%m' $REMOTE_DIR/collision.txt 2>/dev/null")"
echo "  local  collision.txt mtime: $LOCAL_MT"
echo "  remote collision.txt mtime: $REMOTE_MT"
OUT7="$(sync_once)"; echo "$OUT7"
WIN_LOCAL="$(cat "$LOCAL_DIR/collision.txt" 2>/dev/null)"
WIN_REMOTE="$(ssh -o BatchMode=yes "$MINI" "cat $REMOTE_DIR/collision.txt 2>/dev/null")"
echo "  local  content after sync: '$WIN_LOCAL'"
echo "  remote content after sync: '$WIN_REMOTE'"
if [ "$WIN_LOCAL" = "$WIN_REMOTE" ]; then
  ok "r7: conflict resolved consistently — both sides: '$WIN_LOCAL'"
else
  bad "r7: conflict NOT consistent — local='$WIN_LOCAL' remote='$WIN_REMOTE'"
fi
if echo "$OUT7" | grep -q "conflict"; then
  ok "r7: conflict reported in sync output"
else
  bad "r7: conflict NOT reported in sync output (got: $OUT7)"
fi
# Probe same-size-same-mtime existing file: change detection relies on mtime.
# If we trick both sides to have same mtime after writing different content
# no plan action fires (see plan.go:66-67). Document observed behavior:
echo "  NOTE: change detection is size+mtime only (scan.go returns no hash)."
echo "        If size and mtime match post-sync, diverged content is invisible."

# ─────────────────────────────── round 8 ─────────────────────────────────────
echo
echo "== round 8: single-quote filename skipped cleanly =="
reseed
echo "normal file" > "$LOCAL_DIR/normal.txt"
touch "$LOCAL_DIR/it's.txt"
OUT8="$(sync_once)"; echo "$OUT8"
# Warning check
if echo "$OUT8" | grep -qi "skip.*it\|single.quote\|it.*s\.txt"; then
  ok "r8: warning emitted for single-quote filename"
else
  bad "r8: NO warning emitted for single-quote filename (output: $OUT8)"
fi
# normal.txt should have synced
if ssh -o BatchMode=yes "$MINI" "test -f $REMOTE_DIR/normal.txt"; then
  ok "r8: normal.txt synced despite single-quote file"
else
  bad "r8: normal.txt NOT synced"
fi
# it's.txt must NOT be on remote (quoting breakage check)
# We test by listing the remote dir and grepping — avoids quoting the path in test -e.
REMOTE_LS="$(ssh -o BatchMode=yes "$MINI" "ls $REMOTE_DIR 2>/dev/null")"
if echo "$REMOTE_LS" | grep -qF "it's.txt"; then
  bad "r8: single-quote file WAS pushed to remote"
else
  ok "r8: single-quote file correctly absent from remote"
fi
# Confirm sync exit code was 0 (no crash)
"$BIN" sync -f "$MANIFEST" edge-folder >/dev/null 2>&1 && ok "r8: no-op re-sync exits 0 (no corruption)" || bad "r8: re-sync failed after single-quote round"

# ─────────────────────────────── round 9 ─────────────────────────────────────
echo
echo "== round 9: local delete + remote modify (modification wins) =="
reseed
echo "victim content" > "$LOCAL_DIR/victim.txt"
OUT="$(sync_once)"; echo "$OUT"   # base: victim.txt on both
sleep 2
rm "$LOCAL_DIR/victim.txt"
ssh -o BatchMode=yes "$MINI" "echo 'remote modification' >> $REMOTE_DIR/victim.txt"
OUT9="$(sync_once)"; echo "$OUT9"
if [ -f "$LOCAL_DIR/victim.txt" ]; then
  ok "r9: victim.txt restored locally (modification wins deletion)"
  if grep -q "remote modification" "$LOCAL_DIR/victim.txt"; then
    ok "r9: restored content contains remote modification"
  else
    bad "r9: restored content does NOT contain remote modification (got: $(cat "$LOCAL_DIR/victim.txt"))"
  fi
else
  bad "r9: victim.txt NOT restored — deletion propagated instead (plan.go:116-121 modification path not triggered)"
fi
if trees_match; then ok "r9: trees match after modification-wins-deletion"; else
  bad "r9: trees diverged after round 9"; dump_trees; fi

# ─────────────────────────────── round 10 ────────────────────────────────────
echo
echo "== round 10: 5-level rename dir1→dir2 (delete+add) =="
reseed
mkdir -p "$LOCAL_DIR/dir1"
echo "file a" > "$LOCAL_DIR/dir1/a.txt"
echo "file b" > "$LOCAL_DIR/dir1/b.txt"
OUT="$(sync_once)"; echo "$OUT"   # base: dir1/{a,b}.txt on both
sleep 1
mv "$LOCAL_DIR/dir1" "$LOCAL_DIR/dir2"
OUT10="$(sync_once)"; echo "$OUT10"
if trees_match; then ok "r10: trees match after dir1→dir2 rename"; else
  bad "r10: trees diverged after dir1→dir2 rename"; dump_trees; fi
if ssh -o BatchMode=yes "$MINI" "test -d $REMOTE_DIR/dir2"; then
  ok "r10: dir2 present on remote"
else
  bad "r10: dir2 NOT present on remote"
fi
if ssh -o BatchMode=yes "$MINI" "test ! -e $REMOTE_DIR/dir1"; then
  ok "r10: dir1 removed from remote"
else
  bad "r10: dir1 still present on remote"
fi

# ─────────────────────────────── cleanup ─────────────────────────────────────
echo
echo "=== CLEANUP ==="
clean_all
echo "  Both sides and state file removed."

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))

echo
echo "=============================="
echo "RESULT: $PASS passed, $FAIL failed"
echo "Runtime: ${ELAPSED}s"
echo "=============================="
[ "$FAIL" -eq 0 ]
