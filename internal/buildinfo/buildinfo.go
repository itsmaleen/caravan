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
const Version = "0.8.1"
