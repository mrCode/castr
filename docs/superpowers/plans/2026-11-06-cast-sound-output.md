# Choosing where a cast's sound plays — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Each cast chooses whether its sound plays on the receiver (TV), on the laptop, or on both. The default is TV. The choice can be switched mid-cast from the widget and the CLI. It works the same on Apple TV and Chromecast, and the laptop's sound always comes back.

**Architecture:**
- The routing is done with pactl, against PipeWire's pulse server. While any cast with sound is running, castrd creates a virtual output `castr_mix` and makes it the default, so all laptop sound lands there.
- Each cast gets its own virtual output, `castr_cast_<n>`, fed from `castr_mix.monitor` by a loopback when that cast is on TV or Both. The helper and the Chromecast pipeline capture `castr_cast_<n>.monitor`.
- A loopback from `castr_mix.monitor` to the real output exists while any cast is on Laptop or Both.
- Switching only loads or unloads loopbacks, so the picture never restarts.
- A state file and a detached guard process undo everything if castrd dies.

**Tech Stack:**
- Go: castr, plus the helper fork `~/workspace/doubletake` on branch `castr`.
- `pactl` (libpulse) against pipewire-pulse.
- GStreamer: `pulsesrc`.
- QML: the Quickshell widget.

**Spec:** `docs/superpowers/specs/2026-11-06-cast-sound-output-design.md`

## Facts measured on this machine (2026-11-06)

Read these before writing any routing code:

- **Muting a sink, or setting it to 0%, silences its `.monitor` too** (peak 0 or no data). Never mute or turn down a sink that is being captured. Muting the real output is therefore not a way to silence the laptop.
- **A `module-null-sink` monitor carries what is played into it** (peak 8000 from an 8000-peak tone). When nothing plays, it still delivers a continuous stream of zeros.
- **`module-loopback source=<a>.monitor sink=<b>` copies the sound into `<b>`.** Measured peak 8000 on `<b>.monitor`.
- **WirePlumber remembers each sink's volume and mute by name, across loads.** A null sink once set to 0% came back at 0%. So after creating each castr sink, always set `set-sink-volume <name> 100%` and `set-sink-mute <name> 0`.
- **A fresh `parec` or `pulsesrc` can deliver nothing for its first ~2 s** while the graph settles. Tests against real pactl must allow for that.

## Global Constraints

These come from the spec, which every task implicitly includes:

- **Choices are exactly `tv`, `laptop` and `both`.** The CLI and protocol use these lowercase values. The widget labels are **TV**, **Laptop** and **Both**.
- **The default is `tv` at the start of every cast.** It is never remembered for the next cast.
- **The choice covers all laptop sound, not one app's.** Per-app routing is out of scope.
- **"Laptop" is whatever the laptop is playing through at that moment.** If the default output changes during a cast, castr adopts the new output as the laptop.
- **How several casts combine:**
  - A receiver gets sound if its cast is `tv` or `both`.
  - The laptop plays if ANY cast is `laptop` or `both`.
  - When the last cast ends, everything is restored to how it was before the first cast.
- **Anti-goals:**
  - The laptop must never be left silent: not after a stop, a hang-up, a helper crash, castrd being killed, or logout.
  - The default device and its volume must never be left changed.
  - Switching must never restart the picture.
  - Apple TV sound must not regress.
  - No stray castr sinks or loopbacks may be left behind.
- **Sound off means off everywhere.** The `[airplay] audio = false` setting now turns sound off for Chromecasts too. castr then touches nothing, receivers get silence (a Chromecast keeps its silent AAC track), and the session reports no sound choice.
- **If routing is impossible,** for example pactl fails or there is no default sink:
  - The cast still starts with sound on both, as today.
  - The session reports no choice, plus a one-line note saying why.
- **`libpulse` becomes a hard `depends`** in `packaging/PKGBUILD`.
- **Widget rules (marketplace review):**
  - Only plain buttons that run `castr` commands with fixed arguments.
  - No editable text inputs and no clipboard.
  - Receiver names are always `Text.PlainText`.
