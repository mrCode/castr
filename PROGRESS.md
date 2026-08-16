# castr — port progress

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
| `internal/hypr` | **done** | virtual/mirrored outputs, cleanup, state-modelling fake |
| `internal/backend/airplay` | **partial** | argv + output scanning done; process supervision and session lifecycle NOT started |
| `internal/daemon` | not started | flock, IPC, idle watchdog, discovery waits |
| `internal/picker` | not started | omarchy-menu-select / walker |
| `internal/config` | not started | TOML |
| `cmd/castr` | not started | CLI + menu |
| `cmd/castrd` | not started | daemon entry point, real runners live here |
| `cmd/castr-tui` | not started | bubbletea; ship v1 without it if it slips |
| packaging | not started | PKGBUILD, AUR submission as a NEW package |

Scope for v1: **AirPlay only**. Chromecast is deferred — its capture path was
observed streaming the webcam, and it is the only part needing cgo.

## Build order

1. ~~discovery~~ · ~~session~~ · ~~hypr~~ · ~~doubletake argv/scanning~~
2. **airplay session lifecycle** — spawn, supervise, teardown; mirror creates a
   mirrored output, extend an independent one; separate creds per mode
3. **daemon** — flock BEFORE any cleanup sweep, then IPC, then discovery waits
4. **config** — TOML, with the migration from `~/.config/omarchy-cast`
5. **picker + cmd/castr** — CLI and menu
6. **Quickshell widget** — port `share/quickshell/`, change the plugin id
7. **TUI** — last, optional for v1
8. **packaging** — PKGBUILD, then AUR

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
- [ ] Never switch the panel's mode; virtual output first, panel switch only as
      fallback (rule lives in the session lifecycle, step 2)
- [ ] Mirror and extend use separate portal credentials
- [ ] One daemon only — flock taken before any cleanup sweep
- [ ] Both `list` and `start` wait for discovery on a cold daemon; the wait keys
      on mDNS having answered, not on "anything known"
- [ ] `start` waits longer than `list` (~12s vs ~3s), measured from daemon start
- [ ] Daemon stays resident ~15 min so the discovery cache is not discarded
- [ ] Ready timeout ≥60s and configurable (capture began 23s after ready)
- [ ] Only notify failures nobody is waiting on; one banner per event
- [ ] `stop` must not report success while an output remains
- [ ] Terminate the child's process GROUP, or capture pipelines outlive it
- [ ] Cross-subnet casting works — never advise switching networks
- [ ] Migrate state from `~/.config/omarchy-cast` on first run

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

## Environment facts

- Omarchy "Quarto": Quickshell replaced waybar **and** walker. `walker` is not
  installed; `omarchy-menu-select` / `omarchy-menu-input` are the pickers.
- Test receiver: Meeting Room, `AppleTV11,1`, usually `10.10.10.231` — reachable
  cross-subnet from the `172.26.x` network.
- The laptop panel has only `2560x1600@240` and `@60` — no native 1080p. This is
  why forcing the panel to 1080p cost 240Hz→60Hz.
- `doubletake-git` is required; 0.4.0 cannot capture on Hyprland.
- Firewall needs UDP 5353 (mDNS) and TCP/UDP 60000-60010 from the receiver.
