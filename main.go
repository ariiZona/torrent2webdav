// Command torrent2webdav serves the files of one torrent over read-only WebDAV,
// downloading pieces on demand from the swarm.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/net/webdav"
)

const (
	// How much data to prioritize ahead of the current read position. Big enough
	// for rclone's sequential reads, small enough not to fetch the whole torrent.
	readaheadBytes = 32 << 20
	// Cache eviction only runs after this much time without a read, so a paused
	// player or a gap between episodes doesn't throw the cache away.
	evictIdle = time.Minute
	// While idle, refresh the torrent's DHT entry so the next read starts from a
	// known peer pool instead of bootstrapping again.
	warmEvery = 15 * time.Minute
	// Records when cached data was last read, so CACHE_TTL also works across restarts.
	cacheMarker = ".last-used"
)

func main() {
	torrentFile := flag.String("torrent", env("TORRENT_FILE", ""), "path to the .torrent file (required)")
	webdavAddr := flag.String("webdav-addr", env("WEBDAV_ADDR", ":8080"), "WebDAV listen address")
	portText := flag.String("torrent-port", resourceEnv("TORRENT_PORT", "42069"), "BitTorrent TCP/UDP listen port")
	cacheDir := flag.String("cache-dir", env("CACHE_DIR", "./cache"), "directory for downloaded pieces")
	cacheMax := flag.String("cache-max-size", resourceEnv("CACHE_MAX_SIZE", "0"), "clear the cache above this size while idle (e.g. 50G; 0 = no limit)")
	ttlText := flag.String("cache-ttl", resourceEnv("CACHE_TTL", "0"), "clear the cache after this much idle time (e.g. 24h; 0 = never)")
	user := flag.String("user", env("WEBDAV_USER", ""), "optional WebDAV basic-auth username")
	pass := flag.String("pass", env("WEBDAV_PASS", ""), "optional WebDAV basic-auth password")
	flag.Parse()

	cacheTTL, err := parseDuration(*ttlText)
	if err != nil {
		log.Fatalf("cache-ttl: %v", err)
	}

	if *torrentFile == "" {
		log.Fatal("no torrent: set TORRENT_FILE or -torrent")
	}
	// Checked before newStream: a half-configured pair must not reach the code
	// that may wipe the cache.
	if err := validateAuth(*user, *pass); err != nil {
		log.Fatalf("auth: %v", err)
	}
	torrentPort, err := strconv.Atoi(*portText)
	if err != nil {
		log.Fatalf("torrent-port: %v", err)
	}
	if err := checkPort(torrentPort); err != nil {
		log.Fatalf("torrent-port: %v", err)
	}
	maxBytes, err := parseSize(*cacheMax)
	if err != nil {
		log.Fatalf("cache-max-size: %v", err)
	}

	s := newStream(*torrentFile, *cacheDir, torrentPort, maxBytes, cacheTTL)
	defer s.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	check := startSelfTest(ctx, torrentPort)
	dav := methodWrapper(&webdav.Handler{
		Prefix:     "/",
		FileSystem: s,
		LockSystem: webdav.NewMemLS(),
	})
	status := newStatusHandler(s, check)
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStatusPath(r.URL.Path) {
			status.ServeHTTP(w, r)
			return
		}
		dav.ServeHTTP(w, r)
	})
	if *user != "" {
		h = withBasicAuth(*user, *pass, h)
	}

	l := newHTTPLifecycle(h, shutdownGrace)

	log.Printf("webdav on %s (status at /_status)", *webdavAddr)
	if err := l.serve(ctx, *webdavAddr); err != nil {
		log.Fatal(err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func resourceEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// parseDuration rejects negative values and positive values that ParseDuration
// truncates to zero (anything under a nanosecond).
func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}
	if d == 0 && strings.ContainsAny(s, "123456789") {
		return 0, fmt.Errorf("duration %q is less than one nanosecond", s)
	}
	return d, nil
}

// checkPort rejects ports outside the TCP/UDP range. Port 0 is rejected too:
// it would silently pick a random port instead of the forwarded one configured.
func checkPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %d (want 1-65535)", port)
	}
	return nil
}

