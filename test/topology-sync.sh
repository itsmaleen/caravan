#!/bin/bash
# Caravan hub-and-spoke topology test: three replicas across two machines.
#
#   A  = MacBook  ~/caravan-topo-A          (this machine)
#   hub= mini     ~/caravan-topo-hub        (always-on hub)
#   B  = mini     ~/caravan-topo-B          (second replica, synced hub<->B by a
#                                            caravan watch RUNNING ON THE MINI
#                                            over the local: transport)
#
# A<->hub runs as a watch on this machine. Changes must flow A->hub->B and
# B->hub->A. Conflicts between A and B are mediated by the hub (newer wins,
# losers backed up on the losing side).
set -u

MINI="${CARAVAN_TEST_REMOTE:-}"
[ -n "$MINI" ] || { echo "usage: set CARAVAN_TEST_REMOTE=user@host (ssh target for the test remote)"; exit 2; }
BIN="${CARAVAN_BIN:-$(pwd)/caravan}"
A="$HOME/caravan-topo-A"
MAN_A="$(mktemp -d)/caravan.toml"

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  PASS: $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL: $1"; }

# wait_for <desc> <timeout-s> <cmd...>  — poll until cmd succeeds
wait_for() {
  local desc="$1" timeout="$2"; shift 2
  local waited=0
  while ! "$@" >/dev/null 2>&1; do
    sleep 0.5; waited=$((waited+1))
    if [ "$waited" -ge $((timeout*2)) ]; then bad "$desc (timeout ${timeout}s)"; return 1; fi
  done
  ok "$desc ($(python3 -c "print(f'{$waited*0.5:.1f}')")s)"
}

on_mini() { ssh -o BatchMode=yes "$MINI" "$@"; }

echo "== setup =="
rm -rf "$A" "$HOME/.config/caravan/sync-state/topo-A.json"
on_mini 'rm -rf caravan-topo-hub caravan-topo-B ~/.config/caravan/sync-state/topo-B.json ~/Library/Logs/caravan/topo-B.log'
mkdir -p "$A"

cat > "$MAN_A" <<EOF
version = 1
[workspace]
root = "~/code"
[[sync]]
name   = "topo-A"
local  = "~/caravan-topo-A"
remote = "$MINI:~/caravan-topo-hub"
EOF

# Mini-side manifest: append the hub<->B entry if not present.
# (local: transport needs an absolute path; derive the remote home at runtime)
RHOME="$(on_mini 'echo $HOME')"
on_mini "grep -q caravan-topo-B ~/.config/caravan/caravan.toml 2>/dev/null || cat >> ~/.config/caravan/caravan.toml <<EOF

[[sync]]
name   = \"topo-B\"
local  = \"~/caravan-topo-hub\"
remote = \"local:$RHOME/caravan-topo-B\"
EOF"

echo "== start hub<->B daemon on the mini =="
on_mini '~/.local/bin/caravan daemon install topo-B --interval 1s' && ok "daemon installed on mini" || bad "daemon install on mini"
sleep 2
on_mini '~/.local/bin/caravan daemon status topo-B' | grep -q "✓" && ok "mini daemon running" || bad "mini daemon running"

echo "== start A<->hub watch locally =="
"$BIN" sync --watch -f "$MAN_A" topo-A > /tmp/topo-watch.log 2>&1 &
WPID=$!
sleep 3

echo "== A -> hub -> B propagation =="
echo "born on A" > "$A/from-a.txt"
wait_for "from-a.txt reaches B via hub" 30 on_mini 'test -f caravan-topo-B/from-a.txt'

echo "== B -> hub -> A propagation =="
on_mini 'echo "born on B" > caravan-topo-B/from-b.txt'
wait_for "from-b.txt reaches A via hub" 30 test -f "$A/from-b.txt"

echo "== delete propagation A -> B =="
rm "$A/from-a.txt"
wait_for "deletion of from-a.txt reaches B" 30 on_mini 'test ! -e caravan-topo-B/from-a.txt'

echo "== three-way content parity =="
sleep 3
echo "shared" > "$A/parity.txt"
wait_for "parity.txt reaches B" 30 on_mini 'test -f caravan-topo-B/parity.txt'
HA=$(shasum -a 256 "$A/parity.txt" | awk '{print $1}')
HH=$(on_mini 'shasum -a 256 caravan-topo-hub/parity.txt' | awk '{print $1}')
HB=$(on_mini 'shasum -a 256 caravan-topo-B/parity.txt' | awk '{print $1}')
[ "$HA" = "$HH" ] && [ "$HH" = "$HB" ] && ok "A, hub, B byte-identical" || bad "replica divergence: A=$HA hub=$HH B=$HB"

echo "== A-vs-B conflict mediated by hub (B newer wins) =="
echo "A version" > "$A/clash.txt"
wait_for "clash.txt seeded to B" 30 on_mini 'test -f caravan-topo-B/clash.txt'
sleep 2
on_mini 'echo "B version wins" > caravan-topo-B/clash.txt'
wait_for "B version reaches A" 40 grep -q "B version wins" "$A/clash.txt"

echo "== cleanup =="
kill $WPID 2>/dev/null
on_mini '~/.local/bin/caravan daemon uninstall topo-B' >/dev/null 2>&1 && ok "mini daemon uninstalled" || bad "mini daemon uninstall"
rm -rf "$A" "$HOME/.config/caravan/sync-state/topo-A.json"
on_mini 'rm -rf caravan-topo-hub caravan-topo-B ~/.config/caravan/sync-state/topo-B.json'
# strip the topo-B entry from the mini manifest again
on_mini 'python3 - <<PYEOF
import os, re
p = os.path.expanduser("~/.config/caravan/caravan.toml")
s = open(p).read()
s = re.sub(r"\n*\[\[sync\]\]\nname   = \"topo-B\"\nlocal  = \"~/caravan-topo-hub\"\nremote = \"local:[^\"]*caravan-topo-B\"\n?", "\n", s)
open(p, "w").write(s)
PYEOF'

echo
echo "RESULT: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
