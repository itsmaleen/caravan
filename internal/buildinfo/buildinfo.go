// Package buildinfo provides build-time constants for caravan.
package buildinfo

// Version is the current caravan release version.
// 0.6.1: ssh keepalive (ServerAliveInterval/ConnectTimeout) + a hard timeout on
// the remote long-poll, so a half-open connection after sleep can no longer
// freeze the watch loop indefinitely. See docs/reliability-issues.md.
// 0.8.0: native file watching, two sync-race fixes, 5 s keepalive, and
// move-staging (one-process remote moves on push).
// 0.8.1: remote stat after a pass, long-poll --since digest, and an
// upgrade-only version handshake (never pushes over a newer remote).
// 0.8.2: mid-pass deletes propagate; binary installed by rename (macOS
// SIGKILLs a binary overwritten in place); re-install on exit 126/137.
// 0.8.3: a long-poll with native events and a since digest skips its
// in-window safety rescans (most of the hub's CPU with several spokes).
// 0.8.4: a pass that left work unfinished is retried on a timer; a failed
// delete is retried, not undone; a chmod refreshes the mode only.
const Version = "0.8.4"
