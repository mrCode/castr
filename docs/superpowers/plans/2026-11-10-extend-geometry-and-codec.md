# Configurable stream size and codec (castr issue #3) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** castr users can choose the stream size and codec. The choices are:
- `[capture] geometry`: `1920x1080@60` (the default, today's behaviour), any `WxH@R`, or `auto` for the receiver's own canvas.
- `[capture] codec = auto | h264 | hevc`.

Also, AMD and Intel machines get hardware HEVC through `vah265enc`.

**Architecture:**
- The helper (castr-airplay, fork `~/workspace/doubletake`, branch `castr`) gains three things:
  - `vah265enc` as an explicit-HEVC candidate;
  - a `-max-size WxH` cap on the canvas it negotiates;
  - a `-print-canvas` mode that connects, reads `/info`, prints the canvas it would stream for the given codec and cap, and exits.
- castr gains two config keys and passes `-video-codec` and `-max-size` to the helper.
- In extend mode, castr sizes the virtual output from `geometry`. For `auto` it asks the helper with `-print-canvas` first.

**Tech Stack:** Go, GStreamer (`vah265enc` from gst-plugin-va), Hyprland virtual outputs.

**Spec:** the requirements are the reporter's issue (mrCode/castr#3) plus castr's reply to it (issuecomment 6752503941). `gh issue view 3 -R mrCode/castr --comments` shows both. The user asked to skip the design conversation, so this plan carries the decisions.

## Facts established (2026-11-10, reading the helper at e699535)

- **How the helper chooses its canvas** (`internal/airplay/video_codec.go` `selectVideo`):
  - `h264` and `auto` use the receiver's nominal `MirrorSize()`, for example 1920x1080.
  - Explicit `hevc` uses `maximumVideoCanvas()`: the receiver's advertised maximum, capped aspect-preserving to 3840x2160 by `fitVideoSize`.
  - There is no way to ask for HEVC at anything below the receiver's maximum.
- **HEVC encoders** (`internal/airplay/capture.go` `selectGstHEVCEncoder`):
  - The candidates are only `nvh265enc` (method `nvenc`) and `x265enc` (method `none`).
  - The candidate loop skips any whose method isn't `hwaccel`, unless `hwaccel` is `auto`.
  - `automaticHEVCAvailableWithProbe` deliberately allows automatic high-resolution mode only on NVENC. That stays as it is.
- **castr's extend size** is the constant `hypr.StreamGeometry = "1920x1080@60"` (`internal/hypr/mode.go:14`; parsed at `:110`).
  - Its comment says the old doubletake fallback path had no scaler, so a larger source made the receiver hang up.
  - The current helper scales to the receiver's canvas (`receiverScaleStages`), as the 2026-11-06 720p runs showed, so that reason is believed gone. Task 6 verifies it.
- **castr's encoder vocabulary**: `[capture] encoder` is one of `auto|vaapi|nvenc|x264`. `BuildArgv` maps it to `-hwaccel` through the `hwaccel` map in `internal/backend/airplay/doubletake.go`.
- **Device-controlled data**: `/info` is controlled by whatever answers at the receiver's address. A canvas read from it must be clamped before castr sizes a monitor from it.

## Global Constraints

- **Defaults change nothing.** With neither key set, behaviour and argv are identical to today: extend stays `1920x1080@60`, and no `-video-codec` or `-max-size` flag is passed.
- **`geometry`** is `"WxH@R"` with W 640–7680, H 360–4320 and R 24–240 (all integers), or `"auto"`.
  - An invalid value logs a warning naming the value and falls back to `1920x1080@60`. It never fails the whole config, the same way `target_latency_ms` is handled in `config.go`.
- **`codec`** is `"auto"`, `"h264"` or `"hevc"`, case-insensitive.
  - An invalid value logs a warning and falls back to `auto`.
  - `auto` passes no `-video-codec`, so the helper's own default applies.
- **The `-max-size` cap only ever shrinks.** It is applied with `fitVideoSize`, so the aspect ratio is preserved and the receiver's maximum is never exceeded.
- **Clamping the canvas** from `-print-canvas` or `/info`: castr accepts only 640x360 to 3840x2160. Anything else falls back to `1920x1080@60`, with a log line.
- **Automatic HEVC stays NVENC-only.** `vah265enc` is used only when HEVC is requested explicitly (`-video-codec hevc`).
- **Helper changes go on doubletake branch `castr`.** Local commits only, no tag; castr's PKGBUILD tag bump belongs to the release.
- **Commits** end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Do not push.
- **The base** is castr branch `feature/extend-geometry`, created from `feature/sound-output`, because both features ship together as castr 0.4.0.

## Review Focus

1. A receiver advertising nonsense (0x0, 100000x100000, a missing `displays`) in `-print-canvas`: castr clamps it or falls back, and never creates an absurd monitor. Test: Task 5 `TestAnAbsurdReceiverCanvasFallsBackTo1080p`.
2. `codec = hevc` on a machine with no HEVC encoder: the helper's error reaches the user the way other capture errors already do (the session's error text), not as a silent hang. Test: Task 2 `TestExplicitHEVCWithNoEncoderSaysWhichToInstall`.
3. `-print-canvas` against a receiver that is unreachable or slow: castr falls back to `1920x1080@60` within a bounded time (5 s) and still starts the cast. Test: Task 5 `TestAutoGeometryFallsBackWhenTheReceiverCannotBeAsked`.
4. `geometry` larger than the receiver's maximum: the helper streams at most the receiver's maximum (`-max-size` never raises), and castr's extend output is still the size asked for, scaled by the helper. Test: Task 3 `TestTheCapNeverRaisesTheCanvas`.
5. Mirror mode with `geometry` set: the cap applies to mirror too (`-max-size`), and mirror never creates an output. Test: Task 4 `TestGeometryCapsMirrorWithoutCreatingAnOutput`.

