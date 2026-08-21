// Package stream serves a live capture over HTTP to a receiver that fetches
// it, which is how Chromecast works: castr sends the television a URL, and the
// television comes back for the video.
package stream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Path is where the stream lives. It is fixed rather than random: the URL is
// only reachable from the local network, and a guessable path costs nothing
// that the network does not already give away.
const Path = "/stream"

// Server hands a live byte stream to whoever connects.
//
// One viewer at a time is the whole design. A second connection replaces the
// first rather than forking the stream, because a Chromecast reconnects
// routinely -- on a network hiccup, on a LOAD retry -- and a server that kept
// the old connection would end up writing to a socket nobody reads while the
// television waits on a socket nobody writes.
type Server struct {
	// ContentType is what the receiver was told to expect.
	ContentType string

	listener net.Listener
	srv      *http.Server

	mu      sync.Mutex
	viewer  chan []byte
	served  int64
	viewers int
}

// Listen starts a server bound to the address the receiver can reach.
//
// The bind address is chosen deliberately rather than left at 0.0.0.0. This
// machine has docker bridges, and a stream bound to one of those is invisible
// to the television while looking perfectly healthy from here.
func Listen(bindIP string, port int) (*Server, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindIP, port))
	if err != nil {
		return nil, fmt.Errorf("serving the stream on %s:%d: %w", bindIP, port, err)
	}

	s := &Server{ContentType: "video/mp4", listener: ln}
	mux := http.NewServeMux()
	mux.HandleFunc(Path, s.handle)
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(ln)
	return s, nil
}

// URL is the address to give the receiver.
func (s *Server) URL() string {
	return fmt.Sprintf("http://%s%s", s.listener.Addr().String(), Path)
}

// Close stops serving.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// Served reports how many bytes have reached a receiver.
//
// This is the number that distinguishes "the television is playing" from "the
// television said OK and then did nothing", and no status message can
// substitute for it.
func (s *Server) Served() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served
}

// Viewers reports how many receivers have ever connected.
func (s *Server) Viewers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewers
}

// Write hands a chunk of the live stream to the connected viewer.
//
// A chunk arriving with nobody watching is DROPPED, not buffered. The capture
// is live: by the time a receiver connects, everything encoded before it is
// already too old to show, and a buffer would only make the television start
// several seconds behind and stay there.
func (s *Server) Write(chunk []byte) {
	s.mu.Lock()
	viewer := s.viewer
	s.mu.Unlock()
	if viewer == nil {
		return
	}
	select {
	case viewer <- chunk:
	default:
		// The receiver is not keeping up. Dropping is right for the same
		// reason: a live stream that waits for a slow viewer stops being live.
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	chunks := make(chan []byte, 8)
	s.mu.Lock()
	previous := s.viewer
	s.viewer = chunks
	s.viewers++
	s.mu.Unlock()
	if previous != nil {
		close(previous)
	}

	defer func() {
		s.mu.Lock()
		if s.viewer == chunks {
			s.viewer = nil
		}
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", s.ContentType)
	// No Content-Length and no ranges: the stream has no end and no middle to
	// seek to. A receiver that asks for a range gets the live stream anyway.
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case chunk, open := <-chunks:
			if !open {
				return // replaced by a newer connection
			}
			n, err := w.Write(chunk)
			s.mu.Lock()
			s.served += int64(n)
			s.mu.Unlock()
			if err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// LocalAddressFor reports the address on this machine that can reach the
// given host -- the one to hand a receiver.
//
// No packet is sent: connecting a UDP socket only asks the routing table which
// interface would be used. That is the question being asked, and it answers it
// without depending on the receiver being up.
func LocalAddressFor(host string) (string, error) {
	conn, err := net.Dial("udp", net.JoinHostPort(host, "9"))
	if err != nil {
		return "", fmt.Errorf("no route to %s: %w", host, err)
	}
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", errors.New("could not determine the local address")
	}
	return addr.IP.String(), nil
}
