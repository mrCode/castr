# castr

Mirror your Hyprland desktop to an Apple TV or a Chromecast — or extend onto an
Apple TV as a second monitor — from your desktop's menu, with a bar indicator.

Static binaries -- a client, a daemon, and `castr-airplay`, castr's own build
of [doubletake](https://github.com/omarroth/doubletake) for AirPlay. `avahi`
for discovery, `hyprctl` for outputs, GStreamer to capture and encode.

> **Status: released, verified on hardware.** v0.4.0 is on the AUR. Mirror and
> extend have both been cast to a real Apple TV, and mirror to a real Chromecast
> (a Xiaomi TV Stick, a minute of unbroken playback) — picture confirmed, capture
> traced to the screen-share portal rather than a camera, the panel left at its
> own refresh rate, and a nine-minute mirror session held at a steady rate. On
> the Apple TV, sound switched between TV, laptop and both mid-cast, and extend
> ran at 2560x1440 in H.264 and HEVC.

## Why a rewrite

omarchy-cast is Python and installing it pulls 14 packages and a 73 MiB
runtime. The shipped feature set needs none of that: AirPlay capture belongs to
`doubletake` (itself a Go binary), and our own code is subprocess orchestration
— no cgo, no GStreamer bindings, no Python.

```
omarchy-cast   14 packages, 73 MiB runtime
castr           avahi + one backend, two static binaries, 6.5 MB total
```

The Chromecast backend costs more than that line suggests, and it is worth
being straight about it: it talks D-Bus to the screen-share portal and drives
GStreamer to capture, encode and segment. Those are runtime dependencies an
AirPlay-only user never installs, which is why they are optional rather than
required.

## Using it

```bash
castr menu                 # pick a receiver and a mode — bind this to a key
castr list                 # what is on the network
castr start <id> extend    # or mirror, which is the default
castr stop                 # stop everything
castr add 10.0.0.5 "TV"    # a receiver mDNS cannot see
castr pin                  # type the code a receiver is showing
```

**Mirror** shows this screen on the receiver. **Extend** gives you a second
desktop on it, on a virtual output named `castr` — 1920x1080 unless you set
another size (see [Stream size and codec](#stream-size-and-codec)).

Mirror does *not* change your panel's mode — a 240 Hz display stays at 240 Hz.
It captures the screen you choose at the share prompt and lets doubletake scale
it. An earlier design mirrored the panel onto a virtual output to hand the
encoder a smaller source; that output turned out to be uncapturable, because a
mirrored output is not an active monitor and the screen-share portal only offers
active monitors.

The daemon starts itself on first use and exits after fifteen minutes idle. It
stays that long on purpose — discovery is only fast while its cache is warm.

### Sound

A cast's sound plays on the receiver by default, and the laptop goes quiet.
Switch it any time, mid-cast, from the SOUND buttons in the widget or:

```bash
castr sound <id> tv        # or laptop, or both
castr sound <id>           # print the current choice
```

Chromecasts get real sound too. "Both" on a Chromecast echoes: the television
runs one to three seconds behind, so you hear everything twice. Use it on an
Apple TV, or not at all.

castr routes sound with `pactl` (the `libpulse` package), and keeps
WirePlumber's chosen output with `pw-metadata` (from `pipewire`). If it cannot
route, the cast still starts with the laptop's sound left alone, and the widget
says why: an Apple TV then plays the laptop's sound too, and a Chromecast stays
silent. The laptop's sound always comes back afterwards, even if castrd is
killed, and WirePlumber's chosen output with it.

`[airplay] audio = false` turns sound off for every receiver.

### Pairing

The first cast to an Apple TV puts a code on its screen, and the cast waits
until you type it in. Any of these takes it:

- **Click the castr notification** that appears — it opens a prompt for the code.
- **Open the cast menu again** — the first entry is *Enter the pairing code*.
- **The bar panel**, if you use the widget — it opens with a field for the code.
- **`castr pin`** — finds the receiver that is waiting and asks for its code.

The code is asked for once per receiver; after that castr remembers the pairing.

**If it asks on every cast**, check the Apple TV first: *Settings → AirPlay and
HomeKit → Require Code*. Set to **Every Time**, the television demands a new code
on each connection no matter what castr remembers — choose **First Time Only**.

**If castr refuses an Apple TV it has paired with**, the device answering did
not prove it is that Apple TV. castr will not cast to it, and will not pair
with it again by itself: a device on the network pretending to be your Apple
TV would otherwise get your screen. If you reset the Apple TV, tell castr to
pair with it again:

```bash
castr pair-again <id>      # the id from castr list, or the receiver's address
```

The next cast asks for the code the Apple TV shows. castr keeps the old
pairing until then, so a device pretending to be your Apple TV still cannot
take its place. **If no code appears on the television, enter nothing** and
stop the cast: something else answered.

## Installing

```bash
yay -S castr
```

Both backends come with the package; nothing else is needed. For hardware
encoding on Intel or AMD, also install `gst-plugin-va`.

**For an Apple TV**, `castr-airplay` speaks AirPlay. It is doubletake, built
by castr's package from [castr's branch](https://github.com/mrCode/doubletake/tree/castr)
and installed in `/usr/lib/castr`, where nothing else looks. castr ships its own
rather than using whichever doubletake is installed because doubletake upstream
does not check that an Apple TV it has paired with really is that Apple TV:
another device on the network answering at its address receives your screen.
castr's branch checks, and refuses one that cannot prove it; the fix is offered
upstream as [omarroth/doubletake#67](https://github.com/omarroth/doubletake/pull/67).
`doubletake-git` is not used.

**For a Chromecast**, nothing speaks it but castr — the capture, the encoding
and the HTTP server are all castr's own, built on GStreamer.

Your firewall has to let the receiver reach this machine. An Apple TV connects
**back** into a port range; a Chromecast fetches the stream from port 8010,
which is covered under [Chromecast](#chromecast) above.

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

On Omarchy "Quarto" and later, install the bar widget as a plugin:

```bash
omarchy plugin add https://github.com/mrCode/castr-indicator.git --enable --yes
```

Both bars are also shipped in `/usr/share/castr/`:

- **Quickshell** — `quickshell/castr-indicator/`, the same files as the plugin
  repo above.
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
# geometry = "2560x1440@60" # leave unset for 1920x1080@60; see Stream size
                            # and codec
codec = "auto"              # auto | h264 | hevc

[chromecast]
port = 8010                 # the receiver fetches the stream from this port
width = 1280                # scaled before encoding; a receiver REFUSES a
height = 800                # stream above what its decoder handles
segment_seconds = 1         # the floor on how far behind the television runs
bitrate = 4000

[airplay]
port_range = "60000-60010"  # the receiver connects back into these
# target_latency_ms = 150   # leave unset: castr uses 100 without sound and 150
                            # with sound (measured on an office Wi-Fi: 100
                            # glitched the TV's sound, 150 was almost clean).
                            # Set it to override both. NOT just a
                            # responsiveness knob: below ~80 a receiver hangs
                            # up mid-cast. Measured — an Apple TV closed the
                            # stream after ~30s at 50, and ran unbroken at
                            # 100. castr raises anything lower. Each step adds
                            # that delay to everything the TV shows.
ready_timeout = 60          # capture can begin 23s after the session is ready
                            # raise it if you need time to answer the share prompt
audio = true                # false turns sound off for every receiver, Apple TV
                            # and Chromecast alike; see Sound
```

Upgrading from omarchy-cast? Your settings and hand-added receivers are copied
across on first run. omarchy-cast is left untouched and keeps working.

### Stream size and codec

`geometry` is `WxH@R` — width 640–7680, height 360–4320, refresh 24–240. Extend
creates its output at that size, and in both modes it caps the stream: the
receiver is never sent more than `WxH`, its aspect kept, and never more than it
can take. Left unset, extend stays at 1920x1080@60 and nothing is capped.

`codec` is `auto`, `h264` or `hevc`. `auto` leaves the choice to castr-airplay,
which picks HEVC by itself only on an NVIDIA GPU. `hevc` asks the receiver for
its largest picture, up to 3840x2160, and encodes with NVENC, then VA-API
(`vah265enc`, from the `gst-plugin-va` package), then x265 in software;
`encoder` narrows that to one of them (`x264` means x265 here).

For a 4K second desktop:

```toml
[capture]
geometry = "3840x2160@60"
codec = "hevc"
```

4K needs a GPU that can encode it at frame rate. An AMD Renoir laptop iGPU
managed only 21.7 fps at 4K HEVC.

A value castr cannot use — a typo, or `auto` — is ignored with a warning in
castrd's log (`castrd -v`), and the default applies.

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

## Chromecast

```bash
castr list                 # Chromecasts appear alongside Apple TVs
castr start chromecast:<id>
```

Mirror only — extend is AirPlay-only for now.

A Chromecast works the other way round from AirPlay: castr captures the screen,
serves it as HLS over HTTP, and tells the television where to fetch it. So the
receiver has to be able to reach **this machine** on port 8010:

```bash
sudo ufw allow from <your-subnet> to any port 8010 proto tcp
```

Expect one to three seconds of delay. That is the segment length, not the
network: a live HLS receiver will not start until it has a few seconds of video
in hand, and `segment_seconds` is the floor.

The capture is scaled to 1280x800 by default. This is a decoder limit rather
than a bandwidth choice — a receiver refuses a stream above what it can decode,
with no useful error. Raise it in the config if your receiver copes.

**About the webcam bug.** omarchy-cast's Chromecast backend was disabled
because it was observed streaming the user's camera to a television while
reporting that it was mirroring the screen. That is now understood, reproduced,
and fixed — `pipewiresrc`'s `target-object` matches a node *name or serial*,
not a node id, so the source never bound and GStreamer fell back to the default
video device. The cause and the measurements are in
[docs/capture-safety.md](docs/capture-safety.md).

No source configuration fails safe, so castr does not rely on one: it reads the
PipeWire graph and confirms the pipeline is capturing the node the portal
granted **before the stream is reachable from the network**. A capture it
cannot identify is torn down and never cast.

[docs/chromecast.md](docs/chromecast.md) records the seven things a receiver
requires, six of which fail silently.

## License

MIT
