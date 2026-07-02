// Package cliargs provides flag-parsing helpers for caravan commands.
package cliargs

import "flag"

// ParseAnywhere parses flags from fs even when they appear after positional
// arguments (interleaved). It returns the positional (non-flag) arguments in
// the order they appeared.
//
// Algorithm: call fs.Parse repeatedly; each time any positionals remain,
// collect the first one and re-parse the rest. This lets flags appear
// anywhere relative to positionals.
//
// Terminating "--" works naturally: flag.FlagSet.Parse consumes "--" and
// returns everything after it as remaining args, which ParseAnywhere then
// treats as positionals. For example:
//
//	sync name --interval 5s        → positionals=["name"]
//	sync --interval 5s name        → positionals=["name"]  (flags after pos)
//	sync name -- --unknown         → positionals=["name","--unknown"]
func ParseAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positionals = append(positionals, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return positionals, nil
}
