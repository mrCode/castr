# castr — port progress

> **Next session starts here:** confirm extend captures its own output.
> `castr start airplay:10.10.10.231 extend`, then in the share dialog pick
> **(1920x1080) (castr)** — NOT eDP-2 — and check `pw-dump` reports a
> 1920x1080 capture. If it does, extend is verified and the only thing left
> before an AUR submission is the rest of the checklist below.
>
> The test receiver 10.10.10.231 is already paired (no PIN). "meeting room-2"
> is NOT paired and never displays its code — avoid it.

Living status of the Go rewrite. **Read this first** in a new session, before
`docs/design.md`. Update it in the same commit as the work it describes.

Reference implementation: `~/workspace/omarchy-cast` (Python, v0.3.0, works, on
the AUR). Port *from* it; do not delete it until castr reaches parity and has
cast to a real Apple TV.

## Where things stand

| package | status | notes |
|---|---|---|
| `internal/discovery` | **done** | avahi-browse parsing, real captured fixtures |
| `internal/session` | **done** | state machine, injected clock |
| `internal/hypr` | **done** | outputs via `hl.monitor` eval; un-mirror before remove |
| `internal/backend/airplay` | **done** | argv, output scanning, and the full session lifecycle |
| `internal/daemon` | **done** | flock, registry, commands, idle watchdog, unix-socket IPC |
| `internal/picker` | **done** | omarchy-menu-select / walker, both calling conventions |
| `internal/config` | **done** | TOML, manual-device store, migration from omarchy-cast |
| `internal/ui` | **done** | menu entries, id parsing, bar indicator payload |
| Quickshell panel | **done** | real shell panel: mode pills, filled rows, TVs first |
| `internal/client` | **done** | socket client, daemon auto-spawn |
| `internal/cli` | **done** | every command, and the menu flow |
| `cmd/castr` | **done** | wires the real effects; two static binaries, 6.5 MB |
| `internal/notify` | **done** | the notification policy |
| `cmd/castrd` | **done** | daemon entry point; owns lock-order, process groups, notify |
| `cmd/castr-tui` | not started | bubbletea; ship v1 without it if it slips |
| plugin repo | **created** | ~/workspace/castr-indicator — manifest at root, for the marketplace |
| packaging | **written** | PKGBUILD + install notes; NOT yet submitted to the AUR |

Go module dependencies: `github.com/BurntSushi/toml` only (no transitive deps).
Package dependencies are unchanged: avahi, plus doubletake as an optdepend.

Scope for v1: **AirPlay only**. Chromecast is deferred — its capture path was
observed streaming the webcam, and it is the only part needing cgo.

## Build order

1. ~~discovery~~ · ~~session~~ · ~~hypr~~ · ~~doubletake argv/scanning~~
2. ~~airplay session lifecycle~~
3. ~~daemon~~
4. ~~config~~
5. ~~picker + cmd/castr~~
6. ~~cmd/castrd~~
7. ~~Quickshell widget~~ — ported, plugin id changed to `castr.indicator`
8. **HARDWARE VERIFICATION** — mirror PASSES; extend's capture source is the
   one thing left to confirm (see the findings at the top)
9. **packaging** — PKGBUILD written; AUR submission after hardware passes
10. **TUI** — optional, after v1

## Invariants — every one is a bug already shipped and fixed

Ticked items have a test that fails if the rule is broken (mutation-checked).

- [x] Only `screen capture started` means streaming — `mirror session ready`
      arrives ~4s earlier, before capture exists
- [x] Scan accumulated text, not lines — the PIN prompt has no newline
- [x] Always `-target`, never `-daemonize` — daemon mode drops `-port-range`
- [x] Surface doubletake's own capture error rather than guessing
- [x] Cleanup never removes an output a live session owns
- [x] Cleanup removes only castr's own names, never every `HEADLESS*`
- [x] Mirror and extend own separate outputs
- [x] Every failure after creation removes the output again
- [x] Prefer the requested output name over a before/after diff
- [x] Use `monitors all` — mirrored outputs are absent from the plain listing
- [x] `Failed` reachable from anywhere; leaving it clears the reason
- [x] Active states keep the idle watchdog from exiting under a live cast
- [x] Never switch the panel's mode; virtual output first, panel switch only as
      fallback — and the mirrored output must really mirror the panel, not just
      be named for it
