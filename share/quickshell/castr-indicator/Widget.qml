// Cast control in the Omarchy bar.
//
// Clicking the icon opens a panel built from the shell's own components, so it
// looks and behaves like the network and audio panels rather than the dmenu
// popup this project used before Omarchy moved to Quickshell.
//
// Everything it knows comes from three castr commands:
//
//   castr bar            polled every 2s; NEVER spawns a daemon, so polling
//                        cannot keep one alive past its idle timeout
//   castr list --json    receivers, read only while the panel is open
//   castr status --json  live casts, read only while the panel is open
//
// The panel parses JSON rather than scraping the human output, which would
// break the moment a column width changed.

import QtQuick
import Quickshell
import Quickshell.Io
import qs.Ui
import qs.Commons

Panel {
  id: root
  moduleName: "castr.indicator"
  ipcTarget: "castr.indicator"

  // ---- from `castr bar`, polled whether or not the panel is open ----
  property string castState: "idle"      // idle | connecting | streaming | failed
  property string tooltip: "Not casting"
  readonly property bool casting: castState === "streaming"
  readonly property bool busy: castState === "connecting"
  readonly property bool failed: castState === "failed"

  // ---- read only while the panel is open ----
  property var devices: []
  property var sessions: []
  property bool loading: false
  property string listError: ""
  property string actionDeviceId: ""     // the row with a request in flight
  property string actionMode: ""

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  function refreshStatus() {
    if (!statusProc.running) statusProc.running = true
  }

  function refreshPanel() {
    if (!root.opened) return
    root.loading = true
    if (!listProc.running) listProc.running = true
    if (!sessionsProc.running) sessionsProc.running = true
  }

  // A receiver's live session, or null. Keyed on device id, never on name:
  // two Apple TVs can share a name and only the id is unique.
  function sessionFor(deviceId) {
    for (var i = 0; i < root.sessions.length; i++)
      if (root.sessions[i].device_id === deviceId) return root.sessions[i]
    return null
  }

  function startCast(deviceId, mode) {
    root.actionDeviceId = deviceId
    root.actionMode = mode
    startProc.command = ["castr", "start", deviceId, mode]
    startProc.running = true
    // Closed straight away: a cast can take most of a minute to come up, and
    // the answer belongs on the bar and in a notification, not in a panel the
    // user is holding open waiting for something to happen.
    root.close()
  }

  function stopCast(deviceId) {
    root.actionDeviceId = deviceId
    stopProc.command = deviceId ? ["castr", "stop", deviceId] : ["castr", "stop"]
    stopProc.running = true
  }

  onOpenedChanged: {
    if (opened) refreshPanel()
    else { root.listError = ""; root.actionDeviceId = "" }
  }

  // ---------------------------------------------------------------- processes

  Process {
    id: statusProc
    command: ["castr", "bar"]
    stdout: StdioCollector {
      onStreamFinished: {
        // A half-written or missing line must not wedge the indicator: fall
        // back to idle rather than showing a stale "streaming" forever.
        try {
          var data = JSON.parse(String(text || "").trim() || "{}")
          root.castState = String(data["class"] || "idle")
          root.tooltip = String(data.tooltip || "Not casting")
        } catch (e) {
          root.castState = "idle"
          root.tooltip = "Not casting"
        }
      }
    }
  }

  Process {
    id: listProc
    command: ["castr", "list", "--json"]
    stdout: StdioCollector {
      onStreamFinished: {
        root.loading = false
        try {
          var data = JSON.parse(String(text || "").trim() || "{}")
          root.devices = data.devices || []
          root.listError = String(data.error || "")
        } catch (e) {
          root.devices = []
          root.listError = "could not read the receiver list"
        }
      }
    }
  }

  Process {
    id: sessionsProc
    command: ["castr", "status", "--json"]
    stdout: StdioCollector {
      onStreamFinished: {
        try {
          var data = JSON.parse(String(text || "").trim() || "{}")
          root.sessions = data.sessions || []
        } catch (e) {
          root.sessions = []
        }
      }
    }
  }

  Process {
    id: startProc
    onExited: { root.actionDeviceId = ""; root.actionMode = ""; root.refreshStatus() }
  }

  Process {
    id: stopProc
    onExited: { root.actionDeviceId = ""; root.refreshStatus(); root.refreshPanel() }
  }

  Timer {
    // Two seconds matches the waybar module. `castr bar` is a socket probe,
    // not a scan, so this is cheap.
    interval: 2000
    running: true
    repeat: true
    triggeredOnStart: true
    onTriggered: root.refreshStatus()
  }

  Timer {
    // While the panel is open, keep the list fresh: mDNS turns receivers up a
    // few seconds apart, and a list that never grows looks broken.
    interval: 4000
    running: root.opened
    repeat: true
    onTriggered: root.refreshPanel()
  }

  // ------------------------------------------------------------------ the bar

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar

    // Visible in every state rather than hidden when idle: a control you
    // cannot see is a control you cannot find, and this one stops the cast.
    text: root.casting ? "󰄠" : "󰄡"
    active: root.casting || root.busy
    tooltipText: root.tooltip

    onPressed: function(b) {
      if (b === Qt.RightButton) {
        // Right-click still stops immediately -- muscle memory from the old
        // widget, and quicker than opening the panel to do it.
        if (root.casting || root.busy) root.stopCast("")
      } else {
        root.toggle()
      }
    }
  }

  // ---------------------------------------------------------------- the panel

  KeyboardPanel {
    id: panel
    anchorItem: button
    owner: root
    bar: root.bar
    open: root.opened
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(400))
    contentHeight: panel.fittedContentHeight(column.implicitHeight)

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      onCloseRequested: root.close()
      onTabRequested: function(direction) { root.switchPanel(direction) }

      Column {
        id: column
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.top: parent.top
        spacing: Style.space(12)

        // ---------- hero: what is happening right now ----------
        Item {
          width: parent.width
          implicitHeight: Math.max(heroIcon.implicitHeight, heroLabels.implicitHeight)

          Text {
            id: heroIcon
            text: root.casting ? "󰄠" : "󰄡"
            // The theme's palette, not invented colours: a theme sets
            // foreground/accent/urgent/muted, and anything else ignores it.
            color: root.failed ? Color.urgent
                 : root.casting ? Color.accent
                 : root.busy ? Qt.darker(Color.accent, 1.25)
                 : Qt.darker(root.bar.foreground, 1.5)
            font.family: root.bar.fontFamily
            font.pixelSize: Style.font.display
            anchors.left: parent.left
            anchors.verticalCenter: parent.verticalCenter
            Behavior on color { ColorAnimation { duration: 200 } }
          }

          Column {
            id: heroLabels
            anchors.left: heroIcon.right
            anchors.leftMargin: Style.space(14)
            anchors.right: parent.right
            anchors.verticalCenter: parent.verticalCenter
            spacing: Style.space(2)

            Text {
              text: "Cast"
              color: root.bar.foreground
              font.family: root.bar.fontFamily
              font.pixelSize: Style.font.title
              font.bold: true
              elide: Text.ElideRight
              width: parent.width
            }

            Text {
              // The first line of the tooltip is the human sentence; the rest
              // is the click hint, which is noise inside the panel itself.
              text: String(root.tooltip).split("\n")[0]
              color: Qt.darker(root.bar.foreground, 1.4)
              font.family: root.bar.fontFamily
              font.pixelSize: Style.font.caption
              wrapMode: Text.WordWrap
              width: parent.width
            }
          }
        }

        // ---------- live casts, each with a stop ----------
        Repeater {
          model: root.sessions
          delegate: Item {
            required property var modelData
            width: column.width
            implicitHeight: Style.space(34)

            Rectangle {
              anchors.fill: parent
              radius: Style.space(6)
              color: Qt.rgba(root.bar.foreground.r, root.bar.foreground.g,
                             root.bar.foreground.b, 0.06)
            }

            Text {
              anchors.left: parent.left
              anchors.leftMargin: Style.space(10)
              anchors.right: stopButton.left
              anchors.rightMargin: Style.space(8)
              anchors.verticalCenter: parent.verticalCenter
              text: modelData.name + "  ·  " + modelData.mode
              color: root.bar.foreground
              font.family: root.bar.fontFamily
              font.pixelSize: Style.font.body
              elide: Text.ElideRight
            }

            PanelActionButton {
              id: stopButton
              anchors.right: parent.right
              anchors.rightMargin: Style.space(6)
              anchors.verticalCenter: parent.verticalCenter
              iconText: "󰓛"
              tooltipText: "Stop casting to " + modelData.name
              foreground: root.bar.foreground
              hoverColor: Color.urgent
              fontFamily: root.bar.fontFamily
              onClicked: root.stopCast(modelData.device_id)
            }
          }
        }

        PanelSeparator {
          width: parent.width
          foreground: root.bar.foreground
          visible: root.sessions.length > 0
        }

        // ---------- receivers ----------
        PanelSectionHeader {
          width: parent.width
          text: root.devices.length > 0 ? "RECEIVERS"
              : root.loading ? "LOOKING FOR RECEIVERS"
              : "NO RECEIVERS FOUND"
          foreground: root.bar.foreground
          fontFamily: root.bar.fontFamily
        }

        Text {
          width: parent.width
          visible: root.listError !== ""
          text: root.listError
          color: Color.urgent
          font.family: root.bar.fontFamily
          font.pixelSize: Style.font.caption
          wrapMode: Text.WordWrap
        }

        Text {
          width: parent.width
          visible: root.listError === "" && !root.loading && root.devices.length === 0
          text: "Nothing is advertising AirPlay on this network. A receiver that "
              + "does not answer mDNS can still be added with: castr add <address>"
          color: Qt.darker(root.bar.foreground, 1.4)
          font.family: root.bar.fontFamily
          font.pixelSize: Style.font.caption
          wrapMode: Text.WordWrap
        }

        Repeater {
          model: root.devices
          delegate: Item {
            id: row
            required property var modelData
            readonly property var liveSession: root.sessionFor(modelData.id)
            readonly property bool live: !!liveSession
            readonly property bool pending: root.actionDeviceId === modelData.id
            width: column.width
            implicitHeight: live ? 0 : Style.space(38)
            visible: !live   // a live receiver is already shown above

            Rectangle {
              anchors.fill: parent
              radius: Style.space(6)
              color: hover.containsMouse
                ? Qt.rgba(root.bar.foreground.r, root.bar.foreground.g,
                          root.bar.foreground.b, 0.08)
                : "transparent"
              Behavior on color { ColorAnimation { duration: 120 } }
            }

            MouseArea {
              id: hover
              anchors.fill: parent
              hoverEnabled: true
              acceptedButtons: Qt.NoButton
            }

            Column {
              anchors.left: parent.left
              anchors.leftMargin: Style.space(10)
              anchors.right: actions.left
              anchors.rightMargin: Style.space(8)
              anchors.verticalCenter: parent.verticalCenter
              spacing: Style.space(1)

              Text {
                text: modelData.name
                color: root.bar.foreground
                font.family: root.bar.fontFamily
                font.pixelSize: Style.font.body
                elide: Text.ElideRight
                width: parent.width
              }

              Text {
                // Model when the receiver advertises one, address otherwise --
                // which is exactly the case for a hand-added receiver.
                text: row.pending
                  ? (root.actionMode === "extend" ? "Extending…" : "Mirroring…")
                  : (modelData.model ? modelData.model : modelData.address)
                color: Qt.darker(root.bar.foreground, 1.45)
                font.family: root.bar.fontFamily
                font.pixelSize: Style.font.caption
                elide: Text.ElideRight
                width: parent.width
              }
            }

            Row {
              id: actions
              anchors.right: parent.right
              anchors.rightMargin: Style.space(6)
              anchors.verticalCenter: parent.verticalCenter
              spacing: Style.space(4)
              opacity: row.pending ? 0.4 : 1

              PanelActionButton {
                iconText: "󰍹"
                tooltipText: "Mirror this screen to " + modelData.name
                foreground: root.bar.foreground
                hoverColor: Color.accent
                fontFamily: root.bar.fontFamily
                enabled: !row.pending
                onClicked: root.startCast(modelData.id, "mirror")
              }

              PanelActionButton {
                iconText: "󰍺"
                // Named because picking the wrong output at the screen-share
                // prompt silently produces a mirror, and the portal REMEMBERS
                // that choice for every later cast.
                tooltipText: "Extend onto " + modelData.name
                           + " (pick the castr output if asked what to share)"
                foreground: root.bar.foreground
                hoverColor: Color.accent
                fontFamily: root.bar.fontFamily
                enabled: !row.pending
                onClicked: root.startCast(modelData.id, "extend")
              }
            }
          }
        }
      }
    }
  }
}