- **Extend behaves exactly like mirror.**
- **Releasing is out of scope.** The work ends at a built, tested and hardware-checked tree. The helper change needs a new `castr-airplay-2` tag at release time, which is not part of this plan.

## Review Focus

Most likely to bite first. Each line has its test in the owning task:

1. **The saved default sink is gone at restore** (Bluetooth disconnected mid-cast). Restore must not set a nonexistent default, must still unload every castr module, and must leave WirePlumber's choice in place. Test: Task 1 `TestRestoreWhenTheSavedSinkIsGone`.
2. **The user switches output mid-cast** (`get-default-sink` no longer returns `castr_mix`). castr adopts the new sink as the laptop output, moves the laptop loopback there, sets the default back to `castr_mix`, and moves that sink's app streams. It must not loop, and must not move its own loopback stream. Test: Task 1 `TestFollowAdoptsANewDefaultOutput`.
3. **A cast fails during start, or two casts end in either order.**
   - Detach runs exactly once per attach.
   - The last detach restores.
   - A failed start leaves nothing behind.

   Tests: Task 5 `TestSoundIsDetachedWhenStartFails` and `TestTheLastCastToEndRestoresSound`.
4. **castrd is killed with SIGKILL mid-cast.** The guard restores within a second or two. The next castrd start also restores, if the guard itself was gone. Tests: Task 2 `TestTheGuardRestoresWhenTheDaemonDies`, plus Task 1 `TestRecoverRestoresAPendingRouting`.
5. **WirePlumber remembered 0% or mute for a castr sink name.** Every castr sink is set to 100% and unmuted right after it is created. Test: Task 1 `TestEveryCastrSinkIsSetToFullVolumeAndUnmuted`.

---

## File structure

**castr** (`~/workspace/castr`):
- **Create `internal/sound/`.** Each file has one job:
  - `choice.go`: the `Choice` type and parsing.
  - `pactl.go`: the parsed pactl calls behind a `Runner`.
  - `router.go`: the routing state machine.
  - `state.go`: the crash-safe state file plus `Recover`.
  - `guard.go`: the detached guard.
  - Tests sit beside each file.
- **Modify:**
  - `internal/daemon/{protocol.go,server.go,daemon.go}` for the command, the `SessionJSON` fields, and the attach/detach hooks.
  - `internal/client/client.go`.
  - `internal/cli/cli.go` for the `sound` subcommand.
  - `cmd/castrd/main.go` for recovery, the router, the guard, the follow ticker and shutdown.
  - `internal/backend/airplay/{doubletake.go,session.go}`.
  - `internal/capture/pipeline.go` and `internal/backend/chromecast/{backend.go,live.go}`.
  - `share/quickshell/castr-indicator/Widget.qml`.
  - `internal/ui/widget_test.go`.
  - `packaging/PKGBUILD`.
  - `README.md`.

**helper** (`~/workspace/doubletake`, branch `castr`): modify `cmd/doubletake/main.go` (flag) and `internal/airplay/audio.go` (an explicit source).

---

### Task 1: `internal/sound` — choices, pactl, router, state file

**Files:**
- Create: `internal/sound/choice.go`, `internal/sound/pactl.go`, `internal/sound/router.go`, `internal/sound/state.go`
- Test: `internal/sound/router_test.go`, `internal/sound/choice_test.go`

**Interfaces:**
- **Produces, choices:**
  - `type Choice string` with constants `TV Choice = "tv"`, `Laptop Choice = "laptop"`, `Both Choice = "both"`.
  - `func ParseChoice(s string) (Choice, error)`. It is case-insensitive and rejects anything else with an error naming the three choices.
- **Produces, pactl:**
  - `type Runner func(name string, args ...string) (string, error)`, the same shape as `hypr.Runner`.
