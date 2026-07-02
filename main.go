// Caravan — drive for devs. One manifest, identical workspaces everywhere.
package main

import (
	"fmt"
	"os"

	"caravan/internal/manifest"
	"caravan/internal/provision"
	"caravan/internal/secrets"
	"caravan/internal/syncengine"
)

const version = "0.1.0"

const usage = `caravan — one manifest, identical dev workspaces everywhere

Usage:
  caravan init [--root DIR] [--force] [-f MANIFEST]     discover repos, draft manifest
  caravan up [--dry-run] [--only a,b] [-f MANIFEST]     provision workspace (clone/pull, secrets, toolchain)
  caravan status [-f MANIFEST]                          repo + sync-folder status
  caravan secrets <init|set|show|add-machine> [...]     manage encrypted secrets sidecar
  caravan sync [NAME] [--watch] [--interval 2s] [--dry-run] [--bootstrap] [-f MANIFEST]
                                                        bidirectional folder sync (ssh or local:)
  caravan scan --json DIR [--exclude a,b]               (internal) emit dir state as JSON
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
	case "scan":
		code = syncengine.CmdScan(args)
	case "version", "--version", "-v":
		fmt.Println("caravan " + version)
	case "help", "--help", "-h":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "caravan: unknown command %q\n\n%s\n", os.Args[1], usage)
		code = 2
	}
	os.Exit(code)
}