---

### Task 1: helper — `vah265enc` as an explicit-HEVC candidate

**Files:** modify `~/workspace/doubletake/internal/airplay/capture.go` (`selectGstHEVCEncoder`); test in `internal/airplay/capture_test.go`.

**Interfaces:** produces a new candidate with `method: "vaapi"`, element `vah265enc`, label `VA-API HEVC Main hardware encoding (vah265enc)`.
- Its parts mirror the existing `vah264enc` candidate's rate control and keyframe settings (find them in `selectGstH264Encoder`), using `bitrate=%d`, `key-int-max=%d`, `rate-control=cbr` and `target-usage=7`.
- Its raw format is `NV12`.
- Order: `nvh265enc`, then `vah265enc`, then `x265enc`.
- The `auto` error message becomes `HEVC requires GStreamer nvh265enc, vah265enc or x265enc; none is available`.
- The non-auto message lists `nvenc, vaapi or none`.
- `automaticHEVCAvailableWithProbe` is unchanged.

- [ ] **Step 1: Write the failing tests:**
  - `TestVAAPIHEVCIsAnExplicitHEVCCandidate`: with `hasElement` true only for `vah265enc`, the result's first part is `vah265enc` for `hwaccel` `auto` and `vaapi`.
  - `TestAutomaticHighResolutionStaysNVENCOnly`: `automaticHEVCAvailableWithProbe("auto", only vah265enc and h265parse)` is false.
  - `TestExplicitHEVCWithNoEncoderSaysWhichToInstall`: the error names all three elements.
- [ ] **Step 2:** Run `go test ./internal/airplay/ -run 'HEVC'`. Expected: FAIL.
- [ ] **Step 3:** Implement. Check the property names with `gst-inspect-1.0 vah265enc` on this machine (read-only). Use only properties that exist.
- [ ] **Step 4:** Run `go test ./... && go vet ./... && CGO_ENABLED=0 go build ./cmd/doubletake`. Expected: PASS.
- [ ] **Step 5: Commit:** `HEVC through VA-API: vah265enc on AMD and Intel`.

### Task 2: helper — `-max-size WxH`

**Files:** modify `~/workspace/doubletake/cmd/doubletake/main.go` (the flag) and `internal/airplay/video_codec.go` (the cap in `selectVideo`). Thread it through `StreamConfig` or the existing selection call, whichever already carries `-video-codec`. Test: `internal/airplay/video_codec_test.go`.

