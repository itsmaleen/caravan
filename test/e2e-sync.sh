#!/bin/bash
# Caravan cross-device e2e sync test.
# Runs from the caravan repo root on the primary machine; exercises bidirectional
# sync of a test folder against the Mac mini over ssh, asserting content parity
# after each mutation round.
set -u

MINI="${CARAVAN_TEST_REMOTE:-user@example-host}"
LOCAL_DIR="$HOME/caravan-test-sync"
REMOTE_DIR='caravan-test-sync'   # relative to remote $HOME
BIN="${CARAVAN_BIN:-$(pwd)/caravan}"
MANIFEST="$(mktemp -d)/caravan.toml"
STATE_DIR="$HOME/.config/caravan/sync-state"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1"; }
check() { # check <desc> <cmd...>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi
}

# Tree fingerprint: relative path + sha256 of every file, sorted.
local_tree()  { (cd "$LOCAL_DIR" && find . -type f ! -name .DS_Store ! -path '*/node_modules/*' -print0 | sort -z | xargs -0 shasum -a 256 2>/dev/null | awk '{print $2, $1}'); }
remote_tree() { ssh -o BatchMode=yes "$MINI" "cd $REMOTE_DIR && find . -type f ! -name .DS_Store ! -path '*/node_modules/*' -print0 | sort -z | xargs -0 shasum -a 256 2>/dev/null | awk '{print \$2, \$1}'"; }

trees_match() {
  local l r
  l="$(local_tree)"; r="$(remote_tree)"
  if [ "$l" = "$r" ]; then return 0; else
    echo "--- local ---"; echo "$l"; echo "--- remote ---"; echo "$r"; return 1
  fi
}

sync_once() { "$BIN" sync -f "$MANIFEST" test-folder; }

echo "== setup =="
rm -rf "$LOCAL_DIR" "$STATE_DIR/test-folder.json"
ssh -o BatchMode=yes "$MINI" "rm -rf $REMOTE_DIR" || { echo "ssh unreachable"; exit 1; }
mkdir -p "$LOCAL_DIR/sub/deep" "$LOCAL_DIR/node_modules/fake"
cat > "$MANIFEST" <<EOF
version = 1
[workspace]
root = "~/code"
[[sync]]
name   = "test-folder"
local  = "~/caravan-test-sync"
remote = "$MINI:~/caravan-test-sync"
EOF
echo "hello from $(hostname)" > "$LOCAL_DIR/hello.txt"
echo "nested content"          > "$LOCAL_DIR/sub/deep/nested.txt"
echo "# notes"                 > "$LOCAL_DIR/notes.md"
echo "excluded"                > "$LOCAL_DIR/node_modules/fake/dep.js"
touch "$LOCAL_DIR/.DS_Store"
dd if=/dev/urandom of="$LOCAL_DIR/blob.bin" bs=1024 count=512 2>/dev/null

echo "== round 1: initial push =="
sync_once || bad "sync exit code (initial push)"
check "trees match after initial push" trees_match
if ssh "$MINI" "test -e $REMOTE_DIR/node_modules"; then bad "excludes: node_modules leaked to remote"; else ok "excludes: node_modules not pushed"; fi

echo "== round 2: no-op sync is clean =="
OUT="$(sync_once 2>&1)"; echo "$OUT" | grep -q "in sync" && ok "no-op reports in sync" || bad "no-op reports in sync (got: $OUT)"

echo "== round 3: remote edits propagate back =="
sleep 2
ssh "$MINI" "echo 'edited on mini' >> $REMOTE_DIR/hello.txt && echo 'born on mini' > $REMOTE_DIR/remote-new.txt && rm $REMOTE_DIR/notes.md && mkdir -p $REMOTE_DIR/mini-dir && echo x > $REMOTE_DIR/mini-dir/f.txt"
sync_once || bad "sync exit code (pull round)"
check "trees match after remote edits" trees_match
grep -q "edited on mini" "$LOCAL_DIR/hello.txt" && ok "remote modification pulled" || bad "remote modification pulled"
[ -f "$LOCAL_DIR/remote-new.txt" ] && ok "remote new file pulled" || bad "remote new file pulled"
[ ! -e "$LOCAL_DIR/notes.md" ] && ok "remote deletion propagated to local" || bad "remote deletion propagated to local"

echo "== round 4: local edits propagate =="
sleep 2
echo "local addition" > "$LOCAL_DIR/local-new.txt"
rm "$LOCAL_DIR/remote-new.txt"
echo "local edit" >> "$LOCAL_DIR/sub/deep/nested.txt"
sync_once || bad "sync exit code (push round)"
check "trees match after local edits" trees_match
ssh "$MINI" "test ! -e $REMOTE_DIR/remote-new.txt" && ok "local deletion propagated to remote" || bad "local deletion propagated to remote"

echo "== round 5: conflict — newer (remote) wins =="
sleep 2
echo "local version" > "$LOCAL_DIR/conflict.txt"
sleep 2
ssh "$MINI" "echo 'remote version' > $REMOTE_DIR/conflict.txt"
sync_once || bad "sync exit code (conflict round)"
check "trees match after conflict" trees_match
grep -q "remote version" "$LOCAL_DIR/conflict.txt" && ok "newer remote wins conflict" || bad "newer remote wins conflict (got: $(cat "$LOCAL_DIR/conflict.txt"))"

echo "== round 6: conflict — newer (local) wins =="
sleep 2
ssh "$MINI" "echo 'remote v2' > $REMOTE_DIR/conflict.txt"
sleep 2
echo "local v2" > "$LOCAL_DIR/conflict.txt"
sync_once || bad "sync exit code (conflict round 2)"
check "trees match after conflict 2" trees_match
R="$(ssh "$MINI" "cat $REMOTE_DIR/conflict.txt")"
[ "$R" = "local v2" ] && ok "newer local wins conflict" || bad "newer local wins conflict (remote has: $R)"

echo
echo "RESULT: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
