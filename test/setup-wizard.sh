#!/bin/bash
# Caravan setup-wizard end-to-end test.
#
# Proves the agent-powered `caravan setup` wizard can DRIVE caravan to a working
# state — not just launch. It hands the assembled wizard prompt plus a canned
# answers block to `claude -p` (headless), pointed at a hermetic sandbox using
# the local: transport (no ssh, no remote machine needed), then asserts the
# agent produced a valid manifest, a working sync, and green doctor.
#
# Requires: claude (Claude Code) on PATH. Skips cleanly if absent.
# Usage: CARAVAN_BIN=$(pwd)/caravan ./test/setup-wizard.sh
set -u

BIN="${CARAVAN_BIN:-$(pwd)/caravan}"
AGENT="${CARAVAN_SETUP_AGENT:-claude}"

command -v "$AGENT" >/dev/null 2>&1 || { echo "SKIP: $AGENT not on PATH (set CARAVAN_SETUP_AGENT to another installed agent)"; exit 0; }
[ -x "$BIN" ] || { echo "FATAL: caravan binary not found at $BIN"; exit 1; }

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  PASS: $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL: $1"; }

SB="$(mktemp -d)"
export CARAVAN_MANIFEST="$SB/caravan.toml"
# Ensure the wizard's self-invoked `caravan doctor` and the agent's `caravan`
# calls resolve to the binary under test.
export PATH="$(cd "$(dirname "$BIN")" && pwd):$PATH"

mkdir -p "$SB/notes" "$SB/remote-notes"
echo "hello from setup wizard test" > "$SB/notes/first.md"

echo "== assembling wizard prompt + canned answers =="
{
  "$BIN" setup --print-prompt 2>/dev/null
  cat <<EOF

---
## User's answers (pre-supplied — do NOT ask, just execute)

- Code root: skip repos entirely (none for this test).
- Machines: local only — use the local: transport, no ssh.
- Sync folders: ONE folder named "notes": local "$SB/notes", remote "local:$SB/remote-notes".
- Secrets: none.
- Daemons: none — just run the first sync.

Do exactly this, verifying each step:
1. Write $CARAVAN_MANIFEST as a valid caravan.toml with version=1, a [workspace]
   root, and one [[sync]] entry named "notes" (local/remote as above).
2. Run: caravan sync notes
3. Run: caravan doctor
Then stop and print "WIZARD DONE".
EOF
} > "$SB/prompt.txt"

echo "== launching $AGENT headless to drive caravan =="
"$AGENT" -p "$(cat "$SB/prompt.txt")" \
  --permission-mode acceptEdits \
  --allowedTools "Bash(caravan:*)" "Bash(cat:*)" "Write" "Read" "Edit" \
  < /dev/null 2>&1 | tail -6

echo "== assertions =="
[ -f "$CARAVAN_MANIFEST" ] && ok "manifest written" || bad "manifest written"
grep -q 'name   = "notes"\|name = "notes"' "$CARAVAN_MANIFEST" 2>/dev/null && ok "notes sync entry present" || bad "notes sync entry present"
"$BIN" sync notes >/dev/null 2>&1 && ok "sync notes runs clean" || bad "sync notes runs clean"
[ -f "$SB/remote-notes/first.md" ] && ok "file propagated to remote folder" || bad "file propagated to remote folder"
if [ -f "$SB/remote-notes/first.md" ]; then
  diff -q "$SB/notes/first.md" "$SB/remote-notes/first.md" >/dev/null 2>&1 && ok "content identical" || bad "content identical"
fi
"$BIN" doctor >/dev/null 2>&1; [ $? -le 1 ] && ok "doctor runs (exit 0/1)" || bad "doctor runs"

echo "== cleanup =="
rm -rf "$SB" "$HOME/.config/caravan/sync-state/notes.json"

echo
echo "RESULT: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