- **Produces, the router:**
  - `type Router struct`; `func NewRouter(run Runner, stateDir string) *Router`.
  - `func (r *Router) Attach(castID string) (source string, err error)`:
    - On the first attach, it records the current default sink, creates `castr_mix` (description `castr`) and makes it the default.
    - It moves every app sink-input onto `castr_mix`.
    - It creates `castr_cast_<n>` (description `castr cast`) plus the loopback `castr_mix.monitor → castr_cast_<n>`.
    - The cast's choice starts as `TV`.
    - It returns `"castr_cast_<n>.monitor"`.
  - `func (r *Router) Set(castID string, c Choice) error`
  - `func (r *Router) Choice(castID string) (Choice, bool)`
  - `func (r *Router) Detach(castID string) error`: the last detach calls `Restore`.
  - `func (r *Router) Follow() error`: called every second by the daemon while anything is attached.
  - `func (r *Router) Restore() error`
  - `func Recover(run Runner, stateDir string) error`: restores routing left by a dead daemon.
- **Produces, the state file:** `const StateFile = "sound.json"`, in stateDir.

**pactl usage** (exact argv; parse the outputs as shown):
- **Queries:**
  - `pactl get-default-sink` returns a sink name.
  - `pactl list short sinks` returns lines `id\tname\t...`.
  - `pactl list short modules` returns lines `id\tname\targs`.
  - `pactl --format=json list sink-inputs` returns objects with `"index"`, `"sink"` and `"owner_module"`. App streams are those whose `owner_module` is not a castr module.
- **Creating outputs and loopbacks:**
  - `pactl load-module module-null-sink sink_name=<name> sink_properties=device.description=<desc>` prints the module id.
  - `pactl load-module module-loopback source=<src> sink=<sink> latency_msec=30` prints the module id.
- **Changing state:**
  - `pactl set-sink-volume <name> 100%`
  - `pactl set-sink-mute <name> 0`
  - `pactl set-default-sink <name>`
  - `pactl move-sink-input <index> <sink>`
  - `pactl unload-module <id>`
- **A castr module is any module whose args contain `castr_mix` or `castr_cast_`.** That is how `Restore` and `Recover` find leftovers, even with no state file.

**State file** (`sound.json`): `{"previous_default": "<sink name>", "laptop_sink": "<sink name>"}`.
- It is written before the default is changed.
- It is updated when `Follow` adopts a new laptop sink.
- It is deleted at the end of `Restore`.
- Write it atomically (temp file plus rename), as `config.SaveDevices` does.

**Restore order:**
1. Unload the loopbacks.
2. Set the default back to `previous_default`, but only if `list short sinks` still has it.
3. Move the sink-inputs on `castr_mix` to the new default.
4. Unload the castr sinks.
5. Delete the state file.

Every step runs even if an earlier one fails. Return the joined errors.