**Interfaces:**
- **The flag:** `-max-size WxH`.
  - Empty means no cap.
  - A malformed value is a startup error: `-max-size must be WIDTHxHEIGHT`.
- **The cap:**
  - `selectVideo` applies `fitVideoSize(width, height, maxW, maxH)` after picking the codec canvas.
  - It never raises.
  - It keeps both dimensions even, because encoders need even sizes.

- [ ] **Step 1: Write the failing tests:**
  - `TestTheCapShrinksHEVCToTheRequestedSize`: max canvas 3840x2160 with cap 2560x1440 gives 2560x1440.
  - `TestTheCapNeverRaisesTheCanvas`: nominal 1920x1080 with cap 3840x2160 gives 1920x1080.
  - `TestTheCapKeepsTheAspectRatio`: 3840x2160 with cap 1920x1200 gives 1920x1080.
  - `TestAMalformedMaxSizeIsRejected`, at the flag-parsing level.
- [ ] **Step 2:** Run the tests. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./... && go vet ./... && CGO_ENABLED=0 go build ./cmd/doubletake`. Expected: PASS.
- [ ] **Step 5: Commit:** `-max-size: cap the canvas the helper negotiates`.

### Task 3: helper — `-print-canvas`

**Files:** modify `~/workspace/doubletake/cmd/doubletake/main.go`. Test: `cmd/doubletake` (the existing scripted-receiver tests show how to fake a receiver) or a small pure function in `internal/airplay`.

**Interfaces:**
- **`-print-canvas`** takes `-target`, `-port`, `-video-codec` and `-max-size` into account.
  - It connects and runs `GetInfo` only: no pairing, no credentials read or written, no capture, no portal.
  - It computes the canvas `selectVideo` would choose, with the cap.
  - It prints exactly one line to stdout, `canvas <W>x<H>`, and exits 0.
- **On any failure** it prints `canvas unknown: <reason>` and exits 1.
- **Produces, for castr's Task 5:** the stdout format `canvas 1920x1080`.

- [ ] **Step 1: Write the failing tests:**
  - `TestPrintCanvasReportsTheNominalCanvasForH264`
  - `TestPrintCanvasReportsTheCappedMaximumForHEVC`
  - `TestPrintCanvasNeverPairsOrTouchesCredentials`: the fake receiver sees only `GET /info`.
- [ ] **Step 2:** Run the tests. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./... && go vet ./... && CGO_ENABLED=0 go build ./cmd/doubletake`. Expected: PASS.
- [ ] **Step 5: Commit:** `-print-canvas: say what size a receiver would get, without casting`.

### Task 4: castr — `[capture] geometry` and `[capture] codec`, passed to the helper

**Files:** modify `internal/config/config.go` (with tests) and `internal/backend/airplay/doubletake.go` (`Config` and `BuildArgv`, with tests); wire them in `cmd/castrd/main.go`.

**Interfaces:**
- **Config:** `config.Capture` gains `Geometry string` (`toml:"geometry"`) and `Codec string` (`toml:"codec"`).
- **Parsing:** `func ParseGeometry(s string) (g Geometry, auto bool, err error)` with `type Geometry struct{ Width, Height, Refresh int }` and `func (g Geometry) String() string`, which gives `"WxH@R"`.
  - Defaults are `"1920x1080@60"` and `"auto"`.
  - Validation and fallback follow Global Constraints.
- **The helper's config:** `airplay.Config` gains `Codec string` and `MaxSize string` (`"WxH"`, or empty).
- **`BuildArgv`:**
  - It appends `-video-codec <codec>` only when the codec isn't `auto`.
  - It appends `-max-size <WxH>` only when `MaxSize != ""`.
- **castrd** sets `MaxSize` from an explicit geometry, or from the size resolved in Task 5 for `auto` in extend. In mirror with `auto` it sets no cap.

- [ ] **Step 1: Write the failing tests:**
  - `TestGeometryDefaultsToToday`
  - `TestAnInvalidGeometryFallsBackWithAWarning`: cover `"4k"`, `"100x100@60"` and `"1920x1080@500"`.
  - `TestCodecIsValidatedAndDefaultsToAuto`
  - `TestTheCodecAndCapAreForwardedOnlyWhenSet`
  - `TestGeometryCapsMirrorWithoutCreatingAnOutput`: in mirror mode with an explicit geometry, `BuildArgv` has `-max-size`, and the hypr layer is not asked for an output.
