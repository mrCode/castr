package capture

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// Container is how the encoded video is wrapped for the receiver.
type Container string

const (
	// FragmentedMP4 is H.264 in an MP4 that can be written without ever
	// seeking back to fill in a header -- an ordinary MP4 cannot be streamed
	// live, because its index is written at the end.
	FragmentedMP4 Container = "mp4"
	// WebM is VP8/VP9, the other container a Chromecast plays natively.
	WebM Container = "webm"
	// HLS is H.264 cut into short segments behind a playlist.
	//
	// It exists here because the other two do not work. A Chromecast's default
	// receiver accepts a LOAD of an endless chunked MP4 or WebM, fetches a few
	// hundred kilobytes, and then stops reading and never reports a player
	// state -- measured on a Xiaomi stick, both containers, same behaviour. A
	// live stream has to arrive as segments behind a playlist that keeps
	// changing, which is what the receiver knows how to follow.
	HLS Container = "hls"
)

// Options describe one capture.
type Options struct {
	// NodeID is the node the portal granted. It is what the guard checks
	// against; the pipeline addresses the node by its serial.
	NodeID uint32
	// Serial is the granted node's object.serial. pipewiresrc's target-object
	// matches a name or a serial -- NOT a node id. Passing the node id there
	// is what made omarchy-cast stream a webcam.
	Serial uint32
	// FD is the PipeWire remote from the portal. The pipeline is a child
	// process and receives it as descriptor 3.
	FD *os.File

	FPS       int
	Encoder   string // an element name, already chosen
	Bitrate   int    // kbit/s, 0 for the encoder's default
	Container Container

	// Dir is where HLS writes its playlist and segments. Unused otherwise.
	Dir string
	// Width and Height scale the capture before encoding. Zero leaves the
	// screen's own size.
	//
	// This is not a bandwidth setting. A receiver's decoder has limits, and a
	// stream above them is refused outright: a 2560x1600 capture produced
	// LOAD_FAILED with idleReason ERROR on a Xiaomi stick, while the same
	// pipeline scaled down played.
	Width, Height int
	// SegmentSeconds is the length of one HLS segment. It sets the floor on
	// latency: a receiver waits for a segment or two before it starts, so this
	// is the difference between a two-second delay and a ten-second one.
	SegmentSeconds int
}

// PlaylistName is the HLS playlist a receiver is pointed at.
const PlaylistName = "live.m3u8"

// ContentType is what the receiver must be told to expect.
func (o Options) ContentType() string {
	switch o.Container {
	case WebM:
		return "video/webm"
	case HLS:
		// The RFC 8216 type. The older application/x-mpegurl is what most
		// examples use and what ffmpeg warns about ("mime type is not rfc8216
		// compliant"); a Chromecast handed it reads the playlist and never
		// requests a segment.
		return "application/vnd.apple.mpegurl"
	}
	return "video/mp4"
}

// Args builds the gst-launch invocation.
//
// Two things are deliberately absent. There is no `video/x-raw,framerate=N/1`
// capsfilter on the source: constraining the portal stream that way makes
// pipewiresrc fail negotiation ("set output format: -22") and produce no
// buffers at all, silently, while the session still reports success.
// `videorate max-rate` caps the rate as a property instead, after
// videoconvert, so it never constrains the source pad.
//
// And there is no `autoconnect=false`. It reads like the safety setting and is
// not one: it does stop the substitution, but it stops all capture with it.
// See docs/capture-safety.md. Safety is the guard's job.
func (o Options) Args() []string {
	fps := o.FPS
	if fps <= 0 {
		fps = 30
	}

	args := []string{"--quiet"}
	if o.Container == HLS {
		args = append(args, "hlssink2", "name=hls",
			"target-duration="+strconv.Itoa(o.segmentSeconds()),
			// A sliding window, not an archive. A live playlist that only
			// grows makes a receiver start at the beginning of the cast and
			// stay that far behind for as long as it runs.
			"playlist-length=4", "max-files=6",
			"playlist-location="+filepath.Join(o.Dir, PlaylistName),
			"location="+filepath.Join(o.Dir, "segment%05d.ts"))
	}
	args = append(args,
		"pipewiresrc",
		"fd=3",
		"target-object="+strconv.FormatUint(uint64(o.Serial), 10),
		"do-timestamp=true",
		"!", "videoconvert",
		"!", "videoscale",
		"!", "videorate",
		// A FIXED framerate, not a maximum.
		//
		// The portal emits a frame when the screen changes and nothing when it
		// does not, so an idle desktop produces well under two frames a
		// second. Measured: avg_frame_rate 5/3 on a still screen. For a live
		// segmented stream that is fatal -- the receiver drains the playlist
		// faster than the encoder fills it, plays a few seconds and stalls.
		// A fixed rate makes videorate repeat the last frame, which costs
		// almost nothing to encode and keeps the timeline continuous.
		//
		// The capsfilter sits AFTER videorate deliberately. On the source pad
		// it makes pipewiresrc fail negotiation ("set output format: -22") and
		// produce no buffers at all, silently, while everything still reports
		// success.
		"!", "video/x-raw,framerate="+strconv.Itoa(fps)+"/1"+o.scaleCaps(),
	)

	switch o.Container {
	case HLS:
		args = append(args, "!", o.Encoder)
		args = append(args, h264EncoderArgs(o.Encoder, o.Bitrate, o.keyInterval(fps))...)
		// Every segment must open with a keyframe or a receiver joining
		// mid-stream shows nothing until the next one.
		args = append(args, "!", "h264parse", "config-interval=-1", "!", "hls.video")
		// A silent audio track, because the receiver will not play without one.
		//
		// A video-only HLS stream is fetched -- playlist, then segments -- and
		// then refused with LOAD_FAILED and idleReason ERROR. The receiver
		// downloads the video before rejecting it, so the failure looks like a
		// decoding problem with the picture. Adding silence fixed it; nothing
		// about the video changed.
		args = append(args,
			"audiotestsrc", "wave=silence", "is-live=true",
			"!", "audioconvert", "!", "audioresample",
			"!", "audio/x-raw,rate=44100,channels=2",
			"!", "avenc_aac", "!", "aacparse", "!", "hls.audio")
		return args
	case WebM:
		args = append(args, "!", o.Encoder)
		args = append(args, webmEncoderArgs(o.Encoder, o.Bitrate)...)
		args = append(args, "!", "webmmux", "streamable=true")
	default:
		args = append(args, "!", o.Encoder)
		args = append(args, h264EncoderArgs(o.Encoder, o.Bitrate, o.keyInterval(fps))...)
		// config-interval=-1 repeats the parameter sets before every keyframe.
		// A receiver that joins a live stream mid-flight has not seen the
		// header, and without repeats it waits for one that never comes.
		args = append(args, "!", "h264parse", "config-interval=-1")
		args = append(args, "!", "mp4mux",
			"fragment-duration=1000", "streamable=true", "faststart=false")
	}

	return append(args, "!", "fdsink", "fd=1", "sync=false")
}

