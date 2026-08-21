package stream

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func listen(t *testing.T) *Server {
	t.Helper()
	s, err := Listen("127.0.0.1", 0)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// connect opens the stream and returns the response body.
//
// Read through an HTTP client rather than off the socket: the server sends the
// body with chunked transfer encoding, since a live stream has no length to
// declare, and raw socket reads see the chunk framing instead of the video.
func connect(t *testing.T, s *Server) io.Reader {
	t.Helper()
	req, err := http.NewRequest("GET", s.URL(), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := (&http.Transport{DisableKeepAlives: true}).RoundTrip(req)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != s.ContentType {
		t.Errorf("Content-Type %q, want %q", got, s.ContentType)
	}
	return resp.Body
}

// waitForViewer blocks until the handler has registered, so a test does not
// write into a server that has not finished accepting.
func waitForViewer(t *testing.T, s *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Viewers() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d viewers connected, want %d", s.Viewers(), n)
}

func TestServerStreamsWhatIsWritten(t *testing.T) {
	s := listen(t)
	body := connect(t, s)
	waitForViewer(t, s, 1)

	s.Write([]byte("first"))
	s.Write([]byte("second"))

	got := make([]byte, len("firstsecond"))
	if _, err := io.ReadFull(body, got); err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if string(got) != "firstsecond" {
		t.Errorf("got %q, want %q", got, "firstsecond")
	}
	if s.Served() != int64(len(got)) {
		t.Errorf("Served() = %d, want %d", s.Served(), len(got))
	}
}

// The capture keeps encoding whether or not a television is watching. Those
// chunks must go nowhere rather than pile up -- a buffer of stale frames makes
// a receiver start behind and stay behind.
func TestWritesWithNoViewerAreDropped(t *testing.T) {
	s := listen(t)
	for i := 0; i < 1000; i++ {
		s.Write([]byte("discarded"))
	}

	body := connect(t, s)
	waitForViewer(t, s, 1)
	s.Write([]byte("live"))

	got := make([]byte, 4)
	if _, err := io.ReadFull(body, got); err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if string(got) != "live" {
		t.Errorf("got %q -- a receiver was served buffered history", got)
	}
}

// A Chromecast reconnects on its own: a network hiccup, a LOAD retry. The new
// connection is the real viewer, and the old one must be released rather than
// left holding a socket nobody reads.
func TestASecondViewerReplacesTheFirst(t *testing.T) {
	s := listen(t)
	first := connect(t, s)
	waitForViewer(t, s, 1)

	second := connect(t, s)
	waitForViewer(t, s, 2)

	// The first connection is closed by the server, so reading it ends.
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(first)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the replaced connection was left open")
	}

	s.Write([]byte("now"))
	got := make([]byte, 3)
	if _, err := io.ReadFull(second, got); err != nil {
		t.Fatalf("reading the new stream: %v", err)
	}
	if string(got) != "now" {
		t.Errorf("got %q, want %q", got, "now")
	}
}

// Binding to whatever 0.0.0.0 chooses puts the stream on a docker bridge the
// television cannot reach, while everything looks healthy from this machine.
func TestServerBindsToTheAddressItWasGiven(t *testing.T) {
	s := listen(t)
	if !strings.HasPrefix(s.URL(), "http://127.0.0.1:") {
		t.Errorf("URL() = %q, want it bound to 127.0.0.1", s.URL())
	}
	if !strings.HasSuffix(s.URL(), Path) {
		t.Errorf("URL() = %q, want it to end in %q", s.URL(), Path)
	}
}

func TestLocalAddressForReportsTheInterfaceFacingTheHost(t *testing.T) {
	got, err := LocalAddressFor("127.0.0.1")
	if err != nil {
		t.Fatalf("LocalAddressFor: %v", err)
	}
	if got != "127.0.0.1" {
		t.Errorf("got %q, want 127.0.0.1", got)
	}
}
