// Package service supervises the long-running processes declared in the
// manifest and publishes them at their declared visibility.
//
// The design rule that shapes everything here: caravan never widens exposure
// on its own. `visibility = "public"` is refused unless a human passes
// --allow-public, and `apply` only tears down existing exposure when asked
// with --prune. Exposure changes are decisions, not side effects.
package service

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"caravan/internal/manifest"
)

// LaunchAgentsDir overrides ~/Library/LaunchAgents. Tests set this.
var LaunchAgentsDir = ""

// Usage text for `caravan service`.
const Usage = `Usage:
  caravan service apply [NAME] [--dry-run] [--prune] [--allow-public] [-f MANIFEST]
  caravan service status [NAME] [--json] [-f MANIFEST]
  caravan service logs NAME [--lines N] [-f MANIFEST]
  caravan service rm NAME [-f MANIFEST]`

// CmdService implements `caravan service <sub> [...]`.
func CmdService(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, Usage)
		return 2
	}
	switch args[0] {
	case "apply":
		return cmdApply(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "logs":
		return cmdLogs(args[1:])
	case "rm":
		return cmdRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "service: unknown sub-command %q\n\n%s\n", args[0], Usage)
		return 2
	}
}

// ── labels, paths, ports ──────────────────────────────────────────────────────

// Label returns the launchd label for a service.
func Label(name string) string { return "dev.caravan.service." + name }

// LogPath returns the log file for a service.
func LogPath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, "Library", "Logs", "caravan", "service-"+name+".log")
}

func launchAgentsDir() string {
	if LaunchAgentsDir != "" {
		return LaunchAgentsDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "LaunchAgents"
	}
	return filepath.Join(home, "Library", "LaunchAgents")
}

// PlistPath returns the plist location for a service.
func PlistPath(name string) string {
	return filepath.Join(launchAgentsDir(), Label(name)+".plist")
}

// Auto-allocated ports live in a high, rarely-used range so they do not fight
// with whatever else the machine runs.
const (
	autoPortLow  = 18800
	autoPortHigh = 18999
)

// AutoPort derives a stable port from the service name, then walks forward
// past anything in `taken`. Deterministic so that re-running apply on a second
// machine produces the same port without a shared registry.
func AutoPort(name string, taken map[int]bool) int {
	sum := sha256.Sum256([]byte(name))
	span := uint32(autoPortHigh - autoPortLow + 1)
	p := autoPortLow + int(binary.BigEndian.Uint32(sum[:4])%span)
	for i := 0; i < int(span); i++ {
		cand := autoPortLow + ((p - autoPortLow + i) % int(span))
		if !taken[cand] {
			return cand
		}
	}
	return 0
}

// ResolvePorts assigns a concrete port to every service, honouring explicit
// ports first so that auto-allocation never steals one.
func ResolvePorts(services []manifest.Service) map[string]int {
	taken := map[int]bool{}
	out := map[string]int{}
	for _, s := range services {
		if s.Port != "" && !s.PortIsAuto() {
			if p, err := strconv.Atoi(s.Port); err == nil {
				out[s.Name] = p
				taken[p] = true
			}
		}
	}
	for _, s := range services {
		if s.PortIsAuto() {
			p := AutoPort(s.Name, taken)
			out[s.Name] = p
			taken[p] = true
		}
	}
	return out
}

// ExpandCommand substitutes {{port}} in a command string.
func ExpandCommand(cmd string, port int) string {
	if port == 0 {
		return cmd
	}
	return strings.ReplaceAll(cmd, "{{port}}", strconv.Itoa(port))
}

// Target returns the proxy target URL for a service's port.
func Target(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

// ── plist generation (pure, unit-testable) ────────────────────────────────────

// Plist renders a launchd plist that runs command in dir under `/bin/sh -lc`.
func Plist(label, dir, command string, env map[string]string, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + xmlEscape(label) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-lc</string>
		<string>` + xmlEscape(command) + `</string>
	</array>
`)
	if dir != "" {
		b.WriteString("\t<key>WorkingDirectory</key>\n\t<string>" + xmlEscape(dir) + "</string>\n")
	}
	if len(env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range sortedKeys(env) {
			b.WriteString("\t\t<key>" + xmlEscape(k) + "</key>\n")
			b.WriteString("\t\t<string>" + xmlEscape(env[k]) + "</string>\n")
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString(`	<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(logPath) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// filterServices returns all services, or just the named one.
func filterServices(services []manifest.Service, name string) []manifest.Service {
	if name == "" {
		return services
	}
	for _, s := range services {
		if s.Name == name {
			return []manifest.Service{s}
		}
	}
	return nil
}
