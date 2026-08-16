// Cast state in the Omarchy bar.
//
// Omarchy's Quarto update replaced waybar with Quickshell, so a waybar module
// stops being displayed at all -- the JSON is still produced, but nothing reads
// it. This is the same indicator for the shell that actually runs.
//
// It polls `castr bar`, which is deliberately the same command the waybar
// module uses. That command NEVER spawns the daemon, so polling it cannot start
// anything or keep a daemon alive past its idle timeout.

import QtQuick
import Quickshell
import Quickshell.Io
import qs.Ui

BarWidget {
  id: root
  moduleName: "castr.indicator"

  // "idle" | "connecting" | "streaming" | "failed"
  property string state: "idle"
  property string tooltip: "Not casting"

  readonly property bool casting: state === "streaming"
  readonly property bool busy: state === "connecting"
  readonly property bool failed: state === "failed"

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  function refresh() {
    if (!statusProc.running)
      statusProc.running = true
  }

  Process {
    id: statusProc
    command: ["castr", "bar"]
    stdout: StdioCollector {
      onStreamFinished: {
        // A missing or half-written line must not wedge the indicator: fall
        // back to idle rather than showing a stale "streaming" forever.
        try {
          var data = JSON.parse(String(text || "").trim() || "{}")
          root.state = String(data["class"] || "idle")
          root.tooltip = String(data.tooltip || "Not casting")
        } catch (e) {
          root.state = "idle"
          root.tooltip = "Not casting"
        }
      }
    }
  }

  Timer {
    // Two seconds matches the waybar module's interval. The command is a
    // socket probe, not a scan, so this is cheap.
    interval: 2000
    running: true
    repeat: true
    triggeredOnStart: true
    onTriggered: root.refresh()
  }

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar

    // Deliberately visible in every state rather than hidden when idle: a
    // toggle you cannot see is a toggle you cannot find, and this one is how
    // you stop a cast.
    text: root.casting ? "󰄠" : "󰄡"
    active: root.casting || root.busy

    tooltipText: root.tooltip

    onPressed: function (b) {
      if (b === Qt.RightButton) {
        root.bar.run("castr stop")
        root.refresh()
      } else {
        root.bar.run("castr menu")
      }
    }
  }
}
