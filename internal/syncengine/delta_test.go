package syncengine

import (
	"reflect"
	"testing"
)

// --- partitionBySize tests ---

func TestPartitionBySize_BasicSplit(t *testing.T) {
	entries := map[string]Entry{
		"small.txt":  {Size: 100},
		"medium.txt": {Size: 4 * 1024 * 1024},     // 4 MiB
		"large.txt":  {Size: 10 * 1024 * 1024},    // 10 MiB
		"huge.txt":   {Size: 100 * 1024 * 1024},   // 100 MiB
	}
	threshold := int64(8 * 1024 * 1024) // 8 MiB

	paths := []string{"small.txt", "medium.txt", "large.txt", "huge.txt"}
	small, large := partitionBySize(paths, entries, threshold)

	wantSmall := map[string]bool{"small.txt": true, "medium.txt": true}
	wantLarge := map[string]bool{"large.txt": true, "huge.txt": true}

	for _, p := range small {
		if !wantSmall[p] {
			t.Errorf("unexpected path in small: %s", p)
		}
	}
	for _, p := range large {
		if !wantLarge[p] {
			t.Errorf("unexpected path in large: %s", p)
		}
	}
	if len(small) != 2 {
		t.Errorf("small count: got %d want 2", len(small))
	}
	if len(large) != 2 {
		t.Errorf("large count: got %d want 2", len(large))
	}
}

func TestPartitionBySize_ExactThreshold(t *testing.T) {
	// A file exactly at the threshold should go to large.
	entries := map[string]Entry{
		"exact.txt": {Size: 8 * 1024 * 1024},
	}
	threshold := int64(8 * 1024 * 1024)
	paths := []string{"exact.txt"}
	small, large := partitionBySize(paths, entries, threshold)

	if len(small) != 0 {
		t.Errorf("expected small empty, got %v", small)
	}
	if len(large) != 1 || large[0] != "exact.txt" {
		t.Errorf("expected exact.txt in large, got %v", large)
	}
}

func TestPartitionBySize_MissingEntry_TreatedAsSmall(t *testing.T) {
	// A path not in entries defaults to size 0 → small.
	entries := map[string]Entry{}
	paths := []string{"unknown.txt"}
	small, large := partitionBySize(paths, entries, 1024)

	if len(large) != 0 {
		t.Errorf("expected large empty, got %v", large)
	}
	if len(small) != 1 || small[0] != "unknown.txt" {
		t.Errorf("expected unknown.txt in small, got %v", small)
	}
}

func TestPartitionBySize_AllSmall(t *testing.T) {
	entries := map[string]Entry{
		"a.txt": {Size: 1},
		"b.txt": {Size: 999},
	}
	small, large := partitionBySize([]string{"a.txt", "b.txt"}, entries, 1000)
	if len(large) != 0 {
		t.Errorf("expected no large files, got %v", large)
	}
	if len(small) != 2 {
		t.Errorf("expected 2 small files, got %v", small)
	}
}

func TestPartitionBySize_Empty(t *testing.T) {
	small, large := partitionBySize(nil, nil, 1024)
	if small != nil || large != nil {
		t.Errorf("expected nil slices for empty input, got small=%v large=%v", small, large)
	}
}

// --- rsyncArgs tests ---

func TestRsyncArgs_Push(t *testing.T) {
	got := rsyncArgs(true, "user@host", "/local/path/file.txt", "/remote/root/file.txt")
	want := []string{"-pt", "-e", "ssh -o BatchMode=yes", "/local/path/file.txt", `user@host:'/remote/root/file.txt'`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rsyncArgs push:\n  got  %v\n  want %v", got, want)
	}
}

func TestRsyncArgs_Pull(t *testing.T) {
	got := rsyncArgs(false, "user@host", "/local/path/file.txt", "/remote/root/file.txt")
	want := []string{"-pt", "-e", "ssh -o BatchMode=yes", `user@host:'/remote/root/file.txt'`, "/local/path/file.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rsyncArgs pull:\n  got  %v\n  want %v", got, want)
	}
}

func TestRsyncArgs_HomePath_Push(t *testing.T) {
	// Paths starting with ~/ must use $HOME expansion (double-quotes on remote side).
	got := rsyncArgs(true, "bob@server", "/home/bob/sync/bigfile.bin", "~/sync/bigfile.bin")
	want := []string{"-pt", "-e", "ssh -o BatchMode=yes", "/home/bob/sync/bigfile.bin", `bob@server:"$HOME/sync/bigfile.bin"`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rsyncArgs home push:\n  got  %v\n  want %v", got, want)
	}
}

func TestRsyncArgs_HomePath_Pull(t *testing.T) {
	got := rsyncArgs(false, "bob@server", "/home/bob/sync/bigfile.bin", "~/sync/bigfile.bin")
	want := []string{"-pt", "-e", "ssh -o BatchMode=yes", `bob@server:"$HOME/sync/bigfile.bin"`, "/home/bob/sync/bigfile.bin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rsyncArgs home pull:\n  got  %v\n  want %v", got, want)
	}
}

func TestRsyncArgs_BareHome(t *testing.T) {
	got := rsyncArgs(true, "user@host", "/local/file", "~")
	want := []string{"-pt", "-e", "ssh -o BatchMode=yes", "/local/file", `user@host:"$HOME"`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rsyncArgs bare ~:\n  got  %v\n  want %v", got, want)
	}
}

// --- flattenPath tests ---

func TestFlattenPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"file.txt", "file.txt"},
		{"sub/file.txt", "sub__file.txt"},
		{"a/b/c/deep.bin", "a__b__c__deep.bin"},
		{"toplevel", "toplevel"},
	}
	for _, tt := range tests {
		got := flattenPath(tt.in)
		if got != tt.want {
			t.Errorf("flattenPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