- [ ] **Step 1: Write the failing tests** in `router_test.go`, using a fake `Runner`.
  - The fake keeps sinks, modules, sink-inputs (with owners) and the default in memory, applies the pactl commands above, and records every call.
  - `TestAttachRoutesTheLaptopIntoCastrAndTheCastStartsOnTV`:
    - After `Attach("airplay:A")`, the default is `castr_mix`.
    - App inputs are on `castr_mix`.
    - A loopback `castr_mix.monitor→castr_cast_1` exists.
    - No loopback to the real sink exists.
    - The returned source is `castr_cast_1.monitor`.
    - `Choice` is `TV`.
  - `TestEveryCastrSinkIsSetToFullVolumeAndUnmuted`: every created castr sink gets `set-sink-volume <name> 100%` and `set-sink-mute <name> 0`, and no real sink ever gets a volume or mute command.
  - `TestTheChoicesCombineAcrossCasts`, with A and B attached:
    - A=`laptop`, B=`tv`: the laptop loopback exists, A's cast loopback is gone, B's is kept.
    - Both set to `tv`: no laptop loopback.
    - A=`both`: A's cast loopback exists, and the laptop loopback exists.
  - `TestSwitchingNeverTouchesTheCastSinks`: `Set` only loads or unloads loopbacks. It never unloads a `castr_cast_<n>` sink, and never changes the default.
  - `TestTheLastDetachRestoresEverything`, after A and B attach and both detach in either order:
    - The default is the original.
    - App inputs are on the original.
    - No castr module remains.
    - The state file is gone.
  - `TestRestoreWhenTheSavedSinkIsGone`:
    - Remove the original sink from the fake before detaching.
    - Restore issues no `set-default-sink` to the missing name.
    - Restore unloads every castr module.
    - Restore deletes the state file.
  - `TestFollowAdoptsANewDefaultOutput`:
    - With A on `laptop`, the fake's default changes to `bluez_x`, which has a new app input.
    - After `Follow`, the laptop loopback goes to `bluez_x`.
    - The default is `castr_mix` again.
    - The app input moved to `castr_mix`.
    - The loopback's own sink-input was not moved.
    - `laptop_sink` in the state file is `bluez_x`.
    - A second `Follow` makes no calls.
  - `TestFollowGivesUpWhenCastrMixDisappears`: if `castr_mix` is no longer in `list short sinks`, `Follow` runs `Restore` and returns an error saying the routing was lost.
  - `TestRecoverRestoresAPendingRouting`: with a state file plus leftover castr modules in the fake, `Recover` restores as above. With no state file and no castr modules, it makes no changing calls.
  - `TestAttachFailsCleanlyWithoutADefaultSink`: when `get-default-sink` fails or returns an empty name, `Attach` returns an error and leaves no module or state file behind.
- [ ] **Step 2:** Write `choice_test.go`: `TestParseChoice` accepts `tv`, `TV`, `laptop` and `both`, and rejects `""` and `speaker`.
- [ ] **Step 3:** Run `go test ./internal/sound/`. Expected: FAIL (package or functions undefined).
- [ ] **Step 4: Implement** `choice.go`, `pactl.go`, `state.go` and `router.go` with the signatures above.
  - The router holds a `sync.Mutex`.
  - It keeps a `castID → {n, sinkModule, loopModule (0 if none), choice}` map plus the mix module, the laptop loopback module, the previous default and the laptop sink.
  - `n` increments per attach and is never reused within a process.
- [ ] **Step 5:** Run `go test ./internal/sound/` and `go vet ./...`. Expected: PASS. Mutation-test the rules: drop the volume/unmute call, drop the `owner_module` exclusion, drop the "only if the sink still exists" check. Each must make a test fail.
- [ ] **Step 6: Commit:** `Route a cast's sound through castr's own outputs`.

---

### Task 2: the guard that restores sound if castrd dies

**Files:**
- Create: `internal/sound/guard.go`
- Modify: `cmd/castrd/main.go` (a `--sound-guard` entry point at the top of `main`)
- Test: `internal/sound/guard_test.go`

**Interfaces:**
- **Consumes:** `Recover(run Runner, stateDir string) error` and `StateFile` (Task 1).
- **Produces, the guard loop:** `func Guard(ctx context.Context, daemonPID int, stateDir string, alive func(pid int) bool, recover func() error, poll time.Duration)`.
  - Every `poll`, it checks that the state file exists. If it doesn't, it returns, because the daemon restored cleanly.
  - If `!alive(daemonPID)`, it calls `recover()` and returns.
- **Produces, starting the guard:** `func StartGuard(stateDir string) error`.
  - It starts `os.Executable()` with args `--sound-guard <os.Getpid()> <stateDir>`.
  - The child has `SysProcAttr{Setsid: true}` and null stdio, and is not waited on.
  - The router calls it once, right after it first writes the state file.
- **The castrd entry point:**
  - `castrd --sound-guard <pid> <stateDir>` runs `Guard`.
  - It uses `alive = syscall.Kill(pid, 0) == nil`, `poll = 500ms`, and `recover = Recover(execRunner, stateDir)`.
  - It then exits 0, before any lock or config work.
