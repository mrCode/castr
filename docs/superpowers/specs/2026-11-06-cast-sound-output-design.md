# Choosing where a cast's sound plays — design

Date: 2026-11-06
Status: agreed in conversation; awaiting the user's review of this document.

## Why

While castr casts to an Apple TV, the sound comes out of **both** the TV and
the laptop. castr's AirPlay helper (`castr-airplay`) copies the monitor of
the default output and sends that copy to the TV, and nothing silences the
laptop. The user doesn't want that by default. They want the sound on the TV
and a way to change it during a cast.

A Chromecast today gets **no** real sound at all. castr's pipeline
(`internal/capture/pipeline.go`) muxes a silent AAC track, because a
receiver won't play an HLS stream without an audio track. The user wants the
same behaviour for both kinds of receiver, so Chromecasts gain real sound as
part of this work.

## What the user decided

- **Three choices for each cast:** **TV**, **Laptop** and **Both**.
  - **TV:** the sound plays on the receiver only, and the laptop is silent.
  - **Laptop:** the sound plays on the laptop only. The receiver gets silence
    but keeps the picture.
  - **Both:** the sound plays on both.
- **The default is TV**, every time a cast starts. A choice lasts only for
  that cast and is not remembered for the next one.
- **The choice can change during a cast,** taking effect straight away.
- **The choice covers all of the laptop's sound,** not one app's: video,
  music, notification sounds and calls alike. It is the whole system mix the
  helper already captures, so a call during a cast set to TV plays on the TV.
  The user chose this to keep it simple. Per-app routing is out of scope.
- **"Laptop" means whatever the laptop is playing through at that moment:**
  built-in speakers, headphones or Bluetooth, i.e. the current default
  output. It is not tied to one device.
- **The same behaviour on an Apple TV and a Chromecast.**
- **Each cast has its own choice** when castr casts to more than one
  receiver at once.
- **The widget gets its own SOUND section** (layout A from the mockups):
  - It appears only while something is casting, under the casting row(s) and
    above MODE.
  - It is a three-button group, **TV / Laptop / Both**, styled like MODE's
    Mirror/Extend group.
  - With several casts, each cast gets its own SOUND group, labelled with its
    receiver's name.

## How several casts combine

This rule is mine, played back to the user and not objected to:

- **A receiver** gets sound when its own cast is set to TV or Both.
- **The laptop** plays when **any** cast is set to Laptop or Both. It is
  silent only when every cast is set to TV.
- **When the last cast ends,** the laptop is restored to how it was before
  the first cast began.

## Goals

- The default experience is right with no setup: start a cast, and the sound
  is on the TV only.
- A switch in the widget during a cast changes where the sound plays,
  without interrupting or restarting the picture.
- The `castr` CLI exposes the same switch, because the widget works by
  running `castr` commands (the widget never talks to the daemon directly).
  Exact command syntax is the builder's call, kept in the style of the
  existing commands.

## Anti-goals (failures even if everything else works)

1. **The laptop left silent** after a cast ends, by any route: a normal stop,
   the receiver hanging up, the helper crashing, castrd crashing or being
   killed, logout. This is the one that matters most. Restore must not
   depend only on a clean exit path. If castrd dies while the laptop is
   muted, the next castrd start (or some equally robust mechanism) must put
   it back.
2. **The user's default output device or volume left changed** afterwards,
   or changed during the cast in a way the user would notice beyond the
   intended mute. For example, the volume level must not jump when the
   sound comes back.
3. **Switching interrupts or restarts the picture.**
4. **A regression for Apple TV sound,** which works today (tested
   2026-11-06 on the office TV, mirror with audio).
5. **Leaving other casting software or apps broken.** This includes stray
   virtual sinks or loopbacks left behind, and apps stuck on a device that no
   longer exists.

## Constraints

- **The helper is castr's own:**
  - It is the fork at `~/workspace/doubletake`, branch `castr` (LGPL-3.0).
  - It is built by castr's PKGBUILD from a tag (`castr-airplay-N`).
  - castr may change it, for example to take an explicit audio source or to
    send silence on request. Any helper change needs a new tag, and the
    PKGBUILD then points at it.
