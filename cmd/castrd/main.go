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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mrCode/castr/internal/backend/airplay"
	"github.com/mrCode/castr/internal/backend/chromecast"
	"github.com/mrCode/castr/internal/config"
	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/hypr"
	"github.com/mrCode/castr/internal/notify"
	"github.com/mrCode/castr/internal/session"
	"github.com/mrCode/castr/internal/sound"
)

// version is set at build time with -X main.version.
var version = "dev"

func main() {
	// The sound guard is castrd started again to restore the laptop's sound
	// if the daemon dies. It takes no lock and reads no config.
	if len(os.Args) > 1 && os.Args[1] == sound.GuardFlag {
		os.Exit(runSoundGuard(os.Args[2:]))
	}

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
	// Likewise the sound routing a dead daemon left -- whatever the config
	// says now, since it may have been turned off since.
	if err := sound.Recover(execRunner, stateDir); err != nil {
		log.Printf("restoring the sound: %v", err)
	}

	cfg, err := config.Load(filepath.Join(configDir, "config.toml"))
	if err != nil {
		// Reported, not fatal: a typo in a config file must not stop the user
		// casting, and Load has already fallen back to the defaults.
		log.Printf("config: %v", err)
	}

	// Every mode learns every receiver castr has paired with, so switching
	// between mirror and extend never asks for a code that was already typed.
	if err := config.SyncPairing(stateDir, session.Modes); err != nil {
		log.Printf("syncing pairings: %v", err)
	}

	registry := daemon.NewRegistry(browse, time.Now)
	for _, device := range config.LoadDevices(config.DevicesPath(stateDir)) {
		registry.Add(device)
	}

	notifier := notify.Notifier{
		Run:         runNotify,
		Prompt:      promptNotify,
		PairCommand: codePromptCommand,
		Start:       startDetached,
	}
	backend := newAirPlayBackend(cfg, stateDir)
	castBackend := newChromecastBackend(cfg)

	d := daemon.New(registry, map[string]daemon.Backend{
		discovery.ProtocolAirPlay:    backend,
		discovery.ProtocolChromecast: castBackend,
	})
	d.IdleTimeout = idleTimeout
	router := sound.NewRouter(execRunner, stateDir)
	router.StartGuard = func() error { return sound.StartGuard(stateDir) }
	router.HistoryFile = sound.WirePlumberHistoryFile()
	router.Logf = log.Printf // shown with -v
	d.Sound = router
	// [airplay] audio predates Chromecast sound; "sound off" means off for
	// every receiver, and castr then leaves the laptop's sound alone.
	d.SoundOn = cfg.AirPlay.Audio
	backend.AudioSource = d.AudioSource
	castBackend.AudioSource = d.AudioSource
	d.Notify = func(device discovery.Device, state session.State, reason string) {
		log.Printf("%s: %s %s", device.Name, state, reason)
		notifier.OnState(device, state, reason)
	}
	backend.Emit = d.OnState
	castBackend.Emit = d.OnState

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go d.WatchDiscovery(daemon.BrowseInterval)
	go d.WatchIdle(time.Second)
	go d.WatchSound(time.Second)
	go persistDevices(d, registry, stateDir)

	socket := daemon.DefaultSocketPath()
	log.Printf("listening on %s", socket)
	serveErr := d.Serve(ctx, socket)

	// Reached on SIGTERM, on `castr quit`, and on the idle timeout. Without
	// this, logout and reboot leave doubletake and its capture pipelines alive
	// with nothing owning them.
	shutdown(d, backend, registry, stateDir, hyprctl)
	return serveErr
}

// shutdown tears down live casts and saves what should outlive us. hyprctl
// is a parameter so a test can run it without touching the compositor.
func shutdown(d *daemon.Daemon, backend *airplay.Backend, registry *daemon.Registry, stateDir string, hyprctl hypr.Runner) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, s := range d.Sessions() {
		if err := backend.Stop(ctx, s.Device); err != nil {
			log.Printf("stopping %s: %v", s.Device.Name, err)
		}
	}
	// After the casts, and for all of them: the loop above stops only the
	// AirPlay ones, and a Chromecast's routing must not outlive the daemon.
	if err := d.RestoreSound(); err != nil {
		log.Printf("restoring the sound: %v", err)
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
	return discovery.BrowseAll(hyprctl)
}

func runNotify(argv []string) error {
	return exec.Command(argv[0], argv[1:]...).Run()
}

// promptNotify shows a banner with buttons and returns the name of the button
// clicked, which notify-send prints when given --wait.
func promptNotify(argv []string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	return string(out), err
}

// codePromptCommand is what the pairing banner runs when clicked: `castr pin
// <id>`, which asks for the code the receiver is showing and sends it.
//
// The prompt belongs to the CLI because that is where castr's menu program
// already lives; the daemon only knows the moment it is needed.
func codePromptCommand(device discovery.Device) []string {
	return []string{castrBinary(), "pin", device.ID}
}

// startDetached runs a command in its own session, so it is not tied to the
// daemon, and reaps it so it leaves no zombie behind.
func startDetached(argv []string) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Printf("could not start %v: %v", argv, err)
		return
	}
	go func() { _ = cmd.Wait() }()
}