- **Produces, the hook:** `NewRouter` gains `startGuard func() error` as a settable field `Router.StartGuard`. It is nil in tests unless set.

- [ ] **Step 1: Write the failing tests:**
  - `TestTheGuardRestoresWhenTheDaemonDies`: with a state file present and `alive` flipping to false after two polls, `recover` is called exactly once and `Guard` returns.
  - `TestTheGuardLeavesWhenTheDaemonRestoredItself`: the state file is removed while alive; `recover` is never called, and `Guard` returns.
  - `TestTheRouterStartsTheGuardOnceOnFirstAttach`, in `router_test.go`: `StartGuard` is called once across two attaches, and again after a full restore followed by a new attach.
- [ ] **Step 2:** Run `go test ./internal/sound/`. Expected: FAIL.
- [ ] **Step 3:** Implement `Guard`, `StartGuard`, the `Router.StartGuard` call site, and the `--sound-guard` branch in `cmd/castrd/main.go`. Parse the args strictly, and exit 2 on bad args.
- [ ] **Step 4:** Run `go test ./... && go vet ./...`. Expected: PASS.
- [ ] **Step 5: Commit:** `Restore the laptop's sound even if castrd is killed`.

---

### Task 3: helper `-audio-source` (doubletake fork, branch `castr`)

**Files:**
- Modify: `~/workspace/doubletake/cmd/doubletake/main.go`, `~/workspace/doubletake/internal/airplay/audio.go`
- Test: `~/workspace/doubletake/internal/airplay/audio_test.go`

**Interfaces:**
- **Produces, the flag:** `-audio-source <pulse source name>`.
  - When it is set, audio capture uses `pulsesrc device=<name>` with the same buffer and fragment settings as `pulseMonitorSourceArgs`, instead of `detectPulseMonitor()`.
  - When it is empty, behaviour is unchanged.
- **Produces, the capture call:** thread the value to `StartAudioCapture` through a new `AudioCaptureOptions{Source string}`, or an added parameter, whichever matches the existing call shape. If `pulsesrc` is missing, fail clearly instead of falling back to `pipewiresrc`, since castr asked for that specific source.

- [ ] **Step 1: Write the failing test** `TestAnExplicitAudioSourceIsCapturedInsteadOfTheDefaultMonitor`. The source args for `Source: "castr_cast_1.monitor"` contain `device=castr_cast_1.monitor` and never call the monitor detection; inject the detector, or test the pure args builder.
- [ ] **Step 2:** Run `go test ./internal/airplay/ -run AudioSource`. Expected: FAIL.
- [ ] **Step 3:** Implement the flag and the plumbing.
- [ ] **Step 4:** Run `go test ./... && go vet ./... && CGO_ENABLED=0 go build ./cmd/doubletake`. Expected: PASS, and a static binary.
- [ ] **Step 5: Commit**, on branch `castr`: `Capture audio from a source castr names`. Do not tag.

---

### Task 4: receivers capture their cast's source (AirPlay argv, Chromecast audio)

**Files:**
- Modify: `internal/backend/airplay/doubletake.go`, `internal/backend/airplay/session.go`, `internal/capture/pipeline.go`, `internal/backend/chromecast/backend.go`, `internal/backend/chromecast/live.go`, `cmd/castrd/main.go` (wiring the Chromecast audio flag)
- Test: `internal/backend/airplay/doubletake_test.go`, `internal/capture/pipeline_test.go` (new)

**Interfaces:**
- **Consumes:** the helper flag `-audio-source` (Task 3).
- **Produces, AirPlay argv:** `BuildArgv(cfg Config, deviceID, address string, port int, credsPath, audioSource string) []string`.
  - It appends `-audio-source <audioSource>` only when `cfg.Audio` is true and `audioSource != ""`.
  - `-no-audio` is unchanged.