- [x] A fallback panel switch is restored on teardown, and only then
- [x] Mirror and extend use separate portal credentials
- [x] One daemon only — flock, exclusive, released by process death so a crash
      cannot lock out successors; `cmd/castrd` must call it BEFORE the sweep
- [x] Both `list` and `start` wait for discovery on a cold daemon; the wait keys
      on mDNS having answered, not on "anything known" — and a FAILED browse is
      not an answer
- [x] `start` waits longer than `list` (~12s vs ~3s), measured from daemon start
- [x] Daemon stays resident ~15 min so the discovery cache is not discarded
- [x] A displaced session record is restored only on a refusal, never on a
      failed restart
- [x] An unacknowledged (idle) session is neither reported as a cast nor
      stoppable
- [x] Manual receivers survive every browse and outrank stale discovered records
- [x] The socket is 0600 and every request gets a reply, malformed ones included
- [x] Ready timeout ≥60s by default (capture began 23s after ready) and
      configurable
- [x] Only notify failures nobody is waiting on; one banner per event — and
      only failures are urgent, because urgent means sticky on mako
- [x] The picker is probed, never hardcoded — Quarto removed walker, and a
      hardcoded launcher turns a system update into a broken keybind
- [x] Omarchy's menu takes options as ARGUMENTS, walker takes them on stdin
- [x] Cancelling a menu is not a failure
- [x] The bar indicator stays visible in every state, and `castr bar` never
      spawns a daemon and never exits non-zero
- [x] A manual receiver's id is keyed on its ADDRESS alone, so re-adding
      updates rather than duplicating
- [x] Stopping is reachable from the menu, listed first
- [x] An "ok" with no device list is an error, never an empty network
- [x] `stop` must not report success while an output remains
- [x] A crash before streaming never overwrites the startup outcome; a deliberate
      stop is never reported as a crash; a crash mid-stream always is
- [x] Terminate the child's process GROUP, or capture pipelines outlive it —
      verified against REAL processes in cmd/castrd, since no fake can show it
- [x] A fallback panel switch is remembered on disk, so a daemon killed
      mid-cast restores the panel on its next start
- [ ] Cross-subnet casting works — never advise switching networks
- [x] Migrate state from `~/.config/omarchy-cast` on first run — COPY, never
      move: omarchy-cast still works and must keep working
- [x] A config file that sets one key keeps the defaults for the rest
- [x] Manual receivers persist across daemon restarts; a corrupt store is
      ignored rather than fatal; saves are atomic

## Testing rules

- **Every external command is injected.** `exec.Command` never appears outside
  `cmd/`. The Python suite escaped its sandbox four times: switched the real
  monitor mode, wrote a fake receiver into the user's device store, created real
  Hyprland outputs, and opened a desktop dialog that hung the run.
- **Fakes model state, not calls.** `internal/hypr`'s fake actually adds and
  removes monitors. A call-recording stub passes while the bug remains.
- **Mutation-check anything load-bearing.** Break the rule, confirm a named test
  fails, restore. Several tests here were green against broken code until this
  was done.
- **Hardware verification is a checklist, not a test.** Before any release:
  picture appears, capture traces to the portal's screen node (not a camera,
  via `pw-dump` link tracing), panel keeps its mode, teardown leaves nothing.

## Hardware findings, 2026-08-17 — READ THIS FIRST

A day at a real Apple TV overturned a design assumption and found six bugs.
None of it was visible from the test suite.

**Hyprland 0.56.2 (the Quarto update, installed 2026-08-16) broke three things,
and all of them exit 0 while failing:**

| call | what it does now |
|---|---|
| `hyprctl keyword monitor <cfg>` | refused: "keyword can't work with non-legacy parsers. Use eval." |
| `hyprctl output remove <mirrored>` | "output not found" for an output `monitors` is listing |
| `hyprctl output create headless <existing>` | "Name already taken" |

The replacements: `hyprctl eval 'hl.monitor({...})'` for configuration, and
un-mirroring (`mirror = "none"`) BEFORE removing. A mutating hyprctl command
answers exactly `ok` when it worked; anything else is a failure regardless of
exit status. omarchy-cast used the same two broken calls, so mirror mode was
silently broken there too from the day Hyprland updated.

