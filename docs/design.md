# castr — Go rewrite design

**Date:** 2026-08-16
**Status:** Draft for review
**Supersedes:** the Python implementation in `omarchy-cast` (kept as reference)

## Summary

Rewrite `omarchy-cast` in Go as **`castr`**: a single static binary that mirrors
or extends a Hyprland desktop to an AirPlay receiver.

The Python version works and is verified on hardware, but installing it pulls
**14 packages** and a 73 MiB Python runtime, and a user hit real friction doing
it. The shipped feature set needs none of that.

## Why this is worth doing

The decisive fact is what the *shipped* code actually touches. AirPlay capture
belongs to `doubletake`, which is itself a Go binary we supervise. Our own code
is orchestration:

| responsibility | how |
|---|---|
| discovery | parse `avahi-browse -rtp` |
| capture + encode + AirPlay | supervise `doubletake` |
| virtual outputs, display modes | `hyprctl` |
| daemon, IPC, session state, menu, TUI | our own code |

Nine subprocess call sites, no GStreamer bindings, no D-Bus. In Go that is
plain `os/exec` — no cgo.

**GStreamer and xdg-desktop-portal are only used by the Chromecast path**
(`capture/` + `cast.py`, 723 lines), which is disabled. Excluding it removes the
only part that would need cgo bindings.

Dependency count, measured on the current package:

```
now:    python, python-zeroconf, python-gobject, python-textual, gst-python,
        gstreamer, gst-plugins-{base,good,bad}, gst-plugin-{pipewire,va},
        xdg-desktop-portal, pipewire, avahi          (14)
castr:  avahi        + doubletake (optdepend)        (1-2)
```

A single binary also removes the class of problem the user's friend hit: no
interpreter, no site-packages, no PEP 668, no pip.

## Scope

### v1 — AirPlay only

Everything the Python version ships and has verified on hardware:

- discovery via avahi, with devices added by address remembered on disk
- **mirror** and **extend**, both capturing a virtual 1920x1080 output so the
  panel keeps its resolution and refresh rate
- daemon with single-instance locking, unix-socket IPC, idle timeout
- CLI: `list`, `status`, `start`, `stop`, `pin`, `forget`, `menu`, `waybar`
- desktop menu via `omarchy-menu-select` / `walker`
- Quickshell bar widget (and the waybar module, unchanged — it is just JSON)
- TUI

### Deferred — Chromecast

Not in v1, and not merely for effort. **The Cast capture path was observed
streaming the webcam instead of the screen** and is disabled in the Python
version; porting it would port that. It is also the only component needing
GStreamer and portal bindings in Go (`go-gst`, `godbus`), which is where the
cgo-free property is lost.

Cast returns when the capture bug is fixed and proven with a PipeWire link
trace. The `Backend` interface is designed for it so adding it is additive.

### Non-goals

- Protocols beyond AirPlay and (later) Cast
- Reimplementing AirPlay itself; `doubletake` stays the transport
- Supporting compositors other than Hyprland for extend (mirror may work more
  widely; extend needs `hyprctl output create`)

## Naming and migration

`omarchy-cast` → **`castr`**. Binaries: `castr`, `castr-tui`, `castrd`.

| moves | from | to |
|---|---|---|
| config | `~/.config/omarchy-cast/config.toml` | `~/.config/castr/config.toml` |
| state | `~/.local/state/omarchy-cast/` | `~/.local/state/castr/` |
| socket | `/run/user/$UID/omarchy-cast.sock` | `/run/user/$UID/castr.sock` |
| plugin id | `omarchy-cast.indicator` | `castr.indicator` |

**On first run, castr migrates automatically**: if the new state directory does
not exist and the old one does, copy it and log the move. This carries over
pairing credentials, portal restore tokens and remembered devices — losing them
means re-pairing every Apple TV and re-answering every share dialog, which is
the kind of "upgrade tax" that makes people stop upgrading.

AUR package names are immutable, so `castr` is submitted as a new package and
`omarchy-cast` is updated one final time with a `pkgdesc` pointing at it.

## Architecture

```
cmd/castr        CLI + menu
cmd/castrd       daemon
cmd/castr-tui    TUI (bubbletea)
internal/
  daemon/        session registry, IPC server, idle watchdog, single-instance lock
  session/       state machine (idle/connecting/awaiting_pin/streaming/failed)
  discovery/     avahi-browse parsing; remembered devices
  backend/       Backend interface; airplay implementation
  hypr/          hyprctl: monitors, virtual outputs, mirrored outputs
  picker/        omarchy-menu-select / walker
  config/        TOML
```

The daemon owns all state; every client — CLI, TUI, bar widget — is a thin IPC
caller. This is the current design and it has held up: the TUI, CLI, menu and
indicator always agree because there is one source of truth.

**IPC stays line-delimited JSON over a unix socket**, with the same command
names and response shape. The Quickshell widget and waybar module poll
`castr waybar` and neither needs to change beyond the binary name.