- **Produces, the backends' source hook:** `AudioSource func(deviceID string) string`, a field on both `airplay.Backend` and the chromecast backend. A nil hook, or an empty result, means "no explicit source".
- **Produces, Chromecast capture:** `capture.Options` gains `AudioSource string` and `Audio bool`.
  - With `Audio && AudioSource != ""`, `Args()` replaces the silent track with `pulsesrc device=<AudioSource> do-timestamp=true provide-clock=false ! audioconvert ! audioresample ! audio/x-raw,rate=44100,channels=2 ! avenc_aac ! aacparse ! hls.audio`.
  - Otherwise, it keeps today's `audiotestsrc wave=silence` track exactly.
  - `provide-clock=false`: the pipeline must not run on an audio source's clock. See the capture-freeze fix of 2026-11-06.
- **Produces, Chromecast config:** the chromecast backend reads `cfg.AirPlay.Audio` (wired in `cmd/castrd/main.go`). "Sound off" now covers both receiver types.

- [ ] **Step 1: Write the failing tests:**
  - `TestTheCastsAudioSourceIsPassedToTheHelper`: the argv contains `-audio-source castr_cast_1.monitor` when audio is on, and not when audio is off or the source is empty.
  - `TestAChromecastCarriesTheCastsSound`: `Args()` with `Audio: true, AudioSource: "castr_cast_2.monitor"` contains `pulsesrc device=castr_cast_2.monitor` and `provide-clock=false`, and does not contain `audiotestsrc`.
  - `TestAChromecastWithoutSoundKeepsItsSilentTrack`: for `Audio: false`, or an empty source, `Args()` contains `audiotestsrc wave=silence` and `hls.audio`.
- [ ] **Step 2:** Run `go test ./internal/backend/... ./internal/capture/`. Expected: FAIL.
- [ ] **Step 3:** Implement. Update every `BuildArgv` call site and test helper. The backends call `b.AudioSource(device.ID)` when they build the argv or options.
- [ ] **Step 4:** Run `go test ./... && go vet ./...`. Expected: PASS.
- [ ] **Step 5: Commit:** `Each receiver captures its own cast's sound; Chromecasts get real sound`.

---

### Task 5: the daemon owns the routing; the `sound` command

