package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

const (
	// The owned child of CACHE_DIR. Nothing outside it is ever cleared, so
	// legacy downloads and unrelated siblings are left alone.
	managedCacheDirName = ".torrent2webdav"
	// Proof of ownership, written only when we create the directory. A missing
	// or different marker never counts as ours.
	cacheOwnerMarker = ".owner"
	cacheOwnerValue  = "torrent2webdav cache v1\n"
	payloadDirName   = "payload"
)

// prepareManagedCache creates or adopts the private cache directory under the
// configured root and returns its path. An existing directory is only adopted
// when it carries our ownership marker; anything else (including a symlink) is
// left untouched and a numbered sibling is used instead.
func prepareManagedCache(root string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("cache root: %w", err)
	}
	for i := 0; i < 100; i++ {
		name := managedCacheDirName
		if i > 0 {
			name = fmt.Sprintf("%s-%d", managedCacheDirName, i)
		}
		dir := filepath.Join(root, name)
		owned, err := ensureOwnedCacheDir(dir)
		if err != nil {
			return "", err
		}
		if !owned {
			log.Printf("cache: leaving unowned %s alone", dir)
			continue
		}
		if err := ensureRealDir(filepath.Join(dir, payloadDirName)); err != nil {
			return "", fmt.Errorf("cache payload: %w", err)
		}
		return dir, nil
	}
	return "", fmt.Errorf("cache: no free managed directory under %s", root)
}

// ensureOwnedCacheDir reports whether dir is ours, creating it with an owner
// marker if it does not exist yet.
func ensureOwnedCacheDir(dir string) (bool, error) {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(dir, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return false, nil
			}
			return false, err
		}
		if err := writeCacheOwner(dir); err != nil {
			_ = os.Remove(dir)
			return false, err
		}
		return true, nil
	case err != nil:
		return false, err
	case !fi.IsDir():
		return false, nil
	}
	return hasCacheOwner(dir), nil
}

func writeCacheOwner(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, cacheOwnerMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(cacheOwnerValue); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func hasCacheOwner(dir string) bool {
	path := filepath.Join(dir, cacheOwnerMarker)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(path)
	return err == nil && string(b) == cacheOwnerValue
}

// ensureRealDir makes sure path is a real directory. A file or symlink at path
// is removed (only the link itself) rather than followed.
func ensureRealDir(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.Mkdir(path, 0o755)
	case err != nil:
		return err
	case fi.IsDir():
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return os.Mkdir(path, 0o755)
}

// wipeManagedPayload clears only the payload child, after confirming the
// managed directory is still ours. The managed directory itself (owner marker,
// last-used marker) and the configured root are never removed.
func wipeManagedPayload(managedDir string) error {
	if !hasCacheOwner(managedDir) {
		return fmt.Errorf("cache: %s is not owned; refusing to clear it", managedDir)
	}
	fi, err := os.Lstat(managedDir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("cache: %s is not a directory; refusing to clear it", managedDir)
	}
	// shortcut: ownership check and RemoveAll are not atomic, so a local
	// attacker who can replace managedDir in between can redirect the delete.
	// Upgrade to openat-style traversal if CACHE_DIR becomes shared/untrusted.
	payload := filepath.Join(managedDir, payloadDirName)
	if err := os.RemoveAll(payload); err != nil {
		return err
	}
	return ensureRealDir(payload)
}
