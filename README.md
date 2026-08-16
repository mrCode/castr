# castr

Mirror your Hyprland desktop to an Apple TV — or extend onto it as a second
monitor — from your desktop's menu, with a bar indicator.

A single static binary. `avahi` for discovery, `doubletake` for AirPlay,
`hyprctl` for outputs; nothing else at runtime.

> **Status: in development.** This is a Go rewrite of
> [omarchy-cast](https://github.com/mrCode/omarchy-cast), which works and is
> verified on hardware. Use that until castr reaches parity.

## Why a rewrite

omarchy-cast is Python and installing it pulls 14 packages and a 73 MiB
runtime. The shipped feature set needs none of that: AirPlay capture belongs to
`doubletake` (itself a Go binary), and our own code is subprocess orchestration
— nine call sites, no GStreamer bindings, no D-Bus, no cgo.

```
omarchy-cast   14 packages
castr           1 package (avahi) + doubletake
```

Status and next steps live in [PROGRESS.md](PROGRESS.md).

See [docs/design.md](docs/design.md), whose most important section is *"What
the rewrite must not lose"* — every bug the Python version already shipped and
fixed, restated as a required test so this one does not rediscover them.

## Building

```bash
go build ./cmd/castr
go test ./...
```

## License

MIT