**Files:**
- Modify: `internal/daemon/protocol.go`, `internal/daemon/server.go`, `internal/daemon/daemon.go`, `internal/client/client.go`, `internal/cli/cli.go`, `cmd/castrd/main.go`
- Test: `internal/daemon/daemon_test.go`, `internal/daemon/server_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- **Consumes:** `sound.Router` (Tasks 1–2) and the backends' `AudioSource` (Task 4).
- **Produces, the daemon's sound hook:** `type Sound interface { Attach(id string) (string, error); Set(id string, c sound.Choice) error; Choice(id string) (sound.Choice, bool); Detach(id string) error; Follow() error; Restore() error }`. It is the field `Daemon.Sound`; nil means no routing. Also `Daemon.SoundOn bool`, which comes from `cfg.AirPlay.Audio`.
- **Produces, the protocol:**
  - `CmdSound = "sound"`.
  - `Request.Sound string` with `json:"sound,omitempty"`.
  - `SessionJSON` gains `Sound string` with `json:"sound,omitempty"` (the current choice, or empty), and `SoundNote string` with `json:"sound_note,omitempty"`.
- **Produces, the daemon command:** `func (d *Daemon) SetSound(id string, c sound.Choice) error`. It returns an error if there is no such session, or if the session has no sound choice.
- **Produces, the client:** `func (c *Client) SetSound(id string, choice string) error`.
- **Produces, the CLI:**
  - `castr sound <id> <tv|laptop|both>` sets the choice.
  - `castr sound <id>` prints the current choice, or the note.
  - Add a line to `Usage`.
- **Daemon behaviour:**
  - **On start:** `Daemon.Start` calls `Sound.Attach(id)` before `backend.Start`, when `SoundOn && Sound != nil`.
    - The returned source is what the backends' `AudioSource(id)` hook returns. castrd wires the hook to a daemon method that looks up a `map[id]source`.
    - If `Attach` fails, record `SoundNote` (for example `sound stays on the laptop and the TV: <short reason>`) and start the cast anyway.
  - **On end:** detach exactly once per successful attach. That covers `Daemon.Stop`, the `OnState` reaching `Failed`/`Idle`, and a failed `backend.Start`.
  - **While anything is attached:** a goroutine started in castrd calls `Follow()` every 1s. A `Follow` error sets `SoundNote` on every session, and their choices become empty.
- **castrd wiring** (`cmd/castrd/main.go`):
  - After `hypr.RestorePanelIfPending`, call `sound.Recover(pactl, stateDir)`. The `pactl` runner is `exec.Command(name, args...).Output()`, like `hyprctl`.
  - Build the `sound.Router` with `StartGuard` set.
  - In `shutdown`, after the session loop, call `Sound.Restore()`. That covers Chromecast sessions too, which the existing loop doesn't stop.

- [ ] **Step 1: Write the failing tests:**
  - Use a `fakeSound` in `daemon_test.go` that records its calls.
  - `TestACastStartsWithItsSoundOnTheTV`: after `Start`, `Attach` was called once, and `SessionJSON.Sound == "tv"`.
  - `TestSoundIsDetachedWhenStartFails`: the backend's `Start` errors, so `Detach` is called once and no session remains.
  - `TestTheLastCastToEndRestoresSound`: start A and B, then end them via `Stop` and via `OnState(Failed)` in both orders. Each is detached exactly once.
  - `TestSoundOffTouchesNothing`: with `SoundOn=false`, no `Sound` call is made and `Sound` is empty.
  - `TestACastStartsEvenWhenSoundCannotBeRouted`: `Attach` errors, the session starts, `Sound` is empty, and `SoundNote` is non-empty.
  - `TestTheSoundCommandSwitchesOneCast`, over the socket with `rig.serve`: `{"cmd":"sound","device_id":A,"sound":"laptop"}` → `Set(A, Laptop)`. An unknown choice returns `Err` naming the three choices. An unknown id returns an error.
  - In `cli_test.go`, `TestCastrSoundSendsTheChoice`: `castr sound airplay:A both` sends `CmdSound` with `Sound:"both"`, and `castr sound` with no id fails with the usage text.
- [ ] **Step 2:** Run `go test ./internal/daemon/ ./internal/cli/`. Expected: FAIL.
- [ ] **Step 3:** Implement the protocol, handler, client, CLI and daemon hooks, then the castrd wiring.
- [ ] **Step 4:** Run `go test ./... && go vet ./...`. Expected: PASS. Mutation-test the "detach exactly once" rule: remove the detach from one end path, and a test must fail.
- [ ] **Step 5: Commit:** `castr sound: choose where each cast's sound plays`.

---

### Task 6: the widget's SOUND section

**Files:**
- Modify: `share/quickshell/castr-indicator/Widget.qml`
- Test: `internal/ui/widget_test.go`

**Interfaces:**
- **Consumes:** `castr status --json` session fields `device_id`, `name`, `sound` and `sound_note`, plus `castr sound <id> <choice>` (Task 5).
- **Produces:**
  - **Where it goes:** between the sessions `Repeater`/`PanelSeparator` and the MODE header, add a SOUND section.
    - A `PanelSectionHeader` with the text `SOUND`, visible when any session has a non-empty `sound` or `sound_note`.
    - Then a `Repeater` over `root.sessions`. Each delegate shows:
      - when there are two or more sessions, a `Text` with the receiver's name (`Text.PlainText`);
      - a `ButtonGroup` with options `[{value:"tv",label:"TV"},{value:"laptop",label:"Laptop"},{value:"both",label:"Both"}]`, `value: modelData.sound`, visible when `modelData.sound !== ""`, and the same `foreground`/`accent`/`fontFamily` as MODE;
      - when `sound_note` is set, the note as a caption `Text`.
  - **Switching:**
    - `onChanged` calls `root.setSound(modelData.device_id, value)`.
    - That sets `soundProc.command = ["castr","sound",deviceId,value]` and runs it.
    - `soundProc` has `onExited`: `refreshPanel()`.
  - **Guard:** `value` is one of the three literals. Validate it in `setSound` before building the command.