// parseSize parses "50G", "1.5GB", "20GiB", "500M" or a plain byte count.
// Sizes are binary (G = GiB).
func parseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "0" {
		return 0, nil
	}
	mult := int64(1)
	for _, suffix := range []struct {
		text string
		mult int64
	}{
		{"TIB", 1 << 40}, {"TB", 1 << 40}, {"T", 1 << 40},
		{"GIB", 1 << 30}, {"GB", 1 << 30}, {"G", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"M", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10}, {"K", 1 << 10},
		{"B", 1},
	} {
		if strings.HasSuffix(t, suffix.text) {
			mult = suffix.mult
			t = strings.TrimSpace(strings.TrimSuffix(t, suffix.text))
			break
		}
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n >= float64(math.MaxInt64)/float64(mult) {
		return 0, fmt.Errorf("size %q overflows int64", s)
	}
	if n == 0 && strings.ContainsAny(t, "123456789") {
		// ParseFloat underflows very small positive values to zero.
		return 0, fmt.Errorf("size %q is less than one byte", s)
	}
	if n > 0 && n*float64(mult) < 1 {
		return 0, fmt.Errorf("size %q is less than one byte", s)
	}
	return int64(n * float64(mult)), nil
}

// validateAuth rejects a half-configured basic-auth pair. A lone user or
// password is always a mistake: it would either serve unprotected or reject
// every request, so refuse to start instead.
func validateAuth(user, pass string) error {
	if (user == "") != (pass == "") {
		return errors.New("user and pass must be set together: set both WEBDAV_USER/-user and WEBDAV_PASS/-pass, or neither")
	}
	return nil
}

func withBasicAuth(user, pass string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="torrent2webdav"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// stream owns the torrent client, its cache, and the current WebDAV share, and
// can reset all of them to enforce cache limits.
type stream struct {
	meta     *metainfo.MetaInfo // original metainfo, for a stable creation date across reopens
	cacheDir string             // private managed directory under -cache-dir; safe to clear
	port     int
	maxBytes int64
	ttl      time.Duration

	mu       sync.Mutex
	cond     *sync.Cond
	active   int            // open WebDAV files and status snapshots; eviction waits for zero
	files    map[string]int // open WebDAV file names, for the status page
	wiping   bool
	lastUsed time.Time

	cl    *torrent.Client
	t     *torrent.Torrent
	share *share

	done      chan struct{}
	loopDone  chan struct{}
	warmStop  chan struct{}
	warmDone  chan struct{}
	closeOnce sync.Once
}

func newStream(torrentFile, cacheDir string, port int, maxBytes int64, ttl time.Duration) *stream {
	mi, err := metainfo.LoadFromFile(torrentFile)
	if err != nil {
		log.Fatalf("load torrent: %v", err)
	}
	managedDir, err := prepareManagedCache(cacheDir)
	if err != nil {
		log.Fatal(err)
	}
	s := &stream{
		meta:     mi,
		cacheDir: managedDir,
		port:     port,
		maxBytes: maxBytes,
		ttl:      ttl,
		files:    map[string]int{},
		lastUsed: time.Now(),
		done:     make(chan struct{}),
		loopDone: make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)

	// Keep the TTL clock across restarts instead of granting a full new TTL.
	if used, ok := cacheMarkerTime(s.cacheDir); ok {
		s.lastUsed = used
	}

	if ttl > 0 && cacheIdleExpired(s.cacheDir, ttl) {
		log.Printf("cache idle for over %s, clearing %s", ttl, filepath.Join(s.cacheDir, payloadDirName))
		if err := wipeManagedPayload(s.cacheDir); err != nil {
			log.Printf("clear cache: %v", err)
		}
	}
	s.open()
	go s.cleanLoop()
	log.Printf("serving %q: %d file(s), %.1f GiB, %d pieces of %.1f MiB, torrent port %d",
		s.t.Name(), len(s.t.Files()), float64(s.t.Length())/(1<<30),
		s.t.NumPieces(), float64(s.t.Info().PieceLength)/(1<<20), port)
	return s
}

// open starts a fresh client/torrent/share against cacheDir. It is only called
// before serving or from cleanLoop while no files are open.
func (s *stream) open() {
	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = s.port
	cfg.Seed = false
	// DHT and PEX are enabled by NewDefaultClientConfig. Accept peers even while
	// nothing is being read, so an idle share keeps its connections for the next
	// play instead of only accepting peers once data is wanted.
	cfg.AlwaysWantConns = true
	cfg.DataDir = s.payloadDir()
	if err := ensureRealDir(cfg.DataDir); err != nil {
		log.Fatalf("cache payload: %v", err)
	}
	cfg.Slogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cl, err := torrent.NewClient(cfg) // owns and closes the storage in DataDir
	if err != nil {
		log.Fatalf("torrent client: %v", err)
	}
	t, err := cl.AddTorrent(s.meta)
	if err != nil {
		cl.Close()
		log.Fatalf("add torrent: %v", err)
	}
	<-t.GotInfo()
	s.cl, s.t, s.share = cl, t, newShare(t, s.meta.CreationDate)
	s.warmStop = make(chan struct{})
	s.warmDone = make(chan struct{})
	go func() {
		defer close(s.warmDone)
		s.keepWarm(s.warmStop, cl, t)
	}()
}

func (s *stream) close() {
	if s.warmStop != nil {
		close(s.warmStop)
		<-s.warmDone
		s.warmStop, s.warmDone = nil, nil
	}
	if s.cl != nil {
		s.t.Drop()   // Finish pending piece checkpoints before the client closes their database.
		s.cl.Close() // flushes the piece-completion database and closes storage
	}
	s.cl, s.t, s.share = nil, nil, nil
}

// keepWarm refreshes the DHT entry while the share is idle. The library's own
// announcer parks until data is wanted, so peers would otherwise forget us
// between plays.
func (s *stream) keepWarm(stop <-chan struct{}, cl *torrent.Client, t *torrent.Torrent) {
	if !waitOrStop(stop, evictIdle) {
		return
	}
	for {
		if s.idle() {
			for _, srv := range cl.DhtServers() {
				annDone, annStop, err := t.AnnounceToDht(srv)
				if err != nil {
					continue
				}
				select {
				case <-stop:
					annStop()
					return
				case <-annDone:
				case <-time.After(warmEvery):
					annStop()
				}
			}
		}
		if !waitOrStop(stop, warmEvery) {
			return
		}
	}
}

func (s *stream) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active == 0 && !s.wiping
}

