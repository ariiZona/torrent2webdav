package main

import (
	"bytes"
	"path/filepath"
	"testing"
)

// Torrents that differ only in content must not share a payload directory.
func TestPayloadDirKeyedByInfoHash(t *testing.T) {
	miA, _, _, _ := makeTorrent(t)
	miB, _, _, _ := makeTorrent(t)

	infoA, err := miA.UnmarshalInfo()
	must(t, err)
	infoB, err := miB.UnmarshalInfo()
	must(t, err)
	if infoA.Name != infoB.Name || infoA.TotalLength() != infoB.TotalLength() {
		t.Fatalf("torrents are not same-name same-length: %q/%d vs %q/%d",
			infoA.Name, infoA.TotalLength(), infoB.Name, infoB.TotalLength())
	}
	if bytes.Equal(miA.InfoBytes, miB.InfoBytes) {
		t.Fatal("test torrents have identical info bytes")
	}

	s := &stream{cacheDir: t.TempDir(), meta: miA}
	dirA := s.payloadDir()
	wantA := filepath.Join(s.cacheDir, "payload", miA.HashInfoBytes().HexString())
	if dirA != wantA {
		t.Fatalf("payloadDir = %q, want %q", dirA, wantA)
	}

	s.meta = miB
	if dirB := s.payloadDir(); dirB == dirA {
		t.Fatalf("same-name same-length torrents share payload dir %q", dirA)
	}

	s.meta = miA
	if again := s.payloadDir(); again != dirA {
		t.Fatalf("payloadDir not stable: %q then %q", dirA, again)
	}
}
