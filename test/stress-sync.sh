#!/bin/bash
# Caravan sync stress test: 1000 small files + a few binary blobs, cross-device.
# Measures initial push, no-op scan, and incremental single-file propagation.
set -u
MINI="${CARAVAN_TEST_REMOTE:-user@example-host}"
BIN="${CARAVAN_BIN:-$(pwd)/caravan}"
LOCAL_DIR="$HOME/caravan-stress-sync"
MANIFEST="$(mktemp -d)/caravan.toml"

cat > "$MANIFEST" <<EOF
version = 1
[workspace]
root = "~/code"
[[sync]]
name   = "stress"
local  = "~/caravan-stress-sync"
remote = "$MINI:~/caravan-stress-sync"
EOF

echo "== setup: 1000 files across 10 dirs + 3 x 5MB blobs =="
rm -rf "$LOCAL_DIR" "$HOME/.config/caravan/sync-state/stress.json"
ssh -o BatchMode=yes "$MINI" 'rm -rf caravan-stress-sync'
for d in $(seq 0 9); do
  mkdir -p "$LOCAL_DIR/dir$d"
  for f in $(seq 0 99); do echo "content $d-$f $(date +%s)" > "$LOCAL_DIR/dir$d/file$f.txt"; done
done
for b in 1 2 3; do dd if=/dev/urandom of="$LOCAL_DIR/blob$b.bin" bs=1m count=5 2>/dev/null; done

echo "== initial push (1003 files, ~15MB) =="
time "$BIN" sync -f "$MANIFEST" stress | tail -2

echo "== remote count =="
ssh -o BatchMode=yes "$MINI" 'find caravan-stress-sync -type f | wc -l'

echo "== no-op sync =="
time "$BIN" sync -f "$MANIFEST" stress

echo "== incremental: touch 1 file =="
sleep 1
echo "changed" >> "$LOCAL_DIR/dir5/file50.txt"
time "$BIN" sync -f "$MANIFEST" stress

echo "== verify incremental content on remote =="
ssh -o BatchMode=yes "$MINI" 'tail -1 caravan-stress-sync/dir5/file50.txt'

echo "== cleanup =="
rm -rf "$LOCAL_DIR" "$HOME/.config/caravan/sync-state/stress.json"
ssh -o BatchMode=yes "$MINI" 'rm -rf caravan-stress-sync'
echo done
