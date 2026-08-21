package stream

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Files serves a directory of HLS segments and the playlist that indexes them.
//
// This is the shape a Chromecast actually accepts for live video. A single
// endless response -- chunked MP4 or WebM -- is accepted at LOAD and then
// abandoned after a few hundred kilobytes, with no player state ever reported.
// See docs/chromecast.md.
type Files struct {
	// AllowFrom is the only address served, when set. The receiver's address
	// is known before the server starts, and nothing else has any business
	// fetching the user's screen.
	//
	// Not a boundary on its own -- an address is forgeable on a hostile LAN --
	// which is why the unguessable path below exists as well.
	AllowFrom string

	// Log, when set, is called for every request with a one-line summary.
	// Without it a receiver that fetches a playlist and gives up is
	// indistinguishable from one that never asked.
	Log func(string)

	dir      string
	prefix   string
	listener net.Listener
	srv      *http.Server

	mu       sync.Mutex
	served   int64
	requests int
	playlist int
}

// ServeDir starts a server for one directory, reachable only under a path
// that cannot be guessed.
//
// The path matters because of CORS. The receiver is a web page and will not
// play HLS without Access-Control-Allow-Origin, so the stream must be readable
// cross-origin -- which means any page the user's browser happens to load can
// fetch it too, if it can find it. A fixed port and a fixed filename is a few
// hundred fetches to sweep a /24. Sixteen random bytes in the path is not.
//
// The receiver never types this URL; it is handed the whole thing over the
// Cast connection, so the length costs nobody anything.
func ServeDir(bindIP string, port int, dir string) (*Files, error) {
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("securing the stream: %w", err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindIP, port))
	if err != nil {
		return nil, fmt.Errorf("serving the stream on %s:%d: %w", bindIP, port, err)
	}

	f := &Files{dir: dir, prefix: hex.EncodeToString(secret), listener: ln}
	f.srv = &http.Server{
		Handler: http.HandlerFunc(f.handle),
		// A segment fetch is a few hundred kilobytes over a local network.
		// Without a header timeout anything on that network can hold
		// connections open indefinitely by sending a request slowly.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go f.srv.Serve(ln)
	return f, nil
}

// URLFor is the address of one file in the served directory.
func (f *Files) URLFor(name string) string {
	return fmt.Sprintf("http://%s/%s/%s", f.listener.Addr().String(), f.prefix, name)
}

// Close stops serving.
func (f *Files) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return f.srv.Shutdown(ctx)
}

// Stats reports what the receiver has actually fetched: total bytes, total
// requests, and how many times it re-read the playlist.
//
// The playlist count is the one that matters for a live stream. A receiver
// that is following the cast re-reads the playlist every segment; one that
// has given up fetches it once and stops, while still reporting nothing wrong.
func (f *Files) Stats() (bytes int64, requests, playlistReads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.served, f.requests, f.playlist
}

func (f *Files) handle(w http.ResponseWriter, r *http.Request) {
	if !f.permitted(r) {
		// Deliberately indistinguishable from a wrong path: a caller that is
		// not the receiver learns nothing about whether a cast is running.
		http.NotFound(w, r)
		return
	}

	// Only the basename is ever used, so a request cannot walk out of the
	// directory with .. or an absolute path. The server is reachable by
	// anything on the local network, and the directory it is pointed at sits
	// next to the rest of the user's files.
	name := path.Base(path.Clean("/" + r.URL.Path))
	if name == "." || name == "/" {
		http.NotFound(w, r)
		return
	}

	// CORS is not optional here, and its absence is invisible.
	//
	// A Chromecast receiver is a web page. Progressive video -- the plain MP4
	// that works without any of this -- is played by a <video> element, which
	// needs no CORS. HLS is played through Media Source Extensions, where the
	// page FETCHES each playlist and segment, and a response without
	// Access-Control-Allow-Origin is delivered to the browser and then
	// discarded before the page ever sees it.
	//
	// What that looks like from here: the receiver requests the playlist, this
	// server answers 200 with a valid body, the receiver requests it a few
	// more times, then stops -- and never asks for a single segment, and never
	// reports a player state. Nothing in the logs is an error. Measured
	// exactly that way against a Xiaomi stick before these three headers
	// existed.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		f.note("OPTIONS %s -> 204 (preflight)", name)
		return
	}

	f.mu.Lock()
	f.requests++
	if name == PlaylistNameHint {
		f.playlist++
	}
	f.mu.Unlock()

	// A live playlist must never be cached: the receiver re-reads the same URL
	// to discover new segments, and a cached copy freezes the stream at
	// whatever it said the first time.
	w.Header().Set("Cache-Control", "no-store")

	if path.Ext(name) == ".m3u8" {
		f.servePlaylist(w, path.Join(f.dir, name))
		return
	}
	// Go's MIME table has no entry for .ts on many systems, and a receiver
	// handed text/plain for a transport stream will not decode it.
	w.Header().Set("Content-Type", "video/mp2t")

	counted := &countingWriter{ResponseWriter: w, status: 200}
	http.ServeFile(counted, r, path.Join(f.dir, name))
	f.note("%s %s -> %d, %d bytes, ua=%q", r.Method, name, counted.status, counted.n,
		r.Header.Get("User-Agent"))

	f.mu.Lock()
	f.served += counted.n
	f.mu.Unlock()
}

