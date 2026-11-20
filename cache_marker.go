package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// writeCacheMarker sets the marker's mtime without ever opening it for writing.
// An existing path must be a regular file: a symlink, directory or device is
// refused so a torrent's own file (or a link planted in the cache) is never
// truncated or redirected.
func writeCacheMarker(path string, used time.Time) error {
	fi, err := os.Lstat(path)
	if err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s: not a regular file (%s)", path, fi.Mode().Type())
		}
		return os.Chtimes(path, used, used)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chtimes(path, used, used)
}