## What the rewrite must not lose

Every item below is a bug this project already shipped and fixed. Each becomes a
test in the Go suite. This section is the most important part of the spec: a
rewrite that rediscovers these has cost more than it saved.

**Single instance.** Nothing enforced one daemon; `serve()` unlinked and rebound
the socket, so a second daemon started and swept the *live* virtual output of a
cast running in the first process. The owning daemon logged nothing, because the
damage happened elsewhere. Take an flock **before** any cleanup sweep.

**Never sweep an output a live session owns.** Cleanup takes the set of
in-use output names and skips them.

**Mirror and extend use separate outputs and separate credentials**
(`castr-mirror`, `castr`). Sharing a name meant one session's teardown killed
the other's; sharing credentials meant replaying a restore token that pointed at
the real panel, silently capturing the panel instead.

**Never switch the panel's mode to get a 1080p source.** A laptop with only
`2560x1600@240` and `@60` gets 1080p synthesised at 60Hz, and the user types on
a screen four times slower while blaming the cast. Capture a mirrored virtual
output instead. Keep the panel switch only as a fallback when no virtual output
can be created.

**Discovery must ask avahi, not run a second mDNS stack.** A cold browser took
15.7s for its first result and found one receiver in ninety seconds; avahi,
warm since boot, listed six instantly.

**`avahi-browse -rtp`'s `proto` field is the mDNS transport, not the address
family.** It reads `IPv6` on lines carrying an IPv4 address. Validate the
address; filtering on that field finds nothing.

**Both `list` and `start` wait for discovery on a cold daemon**, and the wait
keys on mDNS having answered — not on "anything known", which a remembered
device satisfies instantly. `start` waits longer than `list`, since the user has
already committed to a receiver.

**The daemon stays resident** (15 min idle) so the discovery cache is not thrown
away.

**Report the real failure.** doubletake prints its own portal errors; surface
them rather than guessing. Check the routing table before blaming the network,
and never claim a cause that was not checked. Cross-subnet casting **works** —
do not tell users otherwise.

**Only announce failures nobody is waiting on.** A failed `start` is already
returned to the caller; notifying as well produced two sticky banners. Reserve
urgent notifications for a cast that dies mid-stream.

**Never report success without the effect.** `stop` must not answer OK while a
virtual output remains; a session must not report STREAMING before pixels flow
(wait for `screen capture started`, not `mirror session ready`, which fires ~4s
earlier).

**`doubletake -daemonize` drops `-port-range`.** Always run `-target` directly,
one child process per session, in its own process group so terminating it takes
its capture pipelines with it.

**Ready timeout ≥ 60s, configurable.** Capture began 23s after session-ready on
real hardware.

## Testing

Go's standard `testing`, table-driven, with the same discipline that caught
these bugs:

- **Every external command is injected.** `exec.Command` is behind an interface;
  no test shells out to `hyprctl`, `avahi-browse`, `doubletake`, or a menu.
- **A test must not touch the real machine.** The Python suite escaped its
  sandbox four times — switching the developer's monitor mode, writing a fake
  receiver into their device store, creating real Hyprland outputs, and opening
  a desktop dialog that hung the run. Go has no autouse fixtures, so this is
  enforced by construction: the real runners live in `main`, and packages under
  test only ever see injected ones.
- **Hardware verification is a checklist, not a test.** `docs/verification.md`
  lists what must be confirmed against a receiver before a release: the picture
  appears, the capture source traces to the portal's screen node rather than a
  camera, the panel keeps its mode, teardown leaves nothing.

Target: parity with the current suite's *coverage of behaviour*, not its count.

## Packaging

```
depends:    avahi
optdepends: doubletake-git (AirPlay), a menu (omarchy or walker), libnotify
makedepends: go
```

`go build -trimpath -ldflags "-s -w"` produces one static binary per command.
No interpreter, no site-packages. A GitHub release can also carry the binaries
directly, so non-Arch users can download and run.

## Risks

**Rewrites lose hard-won behaviour.** Mitigated by the section above, which
turns each past bug into a required test, and by keeping the Python version
tagged and archived as an executable reference.

**The TUI is the largest genuinely new work.** Textual gives a lot for free;
bubbletea is lower level. It is also the least-used surface — if it slips, ship
v1 without it rather than delay.

**doubletake remains an external dependency** with the same version sensitivity
(`0.4.0` cannot capture on Hyprland; `-git` is required). Unchanged by the
rewrite, and worth stating in the README rather than discovering again.

**No Chromecast at v1** means a feature listed on the AUR page today disappears
from the new package. Since it is disabled and non-functional, the honest
framing is that castr ships what works.

## Open question

This spec assumes **AirPlay only for v1**, with Cast deferred until its capture
bug is fixed. If Cast is wanted in v1, the scope grows by `go-gst` and `godbus`
bindings, cgo, and a known privacy defect that must be fixed first — say so and
this document needs revising before any code is written.
