// Package capture runs the screen-capture pipeline and proves what it is
// capturing.
package capture

import (
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

// Node is a PipeWire node feeding the capture pipeline.
type Node struct {
	ID    uint32
	Name  string
	Class string
	// Role is what exposes the hazard: a substituted device reports
	// media.role=Camera.
	Role string
}

// Describe renders a node the way it should appear in a failure message: the
// user needs to know what castr was about to send to a television.
func (n Node) Describe() string {
	s := fmt.Sprintf("node %d %q", n.ID, n.Name)
	if n.Role != "" {
		s += " role=" + n.Role
	}
	return s
}

// Graph reports which source nodes feed a given process.
//
// It is an interface so the guard can be tested without a running PipeWire.
// Every test of the safety rule would otherwise need a compositor, a portal
// grant and a webcam, which is exactly the kind of test that never runs.
type Graph interface {
	SourcesFeeding(pid int) ([]Node, error)
}

// PipeWire reads the live graph with pw-dump.
type PipeWire struct {
	// Command is the dump command, injectable for tests.
	Command func() ([]byte, error)
}

// NewPipeWire returns a Graph backed by pw-dump.
func NewPipeWire() *PipeWire {
	return &PipeWire{Command: func() ([]byte, error) {
		return exec.Command("pw-dump").Output()
	}}
}

type dumpObject struct {
	ID   uint32 `json:"id"`
	Type string `json:"type"`
	Info struct {
		Props map[string]any `json:"props"`
	} `json:"info"`
}

// SourcesFeeding reports the source nodes linked into the process's nodes.
func (p *PipeWire) SourcesFeeding(pid int) ([]Node, error) {
	raw, err := p.Command()
	if err != nil {
		return nil, fmt.Errorf("reading the PipeWire graph: %w", err)
	}

	var objects []dumpObject
	if err := json.Unmarshal(raw, &objects); err != nil {
		return nil, fmt.Errorf("reading the PipeWire graph: %w", err)
	}

	props := make(map[uint32]map[string]any, len(objects))
	for _, o := range objects {
		props[o.ID] = o.Info.Props
	}

	// A process is found through its PipeWire CLIENT, not its nodes. Only the
	// client object carries application.process.id; the nodes carry a
	// client.id pointing back at it. Matching the pid against nodes directly
	// finds nothing -- and a check that silently finds nothing is worse than
	// no check, because it reports the pipeline captured nothing while the
	// pipeline is running fine.
	// Compared as a NUMBER, not as text.
	//
	// pw-dump emits application.process.id as a JSON number, so it arrives
	// here as a float64, and fmt.Sprint on a float64 switches to scientific
	// notation at a million: pid 1048576 formats as "1.048576e+06", which
	// matches no pid string ever. pid_max on this machine is 4194304, so a
	// string comparison silently stops matching for 76% of the pid space --
	// and the guard then reports that a perfectly good pipeline captured
	// nothing, for reasons nobody could find.
	clients := map[uint32]bool{}
	for _, o := range objects {
		if strings.HasSuffix(o.Type, "Client") &&
			propUint(o.Info.Props, "application.process.id") == uint32(pid) {
			clients[o.ID] = true
		}
	}
	ours := map[uint32]bool{}
	for _, o := range objects {
		if strings.HasSuffix(o.Type, "Node") && clients[propUint(o.Info.Props, "client.id")] {
			ours[o.ID] = true
		}
	}

	var sources []Node
	seen := map[uint32]bool{}
	for _, o := range objects {
		if !strings.HasSuffix(o.Type, "Link") {
			continue
		}
		input := propUint(o.Info.Props, "link.input.node")
		output := propUint(o.Info.Props, "link.output.node")
		if !ours[input] {
			continue
		}
		if seen[output] {
			continue
		}
		seen[output] = true
		sources = append(sources, Node{
			ID:    output,
			Name:  propString(props[output], "node.name"),
			Class: propString(props[output], "media.class"),
			Role:  propString(props[output], "media.role"),
		})
	}
	return sources, nil
}

func propString(props map[string]any, key string) string {
	v, ok := props[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func propUint(props map[string]any, key string) uint32 {
	switch v := props[key].(type) {
	case float64:
		// Out-of-range values would wrap silently into a plausible id, so a
		// value that cannot be a PipeWire object id is reported as absent.
		if v < 0 || v > math.MaxUint32 {
			return 0
		}
		return uint32(v)
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return 0
		}
		return uint32(n)
	}
	return 0
}

// SerialOf reports a node's object.serial, which is what pipewiresrc's
// target-object matches. A node id will not match it, and passing one there is
// how omarchy-cast ended up streaming a webcam.
func (p *PipeWire) SerialOf(node uint32) (uint32, error) {
	raw, err := p.Command()
	if err != nil {
		return 0, fmt.Errorf("reading the PipeWire graph: %w", err)
	}
	var objects []dumpObject
	if err := json.Unmarshal(raw, &objects); err != nil {
		return 0, fmt.Errorf("reading the PipeWire graph: %w", err)
	}
	for _, o := range objects {
		if o.ID != node {
			continue
		}
		if serial := propUint(o.Info.Props, "object.serial"); serial != 0 {
			return serial, nil
		}
		return 0, fmt.Errorf("PipeWire node %d has no object.serial", node)
	}
	return 0, fmt.Errorf("PipeWire has no node %d; the portal session may have ended", node)
}