// castrBinary finds the CLI: on PATH, or beside this daemon, so a build
// directory works without installing anything.
func castrBinary() string {
	if path, err := exec.LookPath("castr"); err == nil {
		return path
	}
	if self, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), "castr")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "castr"
}

// newChromecastBackend wires the Cast backend up to the real system.
//
// It takes no state directory: a Chromecast needs no pairing keys, so there is
// nothing about a receiver worth keeping between casts.
func newChromecastBackend(cfg config.Config) *chromecast.Backend {
	return chromecast.NewLive(chromecast.Config{
		FPS:            cfg.Capture.FPS,
		Encoder:        gstEncoder(cfg.Capture.Encoder),
		Bitrate:        cfg.Chromecast.Bitrate,
		Width:          cfg.Chromecast.Width,
		Height:         cfg.Chromecast.Height,
		Port:           cfg.Chromecast.Port,
		SegmentSeconds: cfg.Chromecast.SegmentSeconds,
		// [airplay] audio predates Chromecast sound; "sound off" means off
		// for every receiver.
		Audio: cfg.AirPlay.Audio,
	}, nil)
}

// gstEncoder turns castr's encoder name into a GStreamer element.
//
// "auto" resolves to VA-API, then NVENC, then x264. The order is not a
// preference between vendors: a hardware encoder leaves the CPU free for the
// scaling and the HTTP server, which run in this process rather than in
// doubletake.
func gstEncoder(name string) string {
	switch name {
	case "vaapi":
		return "vah264enc"
	case "nvenc":
		return "nvh264enc"
	case "x264":
		return "x264enc"
	}
	for _, element := range []string{"vah264enc", "nvh264enc"} {
		if gstHasElement(element) {
			return element
		}
	}
	return "x264enc"
}

func gstHasElement(name string) bool {
	return exec.Command("gst-inspect-1.0", name).Run() == nil
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
			Codec:           cfg.Capture.Codec,
			MaxSize:         cfg.Capture.MaxSize(),
		},
		Geometry:     cfg.Capture.Geometry,
		Hypr:         hyprctl,
		ReadyTimeout: time.Duration(cfg.AirPlay.ReadyTimeout * float64(time.Second)),
		Creds:        func(mode string) (string, error) { return credsPath(stateDir, mode) },
		PullPairing: func(mode string) error {
			return config.PullPairing(stateDir, mode, session.Modes)
		},
		PairAgain: func(d discovery.Device, mode string) (string, bool, error) {
			id, address := airplay.PairingKey(d)
			mark, ok := config.PairAgainFor(stateDir, id, address)
			if !ok {
				return "", false, nil
			}
			if !config.HasCodePairing(stateDir, mode, mark.DeviceID) {
				return "", false, fmt.Errorf("castr cannot pair again with %s in %s mode yet: this "+
					"mode holds no pairing with it made with a code to pair again with. Stop your "+
					"other %s casts and try again, or cast in the other mode", d.Name, mode, mode)
			}
			return mark.DeviceID, true, nil
		},
		PairedAgain: func(id string) {
			if err := config.ClearPairAgain(stateDir, id); err != nil {
				log.Printf("clearing the pair-again mark for %s: %v", id, err)
			}
		},
		Spawn: spawner(cfg, stateDir),
	}
}

// credsPath is doubletake's credentials file for one mode.
//
// Per mode, because each entry carries a screen-share portal restore_token
// alongside the receiver's pairing keys, and the token describes an OUTPUT.
// Mirror and extend capture different outputs, so one shared file makes extend
// replay mirror's grant and cast the wrong screen, silently. config.SyncPairing
// shares the pairing keys across modes so nobody pairs twice.
func credsPath(stateDir, mode string) (string, error) {
	dir := filepath.Join(stateDir, "creds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	return config.CredsPath(stateDir, mode), nil
}

// runSoundGuard runs `castrd --sound-guard <pid> <stateDir>` and returns the
// exit code: 2 for bad arguments, 1 if the sound could not be restored.
func runSoundGuard(args []string) int {
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: castrd --sound-guard <pid> <state-dir>")
		return 2
	}
	if len(args) != 2 || args[1] == "" {
		return usage()
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil || pid <= 0 {
		return usage()
	}
	stateDir := args[1]
	// Its stderr is the daemon's: a reader that has gone must not kill it.
	signal.Ignore(syscall.SIGPIPE)
	err = sound.Guard(context.Background(), pid, stateDir, sound.ProcessAlive,
		func() error { return sound.Recover(execRunner, stateDir) },
		guardTiming)
	if err != nil {
		fmt.Fprintf(os.Stderr, "castrd %s: restoring the sound: %v\n", sound.GuardFlag, err)
		return 1
	}
	return 0
}

// guardTiming: the laptop comes back within half a second of the daemon
// dying, and a recovery that fails is retried every second for a minute.
var guardTiming = sound.GuardTiming{Poll: 500 * time.Millisecond, Retry: time.Second, RetryFor: time.Minute}

// soundTimeout bounds each pactl and pw-metadata call. The router holds its
// lock across them, and status, stop and shutdown wait on that lock: a hung
// call must not freeze castrd.
var soundTimeout = 5 * time.Second

// execRunner runs a sound command and returns its standard output, killing
// it if it outlasts soundTimeout.
func execRunner(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), soundTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second // in case a child it left holds the output pipe
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("%s timed out after %v", name, soundTimeout)
	}
	return string(out), err
}