- [ ] **Step 2:** Run `go test ./internal/config/ ./internal/backend/airplay/`. Expected: FAIL.
- [ ] **Step 3:** Implement, and update every `BuildArgv` call site and test helper.
- [ ] **Step 4:** Run `go test ./... && go vet ./...`. Expected: PASS.
- [ ] **Step 5: Commit:** `[capture] geometry and codec, forwarded to castr-airplay`.

### Task 5: castr — the extend output follows `geometry`, and `auto` asks the receiver

**Files:** modify `internal/hypr/mode.go` (replace the constant), the extend start path in `internal/backend/airplay/session.go`, and `cmd/castrd/main.go`. Test: `internal/hypr/mode_test.go`, `internal/backend/airplay/session_test.go`.

**Interfaces:**
- **The output size:** `hypr.StreamGeometry` becomes `const DefaultStreamGeometry = "1920x1080@60"`, plus a parameter. Every function that created the virtual output at the constant now takes a `config.Geometry` (or a `"WxH@R"` string). Keep the comment's intent, updated: castr-airplay scales to the receiver's canvas, so the output may be larger than the receiver.
- **Asking the receiver:** `func (b *Backend) receiverCanvas(ctx context.Context, device discovery.Device) (w, h int, err error)`.
  - It runs `Binary -print-canvas -target <addr> -port <port> [-video-codec c] [-max-size cap]` with a 5 s timeout.
  - It parses `canvas WxH`.
  - It clamps per Global Constraints.
  - It is injectable for tests, through the backend's existing `Spawner` or a new `func` field.
- **Behaviour for `auto` in extend:**
  - The output is `<W>x<H>@60`, from `receiverCanvas`.
  - On any error or clamp failure, log it and use `1920x1080@60`; the cast starts regardless.
  - `MaxSize` for that cast is set to `WxH`.

- [ ] **Step 1: Write the failing tests:**
  - `TestExtendUsesTheConfiguredGeometry`: `2560x1440@60` gives an output created at `2560x1440@60`.
  - `TestAutoGeometryUsesWhatTheReceiverWouldGet`: fake `-print-canvas` prints `canvas 3840x2160`, so the output is `3840x2160@60` and the argv has `-max-size 3840x2160`.
  - `TestAnAbsurdReceiverCanvasFallsBackTo1080p`: `canvas 100000x100000`, `canvas 0x0` and garbage all give `1920x1080@60`.
  - `TestAutoGeometryFallsBackWhenTheReceiverCannotBeAsked`: an exit status of 1, or the timeout, gives `1920x1080@60`, and the cast starts.
- [ ] **Step 2:** Run the tests. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./... && go vet ./...`. Expected: PASS.
- [ ] **Step 5: Commit:** `Extend at the size asked for, or at the receiver's own canvas`.

### Task 6: docs, and a check on this machine (controller)

- [ ] **Step 1: README.** In the config example and a short "Stream size and codec" note, document:
  - `geometry` and `codec`, with today's behaviour as the default;
  - that `hevc` uses NVENC, VA-API (`vah265enc`, gst-plugin-va) or x265, in that order;
  - that 4K needs a GPU able to encode it, citing the issue's Renoir numbers (21.7 fps for 4K HEVC).

  Commit: `Document stream size and codec`.
- [ ] **Step 2 (controller): run on this machine against the stand-in receiver,** with the `modern` profile. Run each case for 60 s, without freezes.
  - Extend with `geometry = "2560x1440@60"`: the virtual output is 2560x1440, and the helper streams the receiver's canvas, scaled. This confirms the "no scaler" concern is gone.
  - `codec = "hevc"` with `vah265enc` if this machine has it (`gst-inspect-1.0 vah265enc`).
- [ ] **Step 3:** Record the results in `PROGRESS.md`, then ask the user before pinging @diegosoares on #3 with the branch for testing. That comment is outward-facing.
