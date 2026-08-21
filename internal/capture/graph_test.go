package capture

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// dump builds pw-dump output in the shape the real daemon produces.
//
// The shape is the point. pw-dump reports application.process.id as a JSON
// NUMBER and links by node id, and a process is reachable only through its
// Client object -- nodes carry a client.id, not a pid. Both of those were got
// wrong here, and neither was caught, because the only real implementation of
// Graph had no test at all: every guard test used a fake.
func dump(pid int, clientID, gstNode, sourceNode uint32, sourceName, role string) []byte {
	objects := []map[string]any{
		{
			"id": clientID, "type": "PipeWire:Interface:Client",
			"info": map[string]any{"props": map[string]any{
				// A number, exactly as the daemon emits it.
				"application.process.id": pid,
				"application.name":       "gst-launch-1.0",
			}},
		},
		{
			"id": gstNode, "type": "PipeWire:Interface:Node",
			"info": map[string]any{"props": map[string]any{
				"client.id": clientID, "node.name": "gst-launch-1.0",
			}},
		},
		{
			"id": sourceNode, "type": "PipeWire:Interface:Node",
			"info": map[string]any{"props": map[string]any{
				"node.name": sourceName, "media.class": "Video/Source",
				"media.role": role, "object.serial": 4617,
			}},
		},
		{
			"id": 900, "type": "PipeWire:Interface:Link",
			"info": map[string]any{"props": map[string]any{
				"link.output.node": sourceNode, "link.input.node": gstNode,
			}},
		},
	}
	raw, err := json.Marshal(objects)
	if err != nil {
		panic(err)
	}
	return raw
}

func graphOf(raw []byte) *PipeWire {
	return &PipeWire{Command: func() ([]byte, error) { return raw, nil }}
}

func TestSourcesFeedingFindsTheSourceThroughTheClientObject(t *testing.T) {
	g := graphOf(dump(589176, 68, 76, 83, "xdg-desktop-portal-hyprland", ""))

	got, err := g.SourcesFeeding(589176)
	if err != nil {
		t.Fatalf("SourcesFeeding: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d sources, want 1", len(got))
	}
	if got[0].ID != 83 || got[0].Name != "xdg-desktop-portal-hyprland" {
		t.Errorf("got %+v", got[0])
	}
}

// pid_max is 4194304 on an ordinary Linux machine, and pids climb past a
// million within days of uptime. Matching the pid as TEXT stops working there,
// because a float64 formats as "1.048576e+06" -- and the guard then reports
// that a healthy pipeline captured nothing.
func TestSourcesFeedingWorksForPidsAboveOneMillion(t *testing.T) {
	for _, pid := range []int{999999, 1000000, 1048576, 4194303} {
		t.Run(fmt.Sprint(pid), func(t *testing.T) {
			g := graphOf(dump(pid, 68, 76, 83, "xdg-desktop-portal-hyprland", ""))

			got, err := g.SourcesFeeding(pid)
			if err != nil {
				t.Fatalf("SourcesFeeding: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("pid %d found %d sources, want 1", pid, len(got))
			}
		})
	}
}

func TestSourcesFeedingReportsACameraWithItsRole(t *testing.T) {
	g := graphOf(dump(1048576, 68, 76, 46, "v4l2_input.pci-0000_00_14.0", "Camera"))

	got, err := g.SourcesFeeding(1048576)
	if err != nil {
		t.Fatalf("SourcesFeeding: %v", err)
	}
	if len(got) != 1 || got[0].Role != "Camera" {
		t.Fatalf("got %+v, want a source carrying role=Camera", got)
	}
}

// Another process's capture is not this one's. Matching it would let one
// pipeline vouch for another.
func TestSourcesFeedingIgnoresAnotherProcess(t *testing.T) {
	g := graphOf(dump(1048576, 68, 76, 83, "xdg-desktop-portal-hyprland", ""))

	got, err := g.SourcesFeeding(999)
	if err != nil {
		t.Fatalf("SourcesFeeding: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v for an unrelated pid, want nothing", got)
	}
}

func TestSourcesFeedingFailsOnUnreadableOutput(t *testing.T) {
	g := &PipeWire{Command: func() ([]byte, error) { return []byte("not json"), nil }}
	if _, err := g.SourcesFeeding(1); err == nil {
		t.Fatal("unparseable pw-dump output was accepted")
	}

	g = &PipeWire{Command: func() ([]byte, error) { return nil, errors.New("no pw-dump") }}
	if _, err := g.SourcesFeeding(1); err == nil {
		t.Fatal("a missing pw-dump was accepted")
	}
}

func TestSerialOfReadsTheObjectSerial(t *testing.T) {
	g := graphOf(dump(1048576, 68, 76, 83, "xdg-desktop-portal-hyprland", ""))

	serial, err := g.SerialOf(83)
	if err != nil {
		t.Fatalf("SerialOf: %v", err)
	}
	if serial != 4617 {
		t.Errorf("SerialOf = %d, want 4617", serial)
	}
}

func TestSerialOfFailsForAMissingNode(t *testing.T) {
	g := graphOf(dump(1048576, 68, 76, 83, "portal", ""))
	if _, err := g.SerialOf(999); err == nil {
		t.Fatal("a node that does not exist reported a serial")
	}
}
