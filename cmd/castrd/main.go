// Command castrd is castr's background daemon.
//
// Everything with an effect on the outside world is wired up here: running
// hyprctl and avahi-browse, spawning doubletake, showing notifications. The
// packages below take those as function fields, which is why they can be tested
// without a compositor, a network, or a receiver.
//
// Three rules live here because no package can enforce them for itself:
//
//  1. The lock is taken BEFORE the stray-output sweep. A second daemon cannot
//     tell a leftover output from one a live cast is using in another process.
//  2. doubletake is spawned in its own process GROUP and signalled as a group,
//     or its GStreamer capture pipelines outlive it holding the GPU.
//  3. Notifications follow the policy in internal/notify: only what the user
//     must act on, one banner at a time.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mrCode/castr/internal/backend/airplay"
	"github.com/mrCode/castr/internal/config"
	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/hypr"
	"github.com/mrCode/castr/internal/notify"
	"github.com/mrCode/castr/internal/session"
)

// version is set at build time with -X main.version.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	idleTimeout := flag.Duration("idle-timeout", daemon.IdleTimeout,
		"exit after this long with nothing casting")
	verbose := flag.Bool("v", false, "log every state change")
	flag.Parse()

	if *showVersion {
		fmt.Println("castrd", version)
		return
	}

	if err := run(*idleTimeout, *verbose); err != nil {
		log.Fatalf("castrd: %v", err)
	}
}

func run(idleTimeout time.Duration, verbose bool) error {
	log.SetFlags(log.Ltime)
	if !verbose {
		log.SetOutput(io.Discard)
	}

	stateDir := daemon.StateDir()
	configDir := config.Dir()

	// First run after an upgrade: bring omarchy-cast's settings and its
	// hand-typed receivers across. A COPY -- omarchy-cast is still installed
	// and must keep working.
	if copied, err := config.Migrate(configDir, stateDir,
		legacyDir(configDir), legacyDir(stateDir)); err != nil {
		log.Printf("migration: %v", err) // not fatal; castr works without it
	} else if len(copied) > 0 {
		log.Printf("migrated from omarchy-cast: %s", strings.Join(copied, ", "))
	}

	// RULE 1: the lock comes first. Everything below this line touches state a
	// second daemon could destroy.
	lock, err := daemon.Acquire(daemon.LockPath(stateDir), daemon.LockWait)
	if err != nil {
		return err
	}
	defer lock.Release()

	// Only now is it safe to sweep: with the lock held, any castr output still
	// present belongs to a daemon that is gone.
	if removed, err := hypr.CleanupStrays(hyprctl, nil); err != nil {
		log.Printf("cleanup: %v", err)
	} else if removed > 0 {
		log.Printf("removed %d leftover output(s)", removed)
	}
	if hypr.RestorePanelIfPending(hyprctl, stateDir) {
		log.Print("restored the panel mode left behind by a previous daemon")
	}

	cfg, err := config.Load(filepath.Join(configDir, "config.toml"))
	if err != nil {
		// Reported, not fatal: a typo in a config file must not stop the user
		// casting, and Load has already fallen back to the defaults.
		log.Printf("config: %v", err)
	}

	registry := daemon.NewRegistry(browse, time.Now)
	for _, device := range config.LoadDevices(config.DevicesPath(stateDir)) {
		registry.Add(device)
	}

	notifier := notify.Notifier{Run: runNotify}
	backend := newAirPlayBackend(cfg, stateDir)

	d := daemon.New(registry, map[string]daemon.Backend{
		discovery.ProtocolAirPlay: backend,
	})
	d.IdleTimeout = idleTimeout
	d.Notify = func(device discovery.Device, state session.State, reason string) {
		log.Printf("%s: %s %s", device.Name, state, reason)
		notifier.OnState(device, state, reason)
	}
	backend.Emit = d.OnState

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go d.WatchDiscovery(daemon.BrowseInterval)
	go d.WatchIdle(time.Second)
	go persistDevices(d, registry, stateDir)

	socket := daemon.DefaultSocketPath()
	log.Printf("listening on %s", socket)
	serveErr := d.Serve(ctx, socket)

	// Reached on SIGTERM, on `castr quit`, and on the idle timeout. Without
	// this, logout and reboot leave doubletake and its capture pipelines alive
	// with nothing owning them.
	shutdown(d, backend, registry, stateDir)
	return serveErr
}

