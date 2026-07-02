package cliargs_test

import (
	"flag"
	"testing"

	"caravan/internal/cliargs"
)

// makeFS returns a fresh FlagSet with a string flag and a bool flag.
func makeFS(t *testing.T) (fs *flag.FlagSet, str *string, b *bool) {
	t.Helper()
	fs = flag.NewFlagSet("test", flag.ContinueOnError)
	str = fs.String("str", "default", "string flag")
	b = fs.Bool("bool", false, "bool flag")
	return
}

func TestParseAnywhere_FlagsBeforePositionals(t *testing.T) {
	fs, str, _ := makeFS(t)
	pos, err := cliargs.ParseAnywhere(fs, []string{"--str", "hello", "pos1", "pos2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *str != "hello" {
		t.Errorf("str = %q, want hello", *str)
	}
	if len(pos) != 2 || pos[0] != "pos1" || pos[1] != "pos2" {
		t.Errorf("positionals = %v, want [pos1 pos2]", pos)
	}
}

func TestParseAnywhere_FlagsAfterPositionals(t *testing.T) {
	fs, str, _ := makeFS(t)
	pos, err := cliargs.ParseAnywhere(fs, []string{"pos1", "--str", "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *str != "hello" {
		t.Errorf("str = %q, want hello", *str)
	}
	if len(pos) != 1 || pos[0] != "pos1" {
		t.Errorf("positionals = %v, want [pos1]", pos)
	}
}

func TestParseAnywhere_Interleaved(t *testing.T) {
	fs, str, b := makeFS(t)
	pos, err := cliargs.ParseAnywhere(fs, []string{"pos1", "--str", "hello", "pos2", "--bool"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *str != "hello" {
		t.Errorf("str = %q, want hello", *str)
	}
	if !*b {
		t.Error("bool should be true")
	}
	if len(pos) != 2 || pos[0] != "pos1" || pos[1] != "pos2" {
		t.Errorf("positionals = %v, want [pos1 pos2]", pos)
	}
}

func TestParseAnywhere_EqualForm(t *testing.T) {
	fs, str, _ := makeFS(t)
	pos, err := cliargs.ParseAnywhere(fs, []string{"pos1", "--str=world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *str != "world" {
		t.Errorf("str = %q, want world", *str)
	}
	if len(pos) != 1 || pos[0] != "pos1" {
		t.Errorf("positionals = %v, want [pos1]", pos)
	}
}

// TestParseAnywhere_DoubleDash verifies that "--" acts as a flag terminator.
// Everything after "--" is treated as positional even if it looks like a flag,
// because flag.FlagSet.Parse consumes "--" and returns the rest as args.
func TestParseAnywhere_DoubleDash(t *testing.T) {
	fs, str, _ := makeFS(t)
	pos, err := cliargs.ParseAnywhere(fs, []string{"pos1", "--", "--str", "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// --str was after --, so the flag should keep its default.
	if *str != "default" {
		t.Errorf("str = %q, want default (flag after -- is positional)", *str)
	}
	// pos1, --str, and hello all become positionals.
	if len(pos) != 3 || pos[0] != "pos1" || pos[1] != "--str" || pos[2] != "hello" {
		t.Errorf("positionals = %v, want [pos1 --str hello]", pos)
	}
}

func TestParseAnywhere_NoArgs(t *testing.T) {
	fs, str, _ := makeFS(t)
	pos, err := cliargs.ParseAnywhere(fs, []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *str != "default" {
		t.Errorf("str = %q, want default", *str)
	}
	if len(pos) != 0 {
		t.Errorf("positionals = %v, want []", pos)
	}
}

func TestParseAnywhere_OnlyFlags(t *testing.T) {
	fs, str, b := makeFS(t)
	pos, err := cliargs.ParseAnywhere(fs, []string{"--str", "world", "--bool"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *str != "world" {
		t.Errorf("str = %q, want world", *str)
	}
	if !*b {
		t.Error("bool should be true")
	}
	if len(pos) != 0 {
		t.Errorf("positionals = %v, want []", pos)
	}
}

func TestParseAnywhere_UnknownFlag(t *testing.T) {
	fs, _, _ := makeFS(t)
	_, err := cliargs.ParseAnywhere(fs, []string{"--unknown"})
	if err == nil {
		t.Error("expected error for unknown flag, got nil")
	}
}
