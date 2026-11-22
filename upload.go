package main

import (
	"sort"
	"sync"

	"github.com/anacrolix/torrent"
)

// uploadCounter attributes peer uploads to files. The patched torrent library
// reports the torrent offset and length of every piece chunk queued for a peer,
// so these totals are exact.
type uploadCounter struct {
	mu    sync.Mutex
	spans []uploadSpan
	read  map[string]int64
}

type uploadSpan struct {
	name       string
	start, end int64
}

func newUploadCounter() *uploadCounter {
	return &uploadCounter{read: map[string]int64{}}
}

// reset points the counter at a new client's file layout and starts over.
func (c *uploadCounter) reset(files []*torrent.File) {
	spans := make([]uploadSpan, 0, len(files))
	for _, f := range files {
		spans = append(spans, uploadSpan{f.DisplayPath(), f.Offset(), f.Offset() + f.Length()})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	c.mu.Lock()
	c.spans, c.read = spans, map[string]int64{}
	c.mu.Unlock()
}

func (c *uploadCounter) add(_ *torrent.Torrent, off, n int64) {
	if n <= 0 {
		return
	}
	end := off + n
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.spans {
		if s.end <= off {
			continue
		}
		if s.start >= end {
			break
		}
		c.read[s.name] += min(end, s.end) - max(off, s.start)
	}
}

func (c *uploadCounter) uploaded(name string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read[name]
}
