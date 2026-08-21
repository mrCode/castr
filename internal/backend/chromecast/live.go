package chromecast

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/mrCode/castr/internal/capture"
	"github.com/mrCode/castr/internal/cast"
	"github.com/mrCode/castr/internal/portal"
	"github.com/mrCode/castr/internal/stream"
)

// NewLive builds a backend wired to the real portal, PipeWire, GStreamer and
// network.
func NewLive(cfg Config, emit Emit) *Backend {
	graph := capture.NewPipeWire()
	return &Backend{
		Config: cfg,
		Emit:   emit,
		Graph:  graph,
		OpenPortal: func(ctx context.Context) (Portal, error) {
			// Persist is deliberately off. A Chromecast cast is started from a
			// menu, and a remembered grant would let a later cast begin
			// capturing with no prompt at all -- the user should be asked what
			// to share every time something starts watching the screen.
			s, err := portal.Open(ctx, portal.Options{Timeout: 2 * time.Minute})
			if err != nil {
				return nil, err
			}
			return &livePortal{s}, nil
		},
		SerialOf:     graph.SerialOf,
		StartCapture: startCapture,
		Serve: func(bindIP string, port int, dir string) (Server, error) {
			return stream.ServeDir(bindIP, port, dir)
		},
		Restrict: func(s Server, receiverIP string) {
			if f, ok := s.(*stream.Files); ok {
				f.AllowFrom = receiverIP
			}
		},
		Dial: func(ctx context.Context, addr string) (Caster, error) {
			c, err := cast.Dial(ctx, addr)
			if err != nil {
				return nil, err
			}
			return &liveCaster{c}, nil
		},
		LocalAddress: stream.LocalAddressFor,
		TempDir:      func() (string, error) { return os.MkdirTemp("", "castr-cast-") },
	}
}

type livePortal struct{ s *portal.Session }

func (p *livePortal) Node() uint32         { return p.s.NodeID }
func (p *livePortal) Descriptor() *os.File { return p.s.FD }
func (p *livePortal) Close() error         { return p.s.Close() }

type liveCaster struct{ c *cast.Conn }

func (l *liveCaster) Launch(ctx context.Context, appID string) (App, error) {
	app, err := l.c.Launch(ctx, appID)
	if err != nil {
		return App{}, err
	}
	return App{SessionID: app.SessionID, TransportID: app.TransportID, AppID: app.AppID}, nil
}

func (l *liveCaster) Load(ctx context.Context, app App, url, contentType, title string) error {
	return l.c.Load(ctx, cast.App{
		AppID: app.AppID, SessionID: app.SessionID, TransportID: app.TransportID,
	}, cast.Media{URL: url, ContentType: contentType, Title: title, Live: true})
}

func (l *liveCaster) StopApp(ctx context.Context, app App) error {
	return l.c.Stop(ctx, cast.App{
		AppID: app.AppID, SessionID: app.SessionID, TransportID: app.TransportID,
	})
}

func (l *liveCaster) Close() error { return l.c.Close() }

// process is a running gst-launch, terminated as a group.
type process struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startCapture(opts capture.Options) (Pipeline, error) {
	cmd, err := opts.Command("gst-launch-1.0")
	if err != nil {
		return nil, err
	}
	// The pipeline writes its video to files, so its output is only ever
	// diagnostics. Discarding it rather than leaving the pipe unread avoids a
	// child that blocks forever on a full buffer.
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting gst-launch-1.0: %w", err)
	}

	p := &process{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *process) Pid() int { return p.cmd.Process.Pid }

func (p *process) Wait() error {
	<-p.done
	return p.err
}

// Terminate signals the whole process group.
//
// gst-launch spawns helpers, and signalling only the parent leaves them
// holding the portal session and the encoder. Four of those accumulated during
// development, one running for thirteen minutes after its parent was gone.
func (p *process) Terminate() error {
	if p.cmd.Process == nil {
		return nil
	}
	// Checked BEFORE signalling. Once the child has been reaped its pid is
	// back in the kernel's pool, and syscall.Kill bypasses the "already
	// finished" guard that os.Process.Signal would apply -- so signalling an
	// exited child sends SIGTERM, and then SIGKILL, to whatever process group
	// now holds that number.
	select {
	case <-p.done:
		return nil
	default:
	}
	pid := p.cmd.Process.Pid

	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil &&
		!errors.Is(err, syscall.ESRCH) {
		// Falling back to the process alone is better than not signalling at
		// all, even though it can leave children behind.
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}

	select {
	case <-p.done:
		return nil
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-p.done
		return nil
	}
}
