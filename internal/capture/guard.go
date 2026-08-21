package capture

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrWrongSource means the pipeline captured something other than the node the
// portal granted. It exists as a distinct error because the caller's duty on
// seeing it is different from any other failure: stop immediately, and say
// what was about to be sent.
var ErrWrongSource = errors.New("capture: the pipeline is not capturing the granted screen")

// ErrNoSource means nothing was ever linked. A pipeline producing no video is
// a failure, not a quiet success.
var ErrNoSource = errors.New("capture: the pipeline captured nothing")

// Guard verifies that a running pipeline is capturing the node the portal
// granted, and nothing else.
//
// This check is not defensive programming. No pipewiresrc configuration fails
// safe: every one that captures the granted node will silently substitute the
// default video device -- in practice the built-in webcam -- if the node does
// not match. That was measured on this machine, and it is documented in
// docs/capture-safety.md. omarchy-cast shipped the substitution as a working
// feature. The guard is where the guarantee actually lives.
type Guard struct {
	Graph Graph
	// Granted is the node id the portal handed over. Anything else is a
	// failure, whatever it is.
	Granted uint32
	// Timeout is how long to wait for a link to appear. Encoders take a moment
	// to negotiate, so an immediate check reports no source for a pipeline
	// that is merely starting.
	Timeout time.Duration
	// Interval is how often to look.
	Interval time.Duration
	// Sleep is injectable so tests do not wait in real seconds.
	Sleep func(time.Duration)
}

// Verify blocks until the pipeline is confirmed to be capturing the granted
// node, or fails.
//
// Sampling repeatedly rather than once is not a nicety. A single sample taken
// before the link exists reports "nothing connected", which reads as clean and
// is merely early -- that mistake was made twice while investigating this.
func (g *Guard) Verify(ctx context.Context, pid int) error {
	timeout, interval := g.Timeout, g.Interval
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	sleep := g.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	// Node 0 is what an unreadable link reports, so accepting it as "granted"
	// would make an unidentifiable source pass the check that exists to
	// identify sources.
	if g.Granted == 0 {
		return fmt.Errorf("%w: castr was not told which node it may capture", ErrWrongSource)
	}

	deadline := time.Now().Add(timeout)
	for {
		sources, err := g.Graph.SourcesFeeding(pid)
		if err != nil {
			// An unreadable graph is not permission to continue. If castr
			// cannot tell what it is capturing, it does not capture.
			return fmt.Errorf("%w: %v", ErrNoSource, err)
		}

		// Checked before the "did we find it" test, so a pipeline linked to
		// BOTH the screen and a camera fails rather than passing on the
		// strength of the half that is correct.
		if wrong := g.wrongSources(sources); len(wrong) > 0 {
			return fmt.Errorf("%w: it is capturing %s. Nothing has been sent",
				ErrWrongSource, strings.Join(wrong, ", "))
		}
		if len(sources) > 0 {
			return nil
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: nothing was connected to it within %s",
				ErrNoSource, timeout)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sleep(interval)
	}
}

func (g *Guard) wrongSources(sources []Node) []string {
	var wrong []string
	for _, n := range sources {
		if n.ID != g.Granted {
			wrong = append(wrong, n.Describe())
		}
	}
	return wrong
}

// Recheck takes a single sample and reports only a source that is not the
// granted one.
//
// Verify answers "has this started correctly". Recheck answers "is it still
// what it was", which is a different question and needs a different answer to
// an empty graph: a momentarily missing link mid-session is not evidence of a
// substitution, and tearing a cast down for one would make the guard the most
// common cause of failure rather than a defence. A capture that has genuinely
// stopped is caught by the pipeline exiting.
func (g *Guard) Recheck(pid int) error {
	sources, err := g.Graph.SourcesFeeding(pid)
	if err != nil {
		// Unreadable is not proof of wrongdoing, and Verify already
		// established what this pipeline was capturing.
		return nil
	}
	if wrong := g.wrongSources(sources); len(wrong) > 0 {
		return fmt.Errorf("%w: it changed to %s",
			ErrWrongSource, strings.Join(wrong, ", "))
	}
	return nil
}