- **How the helper captures sound today:** `internal/airplay/audio.go` uses
  `pulsesrc device=<default sink monitor>` (found with `pactl`), falling back
  to `pipewiresrc`.
- **The audio stack** is PipeWire with pipewire-pulse (`pactl` is available;
  `libpulse` is an optdepend).
- **castr starts castrd on demand,** with no systemd unit. castrd discards
  its own log output.
- **The widget:**
  - It lives in `share/quickshell/castr-indicator/` in castr, and is shipped
    as the marketplace plugin `castr.indicator`.
  - Plugin repo: `~/workspace/castr-indicator`.
  - Standing rule: the AUR package's widget always matches the released
    plugin. A widget change means a plugin release and a castr release
    together, kept in sync by `packaging/sync-plugin-repo.sh`.
- **The marketplace reviewer has been strict about the widget.** No editable
  text inputs, no clipboard access, nothing on argv that shouldn't be. Keep
  the SOUND control to plain buttons that run `castr` commands with fixed
  arguments.
- **Chromecast latency:** the HLS delay of 1–3 s means **Both** on a
  Chromecast echoes (the laptop plays immediately, the TV seconds later). The
  user accepted that: no warning text is needed.
- **The config already has `[airplay] audio = true`** (`-no-audio` when
  false). When audio is off, there is nothing to route and the SOUND section
  shouldn't offer choices it can't honour. The builder decides how to show
  that.

## Calls I made (the user can overrule them)

- **Sound off in the config means sound off for every receiver.**
  - Today `[airplay] audio = false` only turns off Apple TV sound.
  - With this change it applies to Chromecasts too. The builder may move it
    to a receiver-neutral place in the config, as long as existing configs
    keep working.
  - With sound off: castr never touches the laptop's sound, receivers get
    silence (a Chromecast keeps its required silent track), and the SOUND
    section isn't shown.
- **Extend behaves like mirror.** The SOUND section and its rules are the
  same in both modes.
- **If castr can't route the sound** (`pactl` missing or failing, or no
  default output):
  - The cast still starts with sound on both, as today.
  - The widget's SOUND section shows that the choice is unavailable, with
    one short line saying why.
  - `libpulse`, which provides `pactl`, becomes a hard dependency of the
    package, so this is a failure case rather than the normal one.
- **Prefer routing-only changes over helper changes,** if they can meet the
  goals. This lowers the risk to the Apple TV path that works today. A
  helper change is still allowed when routing can't do it, and it needs a
  new `castr-airplay-N` tag.
- **The plan ends at a built, tested and hardware-checked tree.**
  - Releasing is a separate step the user approves, as with 0.3.0.
  - The widget change will need plugin 1.4.0 and a matching castr release.
  - Marketplace request #11503, currently pending at plugin 1.3.0, is
    retargeted at that time only if the user wants.

## Left to the builder

- **How the laptop is silenced and un-silenced.** One option is a castr null
  sink made the default, with its monitor captured, plus a loopback to the
  real output for Laptop/Both. Another is muting the real sink, if its
  monitor still carries the sound. Which works must be **verified on this
  machine first**: whether a PipeWire sink's monitor still carries audio
  when the sink is muted is not established. Whatever is chosen must follow
  the current default output (headphones plugged in mid-cast), and must meet
  anti-goals 1, 2 and 5.
- **How a receiver gets silence for Laptop.** This could be done in the
  helper, in the capture pipeline, or by routing.
- **Chromecast sound capture.** It replaces the silent track with real sound
  in `internal/capture/pipeline.go`, keeping a valid AAC track when silence
  is wanted.
- **The CLI syntax, and the daemon/client protocol addition,** for setting a
  cast's sound choice.
- **The tests:**
  - Follow the repo's existing patterns (Go tests, `packaging_test.sh`).
  - Include a test that the restore path runs after a simulated crash.
- **Hardware check before release:**
  - The office Apple TV and a Chromecast, if one is available.
  - Each choice, switching mid-cast, headphones plugged in mid-cast, and
    killing castrd mid-cast, followed by checking that the laptop's sound
    is back.
