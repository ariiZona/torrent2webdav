package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareManagedCacheOwnedLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	managed, err := prepareManagedCache(root)
	must(t, err)

	if want := filepath.Join(root, managedCacheDirName); managed != want {
		t.Fatalf("managed dir = %q, want %q", managed, want)
	}
	for _, dir := range []string{managed, filepath.Join(managed, payloadDirName)} {
		fi, err := os.Stat(dir)
		must(t, err)
		if !fi.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}
	b, err := os.ReadFile(filepath.Join(managed, cacheOwnerMarker))
	must(t, err)
	if string(b) != cacheOwnerValue {
		t.Fatalf("owner marker = %q, want %q", b, cacheOwnerValue)
	}
}

func TestPrepareManagedCacheReusesOwned(t *testing.T) {
	root := t.TempDir()
	first, err := prepareManagedCache(root)
	must(t, err)
	keep := filepath.Join(first, payloadDirName, "keep")
	must(t, os.WriteFile(keep, []byte("x"), 0o644))

	second, err := prepareManagedCache(root)
	must(t, err)
	if second != first {
		t.Fatalf("managed dir changed across starts: %q then %q", first, second)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("existing payload not reused: %v", err)
	}
}

func TestPrepareManagedCacheLeavesUnownedDir(t *testing.T) {
	root := t.TempDir()
	unowned := filepath.Join(root, managedCacheDirName)
	must(t, os.Mkdir(unowned, 0o755))
	keep := filepath.Join(unowned, "keep")
	must(t, os.WriteFile(keep, []byte("legacy"), 0o644))

	managed, err := prepareManagedCache(root)
	must(t, err)
	if managed == unowned {
		t.Fatal("adopted an unowned directory")
	}
	if want := filepath.Join(root, managedCacheDirName+"-1"); managed != want {
		t.Fatalf("managed dir = %q, want %q", managed, want)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unowned data touched: %v", err)
	}
}

func TestPrepareManagedCacheSkipsSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	must(t, os.Mkdir(target, 0o755))
	keep := filepath.Join(target, "keep")
	must(t, os.WriteFile(keep, []byte("outside"), 0o644))

	link := filepath.Join(root, managedCacheDirName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	managed, err := prepareManagedCache(root)
	must(t, err)
	if managed == link {
		t.Fatal("adopted a symlink")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("symlink target touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, cacheOwnerMarker)); !os.IsNotExist(err) {
		t.Fatalf("owner marker written through symlink: %v", err)
	}
}

// Eviction removes only the payload: the configured root, unrelated siblings,
// the owner marker and the last-used marker all survive.
func TestWipeManagedPayloadKeepsRootAndMarkers(t *testing.T) {
	root := t.TempDir()
	managed, err := prepareManagedCache(root)
	must(t, err)
	payload := filepath.Join(managed, payloadDirName)

	legacy := filepath.Join(root, "old-torrent")
	must(t, os.Mkdir(legacy, 0o755))
	legacyFile := filepath.Join(legacy, "data")
	must(t, os.WriteFile(legacyFile, []byte("legacy"), 0o644))
	lastUsed := filepath.Join(managed, cacheMarker)
	must(t, os.WriteFile(lastUsed, nil, 0o644))
	must(t, os.WriteFile(filepath.Join(payload, "data"), []byte("cached"), 0o644))

	must(t, wipeManagedPayload(managed))

	entries, err := os.ReadDir(payload)
	must(t, err)
	if len(entries) != 0 {
		t.Fatalf("payload not cleared: %v", entries)
	}
	for _, path := range []string{legacyFile, lastUsed, filepath.Join(managed, cacheOwnerMarker)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed by eviction: %v", path, err)
		}
	}
}

func TestWipeManagedPayloadRefusesUnownedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-ours")
	must(t, os.Mkdir(dir, 0o755))
	keep := filepath.Join(dir, "keep")
	must(t, os.WriteFile(keep, []byte("x"), 0o644))

	if err := wipeManagedPayload(dir); err == nil {
		t.Fatal("cleared a directory without an owner marker")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unowned data removed: %v", err)
	}
}

func TestWipeManagedPayloadDoesNotFollowSymlink(t *testing.T) {
	root := t.TempDir()
	managed, err := prepareManagedCache(root)
	must(t, err)
	payload := filepath.Join(managed, payloadDirName)

	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	must(t, os.WriteFile(keep, []byte("outside"), 0o644))
	must(t, os.RemoveAll(payload))
	if err := os.Symlink(outside, payload); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	must(t, wipeManagedPayload(managed))

	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("symlink target removed: %v", err)
	}
	fi, err := os.Lstat(payload)
	must(t, err)
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("payload is %v, want a real directory", fi.Mode())
	}
}