**MIRROR NO LONGER CREATES AN OUTPUT, and this is the big one.** It used to
create a virtual output mirroring the panel. The screen-share portal never
offered it: a mirrored output is not an ACTIVE monitor (absent from `hyprctl
monitors`, which is why this package reads `monitors all`), and the portal
enumerates active monitors only. Verified by opening the picker with the
mirrored output live — one source listed, the panel — and again with an
independent output — two sources, including ours. So:

- **mirror** creates nothing and captures the panel. doubletake scales it. This
  is what was happening all along; castr now asks for it honestly.
- **extend** keeps its independent output, which the picker DOES offer.
- The panel-switch fallback is gone. Nothing could reach it.

**The user's machine also had a broken screen-share picker.**
`~/.config/hypr/xdph.conf` set `custom_picker_binary = hyprland-preview-share-picker`
(0.2.1), which starts but maps NO window — every screen-share request hung
invisibly. Commented out (backup in the session scratchpad); the stock picker
works. This is a system problem, not castr's, and it would break OBS too.

**`castr reset-share [mode]`** is new: the portal remembers the first choice
forever, and picking your own screen instead of castr's output is easy and
invisible. This forgets the grant without touching the pairing keys.

**Still open:** an extend cast that captures the `castr` output rather than the
panel has NOT been confirmed — the share dialog was never answered in time. Pick
`(1920x1080) (castr)`, not `eDP-2`, and check the capture is 1920x1080.

## Verified end to end on this machine (2026-08-16)

Both binaries built and run against the real system, in an isolated
XDG_STATE_HOME:

- discovery found five real receivers via avahi, including an AppleTV14,1
- a second `castrd` was refused by the lock
- add / list / forget / forget-again all behaved
- `castr bar` answered with no daemon and started none
- `quit` shut down cleanly and removed its socket

**2026-08-17: an actual cast now works.** Mirror to an Apple TV, from the bar
panel, verified streaming with the picture on screen, the panel still at
2560x1600@240, capture traced through `pw-dump` to the portal (no camera), and
clean teardown. A mid-stream network drop was detected and reported correctly.

**Older note, kept for the checklist below:** doubletake is installed and receivers are
reachable, but starting a cast takes over the user's screen, so it needs their
say-so. Everything else is done; this is the last gate before a release.

### The hardware checklist, in order

1. `castr menu` -> pick a receiver -> Mirror. Picture appears on the TV.
2. `hyprctl monitors` shows the PANEL still at 2560x1600@240. This is the
   regression that cost 240Hz once already.
3. `pw-dump` link trace confirms capture comes from the portal's screen node,
   NOT a camera. Never trust the picture alone -- omarchy-cast looked right
   while streaming the webcam.
4. Stop. `hyprctl monitors all` shows no `castr*` output left behind.
5. Repeat for Extend: a second desktop appears, windows can be moved to it.
6. Kill `castrd` with SIGKILL mid-cast, then start it again: no stray outputs,
   no leftover doubletake, panel mode intact.
7. `pgrep -f gst` after a stop: nothing. Capture pipelines must not outlive
   their parent.

## Notes for the next step


`internal/backend/airplay` is pure logic: every external effect is a func field
on `Backend` (`Hypr`, `Spawn`, `Creds`, `Emit`, `SwitchDisplay`,
`RestoreDisplay`). `cmd/castrd` supplies the real ones. Two of them carry
invariants the package cannot enforce itself:

- `Spawn` must set `Setpgid` and `Terminate` must signal the process GROUP
  (`syscall.Kill(-pid, ...)`), or doubletake's capture pipelines outlive it.
- `Emit` is where the notification rules live: only failures nobody is waiting
  on, one banner per event.

## Environment facts

- Omarchy "Quarto": Quickshell replaced waybar **and** walker. `walker` is not
  installed; `omarchy-menu-select` / `omarchy-menu-input` are the pickers.
- Test receiver: Meeting Room, `AppleTV11,1`, usually `10.10.10.231` — reachable
  cross-subnet from the `172.26.x` network.
- The laptop panel has only `2560x1600@240` and `@60` — no native 1080p. This is
  why forcing the panel to 1080p cost 240Hz→60Hz.
- `doubletake-git` is required; 0.4.0 cannot capture on Hyprland.
- Firewall needs UDP 5353 (mDNS) and TCP/UDP 60000-60010 from the receiver.
