package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mrCode/castr/internal/capture"
	"github.com/mrCode/castr/internal/cast"
	"github.com/mrCode/castr/internal/portal"
	"github.com/mrCode/castr/internal/stream"
)

func main() {
	container := capture.HLS
	encoder := "vah264enc"
	if len(os.Args) > 1 {
		container = capture.Container(os.Args[1])
	}
	if len(os.Args) > 2 {
		encoder = os.Args[2]
	}
	dir, _ := os.MkdirTemp("", "castr-hls-")
	defer os.RemoveAll(dir)
	const receiver = "192.168.100.48"

	tokenFile := os.Getenv("HOME") + "/.local/state/castr/probe-restore-token"
	prev, _ := os.ReadFile(tokenFile)
	sess, err := portal.Open(context.Background(), portal.Options{
		RestoreToken: string(prev), Persist: 2, Timeout: 2 * time.Minute,
	})
	if err != nil {
		die("portal", err)
	}
	defer sess.Close()
	if sess.RestoreToken != "" {
		os.WriteFile(tokenFile, []byte(sess.RestoreToken), 0o600)
	}

	graph := capture.NewPipeWire()
	serial, err := graph.SerialOf(sess.NodeID)
	if err != nil {
		die("serial", err)
	}
	fmt.Printf("portal node=%d serial=%d\n", sess.NodeID, serial)

	opts := capture.Options{
		NodeID: sess.NodeID, Serial: serial, FD: sess.FD,
		FPS: 30, Encoder: encoder, Bitrate: 4000, Container: container,
		Dir: dir, SegmentSeconds: 1, Width: 1280, Height: 800,
	}
	fmt.Println("pipeline:", opts.Args())

	cmd, err := opts.Command("gst-launch-1.0")
	if err != nil {
		die("command", err)
	}
	// os.Pipe rather than StdoutPipe: Wait closes the latter underneath us.
	pr, pw, err := os.Pipe()
	if err != nil {
		die("pipe", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		die("start", err)
	}
	pw.Close()
	defer cmd.Process.Kill()

	bind, err := stream.LocalAddressFor(receiver)
	if err != nil {
		die("route", err)
	}
	srv, err := stream.ServeDir(bind, 8010, dir)
	if err != nil {
		die("listen", err)
	}
	defer srv.Close()
	srv.Log = func(line string) { fmt.Println("  HTTP", line) }
	url := srv.URLFor(capture.PlaylistName)
	fmt.Println("serving", url, "as", opts.ContentType())
	go func() {
		buf := make([]byte, 32*1024)
		for {
			if _, err := pr.Read(buf); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	g := &capture.Guard{Graph: graph, Granted: sess.NodeID, Timeout: 8 * time.Second}
	if err := g.Verify(ctx, cmd.Process.Pid); err != nil {
		fmt.Println("GUARD REFUSED:", err)
		os.Exit(1)
	}
	fmt.Println("guard: capturing the granted screen")

	conn, err := cast.Dial(ctx, receiver+":8009")
	if err != nil {
		die("dial", err)
	}
	defer conn.Close()
	conn.Observe = func(ns, payload string) {
		short := ns[strings.LastIndex(ns, ".")+1:]
		if len(payload) > 400 {
			payload = payload[:400] + "..."
		}
		fmt.Printf("  RECV[%s] %s\n", short, payload)
	}
	app, err := conn.Launch(ctx, cast.DefaultMediaReceiver)
	if err != nil {
		die("launch", err)
	}
	// Wait until the playlist holds a few segments. A receiver will not start
	// on a playlist with one or two: HLS clients want roughly three target
	// durations of media available before they begin.
	for i := 0; i < 80; i++ {
		b, err := os.ReadFile(dir + "/" + capture.PlaylistName)
		if err == nil && strings.Count(string(b), "#EXTINF:") >= 4 {
			fmt.Println("  --- playlist at LOAD time ---")
			for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				fmt.Println("   ", l)
			}
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if err := conn.Load(ctx, app, cast.Media{
		URL: url, ContentType: opts.ContentType(), Title: "castr", Live: true,
	}); err != nil {
		fmt.Println("LOAD FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("LOAD accepted -- watching 60s")

	last := int64(0)
	for i := 0; i < 12; i++ {
		time.Sleep(5 * time.Second)
		now, reqs, playlists := srv.Stats()
		state, _ := conn.PlayerState(ctx, app)
		fmt.Printf("  t+%2ds  served=%d KB (+%d KB)  requests=%d playlist-reads=%d  player=%s\n",
			(i+1)*5, now/1024, (now-last)/1024, reqs, playlists, state)
		last = now
	}
	conn.Stop(ctx, app)
	fmt.Println("stopped")
	_ = io.Discard
}

func die(what string, err error) {
	fmt.Printf("%s FAILED: %v\n", what, err)
	os.Exit(1)
}