- [ ] **Step 1: Write the failing tests:**
  - `TestTheSoundSwitchRunsOnlyCastrSoundWithFixedArguments`:
    - `processCommand` of `soundProc` is `["castr","sound",deviceId,value]`.
    - `setSound` rejects anything but `tv`, `laptop` and `both`.
  - Extend `TestEveryProcessTheAlwaysOnTimerStartsIsSafeOrGated`, so `soundProc` is never started by the always-on timer.
  - Extend `TestReceiverTextIsNeverRenderedAsRichText` to cover the new name `Text`.
- [ ] **Step 2:** Run `go test ./internal/ui/`. Expected: FAIL.
- [ ] **Step 3:** Implement in `Widget.qml`.
- [ ] **Step 4:** Run `go test ./... && go vet ./...`. Expected: PASS. Then reload the widget in the running shell, if the owner allows it, and check visually against the mockup (layout A in the spec).
- [ ] **Step 5: Commit:** `Widget: a SOUND section for each cast`.

---

### Task 7: packaging and docs

**Files:**
- Modify: `packaging/PKGBUILD`, `README.md`

- [ ] **Step 1: `packaging/PKGBUILD`:**
  - Move `libpulse` from `optdepends` to `depends`, with the comment `pactl: routes a cast's sound`.
  - Leave `_airplay` and the sums alone, because those belong to the release.
- [ ] **Step 2: `README.md`:**
  - A short "Sound" section: TV by default, the widget's SOUND buttons, `castr sound <id> tv|laptop|both`, the Chromecast echo with Both, and that `[airplay] audio = false` turns sound off for every receiver.
  - Update the `audio = true` comment in the config example.
- [ ] **Step 3:** Run `bash packaging/packaging_test.sh && go test ./...`. Expected: PASS.
- [ ] **Step 4: Commit:** `Document choosing where a cast's sound plays; pactl is now required`.

---

### Task 8: hardware check (controller, with the user)

Not delegated. Needs the user at the TV.

- [ ] **Step 1: Build.**
  - Build the helper from branch `castr`: `CGO_ENABLED=0 go build -o $S/bin/castr-airplay-snd ./cmd/doubletake`.
  - Build castr: `go build ./cmd/castr ./cmd/castrd`.
  - Run castrd with `CASTR_AIRPLAY=$S/bin/castr-airplay-snd`, started from the user's session. It must not be started from the Claude app's shell; see PROGRESS.
- [ ] **Step 2: Office Apple TV, mirror, with sound playing.**
  - Default TV: sound on the TV only.
  - Switch to Laptop: laptop only, and the TV is silent with the picture still live.
  - Switch to Both: both.
  - Back to TV.
  - The picture never restarts: the helper pid is unchanged, and `bytes_sent` keeps rising.
- [ ] **Step 3: Extend:** the same checks once.
- [ ] **Step 4: Change output mid-cast** (headphones or Bluetooth) while on Laptop. The sound follows to the new output.
- [ ] **Step 5: `kill -9` castrd mid-cast.**
  - Within ~2 s the laptop plays sound again.
  - `pactl get-default-sink` is the original.
  - `pactl list short modules | grep castr_` is empty.
- [ ] **Step 6: Volume.** The original sink's volume is the same before and after each test.
- [ ] **Step 7: Chromecast, if one is available:** default TV with sound on the TV, and a Laptop/Both switch.
- [ ] **Step 8:** Record the results in `PROGRESS.md`.