func waitOrStop(stop <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

func (s *stream) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		<-s.loopDone
		s.persistLastUsed()
		s.close()
	})
}

// cleanLoop enforces the cache limits about once a minute, only while idle.
func (s *stream) cleanLoop() {
	defer close(s.loopDone)
	if s.maxBytes <= 0 && s.ttl <= 0 {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.persistLastUsed()
			s.evictIfNeeded()
		}
	}
}

func (s *stream) evictIfNeeded() {
	s.mu.Lock()
	if s.wiping || s.active != 0 || time.Since(s.lastUsed) < evictIdle {
		s.mu.Unlock()
		return
	}
	var reason string
	size := s.t.BytesCompleted()
	switch {
	case s.maxBytes > 0 && size > s.maxBytes:
		reason = fmt.Sprintf("%d bytes cached, limit %d", size, s.maxBytes)
	case s.ttl > 0 && time.Since(s.lastUsed) > s.ttl:
		if size == 0 {
			// Nothing cached: rebuilding the session would only reset the
			// TTL clock without a read.
			s.mu.Unlock()
			return
		}
		reason = "idle for " + s.ttl.String()
	default:
		s.mu.Unlock()
		return
	}
	s.wiping = true
	s.mu.Unlock()

	log.Printf("clearing cache: %s", reason)
	s.close()
	if err := wipeManagedPayload(s.cacheDir); err != nil {
		log.Printf("clear cache: %v", err)
	}
	s.open()

	s.mu.Lock()
	s.lastUsed = time.Now()
	s.wiping = false
	s.cond.Broadcast()
	s.mu.Unlock()
}

// persistLastUsed records the last read time so CACHE_TTL survives restarts.
func (s *stream) persistLastUsed() {
	if s.ttl <= 0 {
		return
	}
	s.mu.Lock()
	used := s.lastUsed
	s.mu.Unlock()
	if err := writeCacheMarker(filepath.Join(s.cacheDir, cacheMarker), used); err != nil {
		log.Printf("cache marker: %v", err)
	}
}

// cacheIdleExpired reports whether a valid marker records more than ttl of
// idleness. A missing or non-regular marker never expires: the payload may come
// from an older version, and guessing would delete data we cannot identify.
func cacheIdleExpired(cacheDir string, ttl time.Duration) bool {
	used, ok := cacheMarkerTime(cacheDir)
	return ok && time.Since(used) > ttl
}

