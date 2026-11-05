// Caravan — drive for devs. One manifest, identical workspaces everywhere.
package main

import (
	"fmt"
	"os"

	"caravan/internal/buildinfo"
	"caravan/internal/doctor"
	"caravan/internal/gitremote"
	"caravan/internal/manifest"
	"caravan/internal/provision"
	"caravan/internal/secrets"
	"caravan/internal/service"
	"caravan/internal/setup"
	"caravan/internal/syncengine"
)

const usage = `caravan — one manifest, identical dev workspaces everywhere

Usage:
  caravan init [--root DIR] [--force] [-f MANIFEST]     discover repos, draft manifest
  caravan up [--dry-run] [--only a,b] [-f MANIFEST]     provision workspace (clone/pull, secrets, toolchain)
  caravan status [-f MANIFEST]                          repo + sync-folder status
  caravan secrets <init|set|show|add-machine> [...]     manage encrypted secrets sidecar
  caravan doctor [-f MANIFEST]                          diagnose environment, repos, sync, secrets
  caravan sync [NAME] [--watch] [--interval 2s] [--dry-run] [--bootstrap] [-f MANIFEST]
                                                        bidirectional folder sync (ssh or local:)
  caravan daemon <install|uninstall|status> [NAME] [--interval 5s] [-f MANIFEST]
                                                        run sync continuously via launchd (macOS)
  caravan service <apply|status|logs|rm> [NAME] [--dry-run] [--prune] [--allow-public] [-f MANIFEST]
                                                        supervise services + publish them at their declared visibility
  caravan setup [--agent NAME] [--headless] [--print-prompt] [-f MANIFEST]
                                                        agent-powered interactive setup wizard
  caravan scan --json DIR [--exclude a,b]               (internal) emit dir state as JSON
  caravan remote <setup|status> [repo] [-f MANIFEST]    configure split-remote routing + install pre-push hook
  caravan publish <branch> [--repo NAME] [--onto main] [--at RFC3339] [--force] [-f MANIFEST]
                                                        promote private branch to public remote (immediate or scheduled)
  caravan publish --run-due [-f MANIFEST]               fire all due scheduled publishes
  caravan guard-prepush <remote-name> <remote-url>      (hook) block private branches from public remote
  caravan version                                       print version

Manifest resolution: -f flag > $CARAVAN_MANIFEST > ~/.config/caravan/caravan.toml`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	args := os.Args[2:]
	var code int
	switch os.Args[1] {
	case "init":
		code = manifest.CmdInit(args)
	case "up":
		code = provision.CmdUp(args)
	case "status":
		code = provision.CmdStatus(args)
	case "secrets":
		code = secrets.CmdSecrets(args)
	case "sync":
		code = syncengine.CmdSync(args)
	case "daemon":
		code = syncengine.CmdDaemon(args)
	case "service":
		code = service.CmdService(args)
	case "doctor":
		code = doctor.CmdDoctor(args)
	case "setup":
		code = setup.CmdSetup(args)
	case "scan":
		code = syncengine.CmdScan(args)
	case "remote":
		code = gitremote.CmdRemote(args)
	case "publish":
		code = gitremote.CmdPublish(args)
	case "guard-prepush":
		code = gitremote.CmdGuardPrepush(args)
	case "version", "--version", "-v":
		fmt.Println("caravan " + buildinfo.Version)
	case "help", "--help", "-h":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "caravan: unknown command %q\n\n%s\n", os.Args[1], usage)
		code = 2
	}
	os.Exit(code)
}