// shutdown tears down live casts and saves what should outlive us.
func shutdown(d *daemon.Daemon, backend *airplay.Backend, registry *daemon.Registry, stateDir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, s := range d.Sessions() {
		if err := backend.Stop(ctx, s.Device); err != nil {
			log.Printf("stopping %s: %v", s.Device.Name, err)
		}
	}
	// A last sweep, because a session that failed to stop cleanly still leaves
	// an output behind, and the next daemon cannot tell it apart from a live one.
	if _, err := hypr.CleanupStrays(hyprctl, nil); err != nil {
		log.Printf("final cleanup: %v", err)
	}
	if err := config.SaveDevices(config.DevicesPath(stateDir), registry.ManualDevices()); err != nil {
		log.Printf("saving receivers: %v", err)
	}
}

// persistDevices saves the hand-typed receivers as they change.
//
// Not only at shutdown: a daemon killed with SIGKILL saves nothing, and the
// receivers that need typing are exactly the ones discovery will never find
// on its own.
func persistDevices(d *daemon.Daemon, registry *daemon.Registry, stateDir string) {
	path := config.DevicesPath(stateDir)
	last := len(registry.ManualDevices())

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.Stopping():
			return
		case <-ticker.C:
			devices := registry.ManualDevices()
			if len(devices) == last {
				continue
			}
			last = len(devices)
			if err := config.SaveDevices(path, devices); err != nil {
				log.Printf("saving receivers: %v", err)
			}
		}
	}
}

func legacyDir(dir string) string {
	return filepath.Join(filepath.Dir(dir), config.LegacyName)
}

// hyprctl runs a compositor command.
func hyprctl(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// browse asks avahi what is on the network.
//
// avahi rather than a Go mDNS library on purpose: avahi has been running since
// boot with a warm cache and answers instantly, while a freshly started browser
// takes seconds. A cold browser is why `list` came back empty and `start` said
// "device not found" for receivers plainly present.
func browse() ([]discovery.Device, error) {
	return discovery.Browse(hyprctl, discovery.ProtocolAirPlay)
}

func runNotify(argv []string) error {
	return exec.Command(argv[0], argv[1:]...).Run()
}

// newAirPlayBackend wires doubletake up to the real system.
func newAirPlayBackend(cfg config.Config, stateDir string) *airplay.Backend {
	return &airplay.Backend{
		Config: airplay.Config{
			PortRange:       cfg.AirPlay.PortRange,
			FPS:             cfg.Capture.FPS,
			Encoder:         cfg.Capture.Encoder,
			Bitrate:         cfg.AirPlay.Bitrate,
			TargetLatencyMS: cfg.AirPlay.TargetLatencyMS,
			Audio:           cfg.AirPlay.Audio,
		},
		Hypr:         hyprctl,
		ReadyTimeout: time.Duration(cfg.AirPlay.ReadyTimeout * float64(time.Second)),
		Creds:        func(string) (string, error) { return credsPath(stateDir) },
		Spawn:        spawner(cfg, stateDir),

		// The fallback, never the normal path -- see hypr.SwitchPanel.
		SwitchDisplay:  func() error { return hypr.SwitchPanel(hyprctl, stateDir) },
		RestoreDisplay: func() error { return hypr.RestorePanel(hyprctl, stateDir) },
	}
}

// credsPath is where doubletake keeps its AirPlay pairing credentials.
//
// ONE file for every mode and every receiver. doubletake keys the contents by
// receiver itself, so a per-mode split gains nothing and costs the user a
// second pairing -- typing the code off the television again the first time
// they extend to a receiver they already mirror to.
func credsPath(stateDir string) (string, error) {
	dir := filepath.Join(stateDir, "creds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	return filepath.Join(dir, "pairing.json"), nil
}