// cacheMarkerTime returns the last-use time recorded by a regular marker file.
func cacheMarkerTime(cacheDir string) (time.Time, bool) {
	fi, err := os.Lstat(filepath.Join(cacheDir, cacheMarker))
	if err != nil || !fi.Mode().IsRegular() {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

func (s *stream) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *stream) current() *share {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.wiping {
		s.cond.Wait()
	}
	return s.share
}

func (s *stream) Mkdir(ctx context.Context, name string, perm fs.FileMode) error {
	return s.current().Mkdir(ctx, name, perm)
}

func (s *stream) RemoveAll(ctx context.Context, name string) error {
	return s.current().RemoveAll(ctx, name)
}

func (s *stream) Rename(ctx context.Context, oldName, newName string) error {
	return s.current().Rename(ctx, oldName, newName)
}

func (s *stream) Stat(ctx context.Context, name string) (fs.FileInfo, error) {
	return s.current().Stat(ctx, name)
}

func (s *stream) OpenFile(ctx context.Context, name string, flag int, perm fs.FileMode) (webdav.File, error) {
	s.mu.Lock()
	for s.wiping {
		s.cond.Wait()
	}
	sh := s.share
	s.active++
	s.mu.Unlock()

	f, err := sh.OpenFile(ctx, name, flag, perm)
	if err != nil {
		s.release()
		return nil, err
	}
	// Directories are opened for listing too; only files are "loaded".
	tracked := ""
	if e := sh.lookup(name); e != nil && !e.dir {
		tracked = name
		s.mu.Lock()
		if s.files == nil {
			s.files = map[string]int{}
		}
		s.files[tracked]++
		s.mu.Unlock()
	}
	return &trackedFile{File: f, s: s, name: tracked}, nil
}

// hold pins the current client and torrent for a status snapshot, so a cache
// eviction cannot swap them mid-read.
func (s *stream) hold() (*torrent.Client, *torrent.Torrent) {
	s.mu.Lock()
	for s.wiping {
		s.cond.Wait()
	}
	s.active++
	cl, t := s.cl, s.t
	s.mu.Unlock()
	return cl, t
}

func (s *stream) release() { s.releaseFile("") }

// releaseFile drops one held file or snapshot and wakes a waiting eviction.
func (s *stream) releaseFile(name string) {
	s.mu.Lock()
	if name != "" {
		if s.files[name] <= 1 {
			delete(s.files, name)
		} else {
			s.files[name]--
		}
	}
	s.active--
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *stream) fileList() []statusFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]statusFile, 0, len(s.files))
	for name, readers := range s.files {
		f := statusFile{Name: name, Readers: readers}
		if s.share != nil {
			if e := s.share.lookup(name); e != nil && e.file != nil {
				f.Size, f.Cached = e.size, e.file.BytesCompleted()
			}
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// cachedFileList reports every file with data on disk, including files no
// client has open.
func (s *stream) cachedFileList() []statusFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []statusFile{}
	if s.share == nil {
		return out
	}
	s.share.walkFiles(func(name string, e *entry) {
		if cached := e.file.BytesCompleted(); cached > 0 {
			out = append(out, statusFile{Name: name, Size: e.size, Cached: cached})
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type trackedFile struct {
	webdav.File
	s    *stream
	name string
	once sync.Once
}

func (f *trackedFile) Read(p []byte) (int, error) {
	f.s.touch()
	return f.File.Read(p)
}

func (f *trackedFile) Seek(offset int64, whence int) (int64, error) {
	f.s.touch()
	return f.File.Seek(offset, whence)
}

func (f *trackedFile) Close() error {
	err := f.File.Close()
	f.once.Do(func() { f.s.releaseFile(f.name) })
	return err
}

// share is a read-only in-memory directory tree backed by torrent file readers.
type share struct {
	root *entry
}

func newShare(t *torrent.Torrent, created int64) *share {
	// Stable modtime (torrent creation date, or the epoch) so clients don't
	// treat files as changed on every container restart.
	mod := time.Unix(created, 0)
	root := &entry{name: t.Name(), dir: true, modTime: mod, kids: map[string]*entry{}}
	for _, f := range t.Files() {
		// x/net/webdav sniffs unknown extensions for the getcontenttype property
		// by reading 512 bytes. Registering a type keeps PROPFIND local instead
		// of blocking on the swarm.
		// shortcut: files without an extension still get sniffed; rare in torrents.
		ext := filepath.Ext(f.DisplayPath())
		if ext != "" && mime.TypeByExtension(ext) == "" {
			mime.AddExtensionType(ext, "application/octet-stream")
		}
		parts := strings.Split(f.DisplayPath(), "/")
		cur := root
		for _, p := range parts[:len(parts)-1] {
			next := cur.kids[p]
			if next == nil {
				next = &entry{name: p, dir: true, modTime: mod, kids: map[string]*entry{}}
				cur.kids[p] = next
			}
			cur = next
		}
		last := parts[len(parts)-1]
		cur.kids[last] = &entry{name: last, size: f.Length(), modTime: mod, file: f}
	}
	return &share{root: root}
}

type entry struct {
	name    string
	size    int64
	modTime time.Time
	dir     bool
	file    *torrent.File
	kids    map[string]*entry
}

func (e *entry) Name() string       { return e.name }
func (e *entry) Size() int64        { return e.size }
func (e *entry) ModTime() time.Time { return e.modTime }
func (e *entry) IsDir() bool        { return e.dir }
func (e *entry) Sys() any           { return nil }
func (e *entry) Mode() fs.FileMode {
	if e.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func (s *share) lookup(name string) *entry {
	cur := s.root
	for _, p := range strings.Split(name, "/") {
		if p == "" {
			continue
		}
		cur = cur.kids[p]
		if cur == nil {
			return nil
		}
	}
	return cur
}

// walkFiles calls f for every file with its share-relative path.
func (s *share) walkFiles(f func(name string, e *entry)) {
	var walk func(prefix string, e *entry)
	walk = func(prefix string, e *entry) {
		for name, kid := range e.kids {
			p := name
			if prefix != "" {
				p = prefix + "/" + name
			}
			if kid.dir {
				walk(p, kid)
			} else {
				f(p, kid)
			}
		}
	}
	walk("", s.root)
}

func (s *share) Mkdir(context.Context, string, fs.FileMode) error { return fs.ErrPermission }
func (s *share) RemoveAll(context.Context, string) error          { return fs.ErrPermission }
func (s *share) Rename(context.Context, string, string) error     { return fs.ErrPermission }

func (s *share) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	e := s.lookup(name)
	if e == nil {
		return nil, fs.ErrNotExist
	}
	return e, nil
}

func (s *share) OpenFile(ctx context.Context, name string, flag int, _ fs.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, fs.ErrPermission
	}
	e := s.lookup(name)
	if e == nil {
		return nil, fs.ErrNotExist
	}
	return &openEntry{e: e, ctx: ctx}, nil
}

// openEntry lazily creates its torrent reader on first read or seek, so PROPFIND
// and stat-only requests never touch the swarm.
type openEntry struct {
	e   *entry
	ctx context.Context
	r   torrent.Reader
	off int // Readdir position
}

func (o *openEntry) reader() torrent.Reader {
	if o.r == nil {
		o.r = o.e.file.NewReader()
		o.r.SetReadahead(readaheadBytes)
		o.r.SetContext(o.ctx) // abort blocked reads when the client disconnects
	}
	return o.r
}

func (o *openEntry) Read(p []byte) (int, error) {
	if o.e.dir {
		return 0, fs.ErrInvalid
	}
	return o.reader().Read(p)
}

func (o *openEntry) Seek(offset int64, whence int) (int64, error) {
	if o.e.dir {
		return 0, fs.ErrInvalid
	}
	return o.reader().Seek(offset, whence)
}

func (o *openEntry) Write([]byte) (int, error)  { return 0, fs.ErrPermission }
func (o *openEntry) Stat() (fs.FileInfo, error) { return o.e, nil }

func (o *openEntry) Close() error {
	if o.r != nil {
		return o.r.Close()
	}
	return nil
}

func (o *openEntry) Readdir(n int) ([]fs.FileInfo, error) {
	if !o.e.dir {
		return nil, fs.ErrInvalid
	}
	names := make([]string, 0, len(o.e.kids))
	for name := range o.e.kids {
		names = append(names, name)
	}
	sort.Strings(names)
	if n <= 0 {
		n = len(names)
	}
	out := make([]fs.FileInfo, 0, n)
	for o.off < len(names) && len(out) < n {
		out = append(out, o.e.kids[names[o.off]])
		o.off++
	}
	if len(out) == 0 {
		return nil, io.EOF
	}
	return out, nil
}