// permitted reports whether a request may be answered at all.
//
// Two independent conditions, because neither is sufficient alone: the secret
// path is what a browser on some other machine cannot guess, and the address
// check is what stops a page running on the receiver's own network segment
// from being handed the stream if the path ever leaks.
func (f *Files) permitted(r *http.Request) bool {
	prefix, _, found := strings.Cut(strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/"), "/")
	if !found || subtle.ConstantTimeCompare([]byte(prefix), []byte(f.prefix)) != 1 {
		return false
	}
	if f.AllowFrom == "" {
		return true
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host == f.AllowFrom
}

func (f *Files) note(format string, args ...any) {
	if f.Log != nil {
		f.Log(fmt.Sprintf(format, args...))
	}
}

// servePlaylist sends the playlist with its target duration corrected.
//
// hlssink2 writes EXT-X-TARGETDURATION from the duration it was ASKED for,
// while EXTINF reports what each segment actually came out as. Those differ:
// segments end on a keyframe, and the capture's real framerate is below the
// nominal one, so a one-second target yields 1.2-second segments. The result
// is a playlist that violates the spec -- TARGETDURATION must be at least
// every EXTINF -- and a Chromecast responds by reading the playlist a few
// times and never requesting a single segment. Measured: 4 playlist reads, 0
// segment requests, no player state, indefinitely.
//
// Correcting it here rather than raising hlssink2's target keeps segments
// short, and segment length is the floor on how far behind the television runs.
func (f *Files) servePlaylist(w http.ResponseWriter, file string) {
	raw, err := os.ReadFile(file)
	if err != nil {
		f.note("playlist %s -> 404 (%v)", file, err)
		http.Error(w, "no playlist", http.StatusNotFound)
		return
	}
	body := repairTargetDuration(string(raw))
	f.note("playlist -> 200, %d bytes, %d segments listed", len(body),
		strings.Count(body, "#EXTINF:"))

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	n, _ := io.WriteString(w, body)

	f.mu.Lock()
	f.served += int64(n)
	f.mu.Unlock()
}

// repairTargetDuration raises EXT-X-TARGETDURATION to cover the longest
// segment the playlist actually lists.
func repairTargetDuration(playlist string) string {
	longest := 0.0
	for _, line := range strings.Split(playlist, "\n") {
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		value := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
		if d, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && d > longest {
			longest = d
		}
	}
	if longest == 0 {
		return playlist
	}

	want := int(math.Ceil(longest))
	lines := strings.Split(playlist, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
			continue
		}
		declared, err := strconv.Atoi(strings.TrimSpace(
			strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")))
		if err == nil && declared >= want {
			return playlist
		}
		lines[i] = "#EXT-X-TARGETDURATION:" + strconv.Itoa(want)
		return strings.Join(lines, "\n")
	}
	return playlist
}

// PlaylistNameHint is the playlist filename, used only to count re-reads.
var PlaylistNameHint = "live.m3u8"

type countingWriter struct {
	http.ResponseWriter
	n      int64
	status int
}

func (c *countingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}
