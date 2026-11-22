package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Totals survive a restart and cache eviction, which only clears the payload.
func TestTotalsPersistAcrossRestarts(t *testing.T) {
	_, torrentPath, _, _ := makeTorrent(t)
	dir := t.TempDir()

	s := newStream(torrentPath, dir, 0, 0, 0, 0, 0)
	s.totalDown.Store(1234)
	s.totalUp.Store(5678)
	s.sent.Store(9012)
	s.Close()

	if err := wipeManagedPayload(s.cacheDir); err != nil {
		t.Fatal(err)
	}

	s2 := newStream(torrentPath, dir, 0, 0, 0, 0, 0)
	t.Cleanup(s2.Close)
	if got := s2.totalDown.Load(); got != 1234 {
		t.Errorf("downloaded = %d, want 1234", got)
	}
	if got := s2.totalUp.Load(); got != 5678 {
		t.Errorf("uploaded = %d, want 5678", got)
	}
	if got := s2.sent.Load(); got != 9012 {
		t.Errorf("WebDAV sent = %d, want 9012", got)
	}
}

func TestReadTotalsIgnoresGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, totalsFile)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readTotals(dir); ok {
		t.Error("corrupt totals file was accepted")
	}
}
