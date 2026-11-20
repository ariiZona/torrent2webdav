package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteCacheMarker(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, cacheMarker)
	used := time.Unix(1_700_000_000, 0)

	check := func() {
		t.Helper()
		fi, err := os.Lstat(marker)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.Mode().IsRegular() {
			t.Fatalf("marker mode = %v, want a regular file", fi.Mode())
		}
		if d := fi.ModTime().Sub(used); d < -time.Second || d > time.Second {
			t.Fatalf("marker mtime = %v, want %v", fi.ModTime(), used)
		}
	}

	if err := writeCacheMarker(marker, used); err != nil {
		t.Fatalf("create: %v", err)
	}
	check()

	// Updating must keep the existing content: the marker is never truncated.
	content := []byte("keep this")
	if err := os.WriteFile(marker, content, 0o644); err != nil {
		t.Fatal(err)
	}
	used = used.Add(time.Minute)
	if err := writeCacheMarker(marker, used); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("marker content = %q, want %q", got, content)
	}
	check()

	// A symlink is refused and its target is left untouched.
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, content, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Logf("symlinks unavailable: %v", err)
	} else if err := writeCacheMarker(link, used); err == nil {
		t.Fatal("symlink accepted as marker")
	}
	got, err = os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("symlink target content = %q, want %q", got, content)
	}

	// Other non-regular files are refused as well.
	bad := filepath.Join(dir, "dir")
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeCacheMarker(bad, used); err == nil {
		t.Fatal("directory accepted as marker")
	}
}