// scaleCaps adds the output size, keeping the aspect ratio square so a 16:10
// desktop is not stretched to fill a 16:9 television.
func (o Options) scaleCaps() string {
	if o.Width <= 0 || o.Height <= 0 {
		return ""
	}
	return fmt.Sprintf(",width=%d,height=%d,pixel-aspect-ratio=1/1", o.Width, o.Height)
}

func (o Options) segmentSeconds() int {
	if o.SegmentSeconds > 0 {
		return o.SegmentSeconds
	}
	return 1
}

// keyInterval is how many frames apart keyframes must be.
//
// For HLS it is exactly one segment. hlssink2 can only cut a segment at a
// keyframe, so a keyframe interval longer than the target duration produces
// segments longer than the playlist claims, and a receiver that trusts the
// playlist runs out of video and stalls.
func (o Options) keyInterval(fps int) int {
	if o.Container == HLS {
		return fps * o.segmentSeconds()
	}
	return fps
}

func h264EncoderArgs(element string, bitrate, keyInterval int) []string {
	key := "key-int-max=" + strconv.Itoa(keyInterval)
	switch element {
	case "x264enc":
		args := []string{"speed-preset=ultrafast", "tune=zerolatency", key}
		if bitrate > 0 {
			args = append(args, "bitrate="+strconv.Itoa(bitrate))
		}
		return args
	case "vah264enc", "nvh264enc":
		args := []string{key}
		if bitrate > 0 {
			args = append(args, "bitrate="+strconv.Itoa(bitrate))
		}
		return args
	}
	return nil
}

func webmEncoderArgs(element string, bitrate int) []string {
	switch element {
	case "vp8enc", "vp9enc":
		// deadline=1 is realtime. The default is "best quality, no time
		// limit", which for a live desktop means seconds per frame.
		args := []string{"deadline=1", "cpu-used=8", "keyframe-max-dist=30"}
		if bitrate > 0 {
			args = append(args, "target-bitrate="+strconv.Itoa(bitrate*1000))
		}
		return args
	}
	return nil
}

// Command builds the capture process, with the portal descriptor passed to it
// and its own process group.
//
// The group matters: gst-launch spawns helpers, and signalling only the parent
// leaves them holding the portal session and the encoder.
func (o Options) Command(name string) (*exec.Cmd, error) {
	if o.FD == nil {
		return nil, fmt.Errorf("capture: no portal descriptor")
	}
	if o.Serial == 0 {
		return nil, fmt.Errorf("capture: no node serial; refusing to start a "+
			"pipeline that would connect to whatever it finds (node %d)", o.NodeID)
	}
	cmd := exec.Command(name, o.Args()...)
	// ExtraFiles[0] is descriptor 3 in the child. This is the whole reason the
	// child can use the portal's PipeWire remote at all.
	cmd.ExtraFiles = []*os.File{o.FD}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

// HLSContentType is what a receiver must be told to expect for an HLS stream.
const HLSContentType = "application/vnd.apple.mpegurl"
