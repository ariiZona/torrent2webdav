package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// lifetime totals survive cache eviction and process restarts. They live next
// to the TTL marker, outside the payload directory that eviction clears.
const totalsFile = ".totals.json"

type persistedTotals struct {
	Downloaded int64 `json:"downloaded"`
	Uploaded   int64 `json:"uploaded"`
	WebDAVSent int64 `json:"webdavSent"`
}

func readTotals(dir string) (persistedTotals, bool) {
	b, err := os.ReadFile(filepath.Join(dir, totalsFile))
	if err != nil {
		return persistedTotals{}, false
	}
	var v persistedTotals
	if err := json.Unmarshal(b, &v); err != nil {
		log.Printf("totals: ignoring %s: %v", totalsFile, err)
		return persistedTotals{}, false
	}
	return v, true
}

// writeTotals replaces the file atomically so a crash cannot leave it half
// written.
func writeTotals(dir string, v persistedTotals) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, totalsFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, totalsFile))
}

func (s *stream) persistTotals() {
	down, up, sent := s.totalDown.Load(), s.totalUp.Load(), s.sent.Load()
	s.mu.Lock()
	t := s.t
	s.mu.Unlock()
	if t != nil {
		stats := t.Stats()
		down += stats.BytesReadData.Int64()
		up += stats.BytesWrittenData.Int64()
	}
	if err := writeTotals(s.cacheDir, persistedTotals{Downloaded: down, Uploaded: up, WebDAVSent: sent}); err != nil {
		log.Printf("totals: %v", err)
	}
}
