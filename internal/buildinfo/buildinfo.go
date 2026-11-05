// Package buildinfo provides build-time constants for caravan.
package buildinfo

// Version is the current caravan release version.
// 0.6.1: ssh keepalive (ServerAliveInterval/ConnectTimeout) + a hard timeout on
// the remote long-poll, so a half-open connection after sleep can no longer
// freeze the watch loop indefinitely. See docs/reliability-issues.md.
const Version = "0.6.1"
