package main

import "path/filepath"

// payloadDir is the managed directory holding this torrent's downloaded files.
// Keying it by info hash keeps same-named torrents apart.
func (s *stream) payloadDir() string {
	return filepath.Join(s.cacheDir, payloadDirName, s.meta.HashInfoBytes().HexString())
}
