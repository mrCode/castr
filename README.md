# castr

Mirror your Hyprland desktop to an Apple TV — or extend onto it as a second
monitor — from your desktop's menu, with a bar indicator.

Two small static binaries -- a client and a daemon. `avahi` for discovery, `doubletake` for AirPlay,
`hyprctl` for outputs; nothing else at runtime.

> **Status: feature-complete, not yet verified against a receiver.** Every
> package is built and tested, and the whole loop has been run end to end
> against real avahi discovery and a real compositor — but an actual cast to a
> television has not happened yet. Until it has, use
> [omarchy-cast](https://github.com/mrCode/omarchy-cast), which is verified on
> hardware and on the AUR.

## Why a rewrite

omarchy-cast is Python and installing it pulls 14 packages and a 73 MiB
runtime. The shipped feature set needs none of that: AirPlay capture belongs to
`doubletake` (itself a Go binary), and our own code is subprocess orchestration
— nine call sites, no GStreamer bindings, no D-Bus, no cgo.

```
omarchy-cast   14 packages, 73 MiB runtime
castr           1 package (avahi) + doubletake, two static binaries, 6.5 MB total
```

## Using it

```bash
castr menu                 # pick a receiver and a mode — bind this to a key
castr list                 # what is on the network
castr start <id> extend    # or mirror, which is the default
castr stop                 # stop everything
castr add 10.0.0.5 "TV"    # a receiver mDNS cannot see
```

**Mirror** shows this screen on the receiver. **Extend** gives you a second
desktop on it. Mirror does *not* change your panel's mode: castr creates a
virtual output that mirrors the panel, so a 240 Hz display stays at 240 Hz.

The daemon starts itself on first use and exits after fifteen minutes idle. It
stays that long on purpose — discovery is only fast while its cache is warm.

## Installing

```bash
yay -S castr doubletake-git
```

`doubletake` is what actually speaks AirPlay, and it is required to cast.
Version 0.4.0 cannot capture on Hyprland; use `doubletake-git`.

Your firewall has to let the receiver connect **back** to this machine:

```bash
sudo ufw allow 5353/udp
```

```bash
sudo ufw allow 60000:60010/tcp
```

```bash
sudo ufw allow 60000:60010/udp
```

Cross-subnet works. If a receiver is on another network and reachable, it will
cast — this was tested, and it does not require switching Wi-Fi.

### Bar indicator

Both bars are shipped in `/usr/share/castr/`:

- **Quickshell** (Omarchy "Quarto" and later) — copy
  `quickshell/castr-indicator/` into your Quickshell plugins directory.
- **waybar** (earlier setups) — merge `waybar/cast-indicator.jsonc` into your
  config and append `waybar/cast-indicator.css` to your style.

The indicator is visible whether or not you are casting, and colour carries the
state. Left-click opens the menu, right-click stops.

## Configuration

`~/.config/castr/config.toml`, all optional:

```toml
[capture]
fps = 30
encoder = "auto"            # auto | vaapi | nvenc | x264

[airplay]
port_range = "60000-60010"  # the receiver connects back into these
target_latency_ms = 100     # lower is more responsive, less jitter-tolerant
ready_timeout = 60          # capture can begin 23s after the session is ready
audio = true
```

Upgrading from omarchy-cast? Your settings and hand-added receivers are copied
across on first run. omarchy-cast is left untouched and keeps working.

## Building

```bash
go build ./cmd/castr ./cmd/castrd
```

```bash
go test ./...
```

The tests need no compositor, no network, and no receiver: every external
command is injected. The exception is `cmd/castrd`, which spawns real
short-lived shells to prove that terminating a cast kills the whole process
group — the one rule a fake cannot demonstrate.

## Design

[docs/design.md](docs/design.md), whose most important section is *"What the
rewrite must not lose"* — every bug the Python version already shipped and
fixed, restated as a required test so this one does not rediscover them.
[PROGRESS.md](PROGRESS.md) tracks which of those are closed.

## Not supported

**Chromecast.** omarchy-cast has a Chromecast backend that reached "streaming
with data flowing" on real hardware and is nonetheless disabled: its capture
path was observed sending the *webcam* instead of the screen. castr does not
carry it forward until that is understood.

## License

MIT
