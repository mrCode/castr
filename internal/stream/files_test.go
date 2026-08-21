package stream

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serveTemp(t *testing.T) *Files {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "live.m3u8"),
		[]byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.2,\nsegment00000.ts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "segment00000.ts"), []byte("PIXELS"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := ServeDir("127.0.0.1", 0, dir)
	if err != nil {
		t.Fatalf("ServeDir: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Transport{DisableKeepAlives: true}).RoundTrip(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTheReceiverCanFetchThePlaylistAndSegments(t *testing.T) {
	f := serveTemp(t)

	if resp := get(t, f.URLFor("live.m3u8")); resp.StatusCode != 200 {
		t.Fatalf("playlist: status %d", resp.StatusCode)
	}
	if resp := get(t, f.URLFor("segment00000.ts")); resp.StatusCode != 200 {
		t.Fatalf("segment: status %d", resp.StatusCode)
	}
}

// The stream must be readable cross-origin or the receiver will not play it,
// which means any page the user's browser loads could fetch it too -- if it
// can find it. A fixed port and a fixed filename is a few hundred requests to
// sweep a /24, so the path carries a secret.
func TestTheStreamIsNotReachableWithoutTheSecretPath(t *testing.T) {
	f := serveTemp(t)
	base := "http://" + f.listener.Addr().String()

	for _, guess := range []string{
		"/live.m3u8",
		"/segment00000.ts",
		"/0000000000000000000000000000000/live.m3u8",
		"/../live.m3u8",
	} {
		if resp := get(t, base+guess); resp.StatusCode == 200 {
			t.Errorf("%s was served without the secret path", guess)
		}
	}
}

func TestTheSecretPathIsDifferentForEveryCast(t *testing.T) {
	if a, b := serveTemp(t), serveTemp(t); a.prefix == b.prefix {
		t.Fatal("two casts shared a path; it is not a secret if it is reused")
	}
	if len(serveTemp(t).prefix) < 32 {
		t.Error("the path secret is too short to be unguessable")
	}
}

// The receiver's address is known before the server starts, and nothing else
// has any business fetching the user's screen.
func TestAnotherAddressIsRefusedEvenWithTheRightPath(t *testing.T) {
	f := serveTemp(t)
	f.AllowFrom = "192.168.100.48" // the television, which is not this test

	resp := get(t, f.URLFor("live.m3u8"))
	if resp.StatusCode == 200 {
		t.Fatal("a request from an address that is not the receiver was served the screen")
	}
	// Indistinguishable from a wrong path: a caller that is not the receiver
	// should not learn whether a cast is running.
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404 so the refusal reveals nothing", resp.StatusCode)
	}
}

func TestPathTraversalCannotEscapeTheDirectory(t *testing.T) {
	f := serveTemp(t)
	base := "http://" + f.listener.Addr().String() + "/" + f.prefix

	for _, attempt := range []string{
		"/../../../../etc/passwd",
		"/..%2f..%2f..%2fetc%2fpasswd",
		"/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/....//....//etc/passwd",
	} {
		resp := get(t, base+attempt)
		if resp.StatusCode == 200 {
			t.Errorf("%s was served", attempt)
		}
	}
}

// hlssink2 writes the target duration it was asked for, while EXTINF reports
// what each segment actually came out as. A playlist claiming a shorter target
// than its own segments violates the spec, and a Chromecast responds by
// reading it a few times and never requesting a segment.
func TestThePlaylistTargetDurationCoversItsSegments(t *testing.T) {
	f := serveTemp(t)

	resp := get(t, f.URLFor("live.m3u8"))
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	got := string(body[:n])

	if !strings.Contains(got, "#EXT-X-TARGETDURATION:2") {
		t.Errorf("target duration was not raised to cover a 1.2s segment:\n%s", got)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q, want the RFC 8216 type", ct)
	}
}

func TestSegmentsAreServedAsTransportStreams(t *testing.T) {
	f := serveTemp(t)
	// Go's MIME table has no .ts entry on many systems, and a receiver handed
	// text/plain for a transport stream will not decode it.
	if ct := get(t, f.URLFor("segment00000.ts")).Header.Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("Content-Type = %q, want video/mp2t", ct)
	}
}
