pragma ComponentBehavior: Bound
import QtQuick
import Quickshell
import Quickshell.Wayland
import Quickshell.Io
import qs.Commons
import qs.Ui

// SudoGate review panel and data owner. The bar badge and this panel read the
// same state: sudogate-server writes $XDG_RUNTIME_DIR/sudogate.state as one
// JSON object ({updated, timeout_sec, pending[]}) on every queue change; we watch it with
// inotifywait (event-driven, zero polling — the mail widget pattern) and re-cat
// on every event. Approve/deny shell out to the sudogate-server binary's
// control subcommands; the password travels via stdin, never argv.
//
// Queue policy mirrors the terminal `review`: oldest entry first. When the
// entry under review disappears from the state file (approved elsewhere,
// denied, or deadline hit on the server), we say so and roll to the next —
// the panel never shows a dead request. That is the deadline↔popup linkage.
//
// demo: true renders two canned requests (no server, no files) so the visuals
// can be previewed without touching the real queue.
Panel {
  id: root
  moduleName: "tomaswade.sudogate"
  ipcTarget: "tomaswade.sudogate"

  property var anchorItem: null

  // ---- settings -------------------------------------------------------------
  readonly property string runtimeDirSetting: String(setting("runtimeDir", "") || "")
  readonly property string serverBinSetting: String(setting("serverBin", "") || "")
  readonly property bool demo: setting("demo", false) === true
  readonly property int timeoutSec: Math.max(10, parseInt(setting("timeoutSec", 120), 10) || 120)

  readonly property string runtimeDir: runtimeDirSetting !== ""
    ? runtimeDirSetting
    : (Quickshell.env("XDG_RUNTIME_DIR") || "/run/user/1000")
  readonly property string statePath: runtimeDir + "/sudogate.state"
  // 所在目录（inotifywait 监听目录、按文件名过滤，见 watchProc 注释）
  readonly property string stateDir: {
    const i = statePath.lastIndexOf("/")
    return i > 0 ? statePath.substring(0, i) : "."
  }

  // ---- theme ------------------------------------------------------------------
  readonly property color fg: Color.popups.text
  readonly property color dim: Qt.darker(Color.popups.text, 1.55)
  readonly property color urgentColor: bar ? bar.urgent : Color.urgent
  readonly property color okColor: bar ? bar.barForeground : Color.foreground
  readonly property string fontFam: bar ? bar.fontFamily : Style.font.family

  // ---- data ---------------------------------------------------------------------
  // Sorted oldest-first: [{id, host, user, command, cwd, createdMs}]
  property var pending: []
  property bool stateKnown: false
  property string currentId: ""
  property string notice: ""
  property real nowMs: Date.now()

  // Action in flight (approve or deny); guards double-clicks.
  property bool actionBusy: false
  property string actionError: ""

  readonly property var queue: demo ? demoPending : pending
  readonly property int count: queue.length
  readonly property var current: {
    for (var i = 0; i < queue.length; i++)
      if (queue[i].id === currentId) return queue[i]
    return queue[0] || null
  }

  readonly property int currentIndex: {
    for (var i = 0; i < queue.length; i++)
      if (queue[i].id === current.id) return i
    return 0
  }

  readonly property var others: {
    var out = []
    for (var i = 0; i < queue.length; i++)
      if (!current || queue[i].id !== current.id) out.push(queue[i])
    return out
  }

  // Seconds since the request was created, reticked from the wall clock so the
  // countdown advances between state-file writes.
  readonly property real currentAgeSec: current && current.createdMs > 0
    ? Math.max(0, (nowMs - current.createdMs) / 1000) : 0
  readonly property real remainSec: Math.max(0, timeoutSec - currentAgeSec)

  readonly property string barTooltip: count > 0
    ? "SudoGate 提权审阅：" + count + " 条待批"
    : "SudoGate：无待批"

  // ---- server binary resolution ------------------------------------------------
  // Discovery happens at exec time inside the shell wrapper: the first
  // executable candidate wins. No QML-side probe state machine to race with.
  readonly property var serverBinCandidates: {
    var home = Quickshell.env("HOME") || "/home/user"
    var list = [
      home + "/.local/bin/sudogate-server",
      "/usr/local/bin/sudogate-server",
    ]
    if (serverBinSetting !== "") list = [serverBinSetting].concat(list)
    return list
  }

  // id first, then every candidate; the wrapper execs the first executable.
  function actionCommand(sub, id) {
    return ["/bin/sh", "-c",
      'id="$1"; shift; for p in "$@"; do [ -x "$p" ] && exec "$p" ' + sub + ' -id "$id"; done; echo "sudogate-server 可执行文件未找到（检查插件设置 serverBin）" >&2; exit 127',
      "-"].concat([id]).concat(serverBinCandidates)
  }

  // ---- demo payload -------------------------------------------------------------
  readonly property var demoPending: [
    {
      id: "demo01",
      host: "demo-host",
      user: "demo",
      command: "systemctl restart syncthing.service",
      cwd: "/home/demo",
      createdMs: Date.now() - 23 * 1000
    },
    {
      id: "demo02",
      host: "demo-host",
      user: "demo",
      command: "pacman -Syu --noconfirm",
      cwd: "/home/demo/tmp",
      createdMs: Date.now() - 81 * 1000
    }
  ]

  // ---- state file plumbing -------------------------------------------------------
  // inotify events storm when several requests land at once; debounce reads
  // and re-read once after the current cat finishes so no update is lost to
  // a busy one-shot process.
  property bool catQueued: false

  function refresh() {
    if (demo) {
      stateKnown = true
      return
    }
    if (catProc.running) {
      catQueued = true
      return
    }
    catProc.command = ["/bin/sh", "-c", 'exec "$0" "$@"', "/usr/bin/cat", statePath]
    catProc.running = true
  }

  function parseState(text) {
    var trimmed = text.trim()
    if (trimmed === "") return
    var j
    try {
      j = JSON.parse(trimmed)
    } catch (e) {
      return
    }
    var updatedMs = Date.parse(j.updated || "")
    if (!(updatedMs > 0)) updatedMs = Date.now()
    var list = []
    if (Array.isArray(j.pending)) {
      for (var i = 0; i < j.pending.length; i++) {
        var e = j.pending[i]
        if (!e || !e.id) continue
        var age = Number(e.age_sec)
        if (!(age >= 0)) age = 0
        list.push({
          id: String(e.id),
          host: String(e.host || "?"),
          user: String(e.user || "?"),
          command: String(e.command || ""),
          cwd: String(e.cwd || "?"),
          createdMs: updatedMs - age * 1000
        })
      }
    }
    list.sort(function(a, b) { return a.createdMs - b.createdMs })
    var oldId = currentId
    var wasReviewing = oldId !== "" && root.opened
    var stillThere = false
    for (var k = 0; k < list.length; k++)
      if (list[k].id === oldId) { stillThere = true; break }
    if (!stillThere) {
      currentId = list.length > 0 ? list[0].id : ""
      // The entry under review vanished (timed out or handled elsewhere).
      // A half-typed password belongs to THAT entry — not to whatever rolls
      // in next. Clear it, or a blind Enter approves a request the human
      // never reviewed. That is the approval-swap hazard under concurrency.
      pwField.text = ""
      if (wasReviewing && oldId !== "" && actionError === "" && !actionBusy
          && Date.now() - lastActionOkAt > 1500)
        notice = list.length > 0
          ? "上一条已超时或已被处理，已切到下一条（密码框已清空）"
          : "请求已全部处理完"
    }
    pending = list
    stateKnown = true
  }

  // ---- actions ---------------------------------------------------------------------
  function approve() {
    if (actionBusy || !current) return
    if (pwField.text === "") {
      actionError = "密码为空：批准需要输入密码（拒绝请点“拒绝”）"
      return
    }
    actionBusy = true
    actionError = ""
    notice = ""
    approveProc.password = pwField.text
    approveProc.command = actionCommand("approve", current.id)
    approveProc.start()
  }

  function deny() {
    if (actionBusy || !current) return
    actionBusy = true
    actionError = ""
    notice = ""
    denyProc.command = actionCommand("deny", current.id)
    denyProc.start()
  }

  function actionDone(ok, message) {
    actionBusy = false
    if (ok) {
      pwField.text = ""
      notice = message
      actionError = ""
      lastActionOkAt = Date.now()
    } else {
      actionError = message
    }
  }

  // Guard so a queue-refresh right after a successful approve/deny does not
  // overwrite the "已批准，密码已送达" feedback with the generic
  // "上一条已超时或已被处理" rollover notice.
  property real lastActionOkAt: 0

  // Approve: password goes in via stdin (RunApprove reads one line), so it
  // never appears in /proc/*/cmdline. Result finalization pairs onExited with
  // the stdout/stderr collectors (the glm-quota pattern) so error text is
  // complete before we report.
  Process {
    id: approveProc

    property string password: ""
    property bool startedSeen: false
    property bool exitedDone: false
    property int exitCode: -1
    property string errText: ""
    property string outText: ""

    function start() {
      startedSeen = false
      exitedDone = false
      exitCode = -1
      errText = ""
      outText = ""
      stdinEnabled = true
      running = true
    }

    command: []
    running: false
    stdinEnabled: false

    onStarted: {
      startedSeen = true
      if (password !== "") {
        write(password + "\n")
        password = ""
      }
    }

    stdout: StdioCollector {
      waitForEnd: true

      onStreamFinished: {
        approveProc.outText = text.trim()
        approveProc.maybeFinish()
      }
    }

    stderr: StdioCollector {
      waitForEnd: true

      onStreamFinished: {
        approveProc.errText = text.trim()
        approveProc.maybeFinish()
      }
    }

    onExited: function(code) {
      exitedDone = true
      exitCode = code
      maybeFinish()
    }

    function maybeFinish() {
      if (!exitedDone) return
      // Collectors may fire on stopped streams even without output; give them
      // a tick to land, then settle.
      Qt.callLater(function() {
        if (!approveProc.running) {
          var ok = approveProc.exitCode === 0
          var detail = approveProc.errText !== "" ? approveProc.errText : approveProc.outText
          var msg = ok ? "已批准，密码已送达"
            : "批准失败: " + (detail !== "" ? detail : "exit " + approveProc.exitCode)
          root.actionDone(ok, msg)
        }
      })
    }

    // A missing binary gives neither started nor exited — Quickshell drops
    // running back to false silently; un-stick the panel in that case.
    onRunningChanged: {
      if (!running && !startedSeen)
        root.actionDone(false, "无法启动 /bin/sh（不可能的失败）")
    }
  }

  Process {
    id: denyProc

    property bool startedSeen: false
    property bool exitedDone: false
    property int exitCode: -1
    property string errText: ""
    property string outText: ""

    function start() {
      startedSeen = false
      exitedDone = false
      exitCode = -1
      errText = ""
      outText = ""
      running = true
    }

    command: []
    running: false

    stdout: StdioCollector {
      waitForEnd: true

      onStreamFinished: {
        denyProc.outText = text.trim()
        denyProc.maybeFinish()
      }
    }

    stderr: StdioCollector {
      waitForEnd: true

      onStreamFinished: {
        denyProc.errText = text.trim()
        denyProc.maybeFinish()
      }
    }

    onExited: function(code) {
      exitedDone = true
      exitCode = code
      maybeFinish()
    }

    function maybeFinish() {
      if (!exitedDone) return

      Qt.callLater(function() {
        if (!denyProc.running) {
          var ok = denyProc.exitCode === 0
          var detail = denyProc.errText !== "" ? denyProc.errText : denyProc.outText
          var msg = ok ? "已拒绝"
            : "拒绝失败: " + (detail !== "" ? detail : "exit " + denyProc.exitCode)
          root.actionDone(ok, msg)
        }
      })
    }

    onRunningChanged: {
      if (!running && !startedSeen)
        root.actionDone(false, "无法启动 /bin/sh（不可能的失败）")
    }
  }

  // One-shot cat of the state file.
  Process {
    id: catProc

    command: []
    running: false

    stdout: StdioCollector {
      waitForEnd: true

      onStreamFinished: {
        if (text.trim() === "") root.stateKnown = false
        else root.parseState(text)
      }
    }

    onExited: function(exitCode) {
      if (exitCode !== 0) root.stateKnown = false
      if (catQueued) {
        catQueued = false
        Qt.callLater(refresh)
      }
    }
  }

  // inotifywait on the state file's DIRECTORY, filename filtered in QML:
  // watching the file inode breaks under tmp+rename atomic writes (move_self
  // fires once, then the watch goes permanently deaf — the process never
  // exits, so the retry timer never runs). Directory watch + moved_to/create
  // survives any write strategy. No shell wrapper / pipeline either:
  // Quickshell's Process kills only its direct child, so `sh -c 'a | b'`
  // would orphan the inotifywait/grep grandchildren on reload — argv mode
  // (mail widget pattern) keeps the worker as the direct child. Bursts
  // coalesce through the 100ms debounce; no polling timer anywhere.
  Process {
    id: watchProc

    command: ["/usr/bin/inotifywait", "-m", "-q", "-e", "modify,create,moved_to", "--format", "%f", root.stateDir]
    running: !root.demo

    stdout: SplitParser {
      splitMarker: "\n"

      // 信号参数必须以具名函数形式接收（裸表达式引用 read 会拿到
      // undefined，过滤永远不通过——面板失聪）
      onRead: function(data) {
        if (data.trim() === "sudogate.state") catDebounce.restart()
      }
    }

    onExited: watchRetryTimer.restart()
  }

  Timer {
    id: catDebounce

    interval: 100
    onTriggered: root.refresh()
  }

  Timer {
    id: watchRetryTimer

    interval: 2000
    onTriggered: {
      if (!root.demo) {
        root.refresh()
        watchProc.running = true
      }
    }
  }

  // Countdown heartbeat: 1s while the panel shows a request.
  Timer {
    interval: 1000
    running: root.opened && root.current !== null
    repeat: true
    triggeredOnStart: true
    onTriggered: root.nowMs = Date.now()
  }

  onOpenedChanged: {
    if (opened) {
      nowMs = Date.now()
      notice = ""
      actionError = ""
      refresh()
      // Focus the password field right away: the Exclusive layer owns the
      // keyboard from map-time (polkit-style modal), so opening the panel
      // means the human can type immediately. Enter approves, Tab walks
      // 拒绝/批准.
      Qt.callLater(function() {
        if (root.opened) pwField.forceActiveFocus()
      })
    }
  }

  Component.onCompleted: refresh()

  // ---- card --------------------------------------------------------------------
  // Modal dialog, the menu/polkit pattern: a full-screen overlay layer with a
  // CONSTANT WlrKeyboardFocus.Exclusive — the surface owns the keyboard from
  // the moment it maps, so opening the panel means the password field is
  // immediately typeable, no click needed. (The shared KeyboardPanel drops to
  // OnDemand after a 75ms prime and 0.3.1 does not reliably re-apply dynamic
  // keyboardFocus changes to an already-mapped surface; a constant Exclusive
  // is what omarchy-menu uses for its search field.) Scrim click or Esc
  // dismisses and releases the keyboard.
  PanelWindow {
    id: pwin
    visible: root.opened
    anchors {
      top: true
      bottom: true
      left: true
      right: true
    }
    color: "transparent"
    exclusionMode: ExclusionMode.Ignore
    WlrLayershell.namespace: "tomaswade.sudogate"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.keyboardFocus: WlrKeyboardFocus.Exclusive

    // Scrim: dim the session and dismiss on any click outside the card.
    Rectangle {
      anchors.fill: parent
      color: Color.polkit.scrim

      MouseArea {
        anchors.fill: parent
        onClicked: root.close()
      }
    }

    // The card, top-right under the bar.
    Rectangle {
      id: card

      readonly property real maxW: pwin.width - Style.gapsOut * 2
      readonly property real maxH: pwin.height - (root.bar ? root.bar.barSize : 43) - Style.gapsOut * 2

      width: Math.min(Style.space(430) + Style.space(20), maxW)
      height: Math.min(contentScroll2.implicitHeight2 + Style.space(24), maxH)
      anchors.right: parent.right
      anchors.rightMargin: Style.gapsOut
      anchors.top: parent.top
      anchors.topMargin: (root.bar ? root.bar.barSize : 43) + Math.round(Style.gapsOut / 2)
      radius: Style.cornerRadius
      color: Color.polkit.background
      border.width: 1
      border.color: Color.polkit.border

      // Swallow clicks inside the card so the scrim does not close.
      MouseArea {
        anchors.fill: parent
        onClicked: {}
      }

      // Esc fallback when focus is not inside the text field.
      Keys.onEscapePressed: root.close()

      Flickable {
        id: contentScroll2

        // Height measure for the card (content + breathing room).
        readonly property real implicitHeight2: contentColumn.implicitHeight

        anchors.fill: parent
        anchors.margins: Style.space(12)
        contentWidth: width
        contentHeight: contentColumn.implicitHeight
        clip: true
        boundsBehavior: Flickable.StopAtBounds
        interactive: contentHeight > height

        Column {
          id: contentColumn
          width: contentScroll2.width
          spacing: Style.space(12)

          // ---- header ----------------------------------------------------------
          Item {
            width: parent.width
            implicitHeight: headerRow.implicitHeight

            Row {
              id: headerRow
              spacing: Style.space(10)

              Text {
                text: "SudoGate 提权请求"
                color: root.fg
                font.family: root.fontFam
                font.pixelSize: Style.font.display * 0.75
                font.bold: true
                anchors.verticalCenter: parent.verticalCenter
              }

              Rectangle {
                visible: root.count > 1
                width: queueLabel.implicitWidth + Style.space(10)
                height: queueLabel.implicitHeight + Style.space(4)
                radius: height / 2
                color: Qt.rgba(root.urgentColor.r, root.urgentColor.g, root.urgentColor.b, 0.12)
                border.width: 1
                border.color: Qt.rgba(root.urgentColor.r, root.urgentColor.g, root.urgentColor.b, 0.35)
                anchors.verticalCenter: parent.verticalCenter

                Text {
                  id: queueLabel
                  anchors.centerIn: parent
                  text: "第 " + (root.currentIndex + 1) + " / " + root.count + " 条"
                  color: root.urgentColor
                  font.family: root.fontFam
                  font.pixelSize: Style.font.caption
                }
              }
            }

            PanelActionButton {
              anchors.right: parent.right
              iconText: "✕"
              foreground: root.fg
              fontFamily: root.fontFam
              tooltipText: "关闭 (Esc)"
              onClicked: root.close()
            }
          }

          // ---- review card -----------------------------------------------------
          Rectangle {
            width: parent.width
            implicitHeight: detailCol.implicitHeight + Style.space(20)
            radius: Style.cornerRadius
            color: Qt.rgba(root.fg.r, root.fg.g, root.fg.b, 0.05)
            border.width: 1
            border.color: Qt.rgba(root.fg.r, root.fg.g, root.fg.b, 0.18)

            Column {
              id: detailCol
              x: Style.space(10)
              width: parent.width - Style.space(20)
              anchors.verticalCenter: parent.verticalCenter
              spacing: Style.space(7)

              DetailRow {
                width: parent.width
                label: "主机"
                value: root.current ? root.current.host : "—"
              }

              DetailRow {
                width: parent.width
                label: "用户"
                value: root.current ? root.current.user : "—"
              }

              DetailRow {
                width: parent.width
                label: "目录"
                value: root.current ? root.current.cwd : "—"
              }

              Column {
                width: parent.width
                spacing: Style.space(3)

                Text {
                  text: "命令"
                  color: root.dim
                  font.family: root.fontFam
                  font.pixelSize: Style.font.caption
                }

                Text {
                  width: parent.width
                  text: root.current ? root.current.command : "—"
                  color: root.fg
                  font.family: "monospace"
                  font.pixelSize: Style.font.body
                  font.bold: true
                  wrapMode: Text.WrapAnywhere
                  textFormat: Text.PlainText
                }
              }

              Row {
                spacing: Style.space(14)

                Text {
                  text: root.current ? ("已等待 " + Math.floor(root.currentAgeSec) + "s") : ""
                  color: root.dim
                  font.family: root.fontFam
                  font.pixelSize: Style.font.caption
                  anchors.verticalCenter: parent.verticalCenter
                }

                Text {
                  text: root.remainSec <= 0 ? "已到 deadline，等待作废…" : ("剩余 " + Math.ceil(root.remainSec) + "s")
                  color: root.remainSec <= 15 ? root.urgentColor : root.dim
                  font.family: root.fontFam
                  font.pixelSize: Style.font.caption
                  font.bold: root.remainSec <= 15
                  anchors.verticalCenter: parent.verticalCenter
                }
              }
            }
          }

          // ---- gate warning ----------------------------------------------------
          Text {
            width: parent.width
            text: "⚠ 只批准与 opencode 闸门1 刚批过、命令对得上的请求"
            color: root.urgentColor
            font.family: root.fontFam
            font.pixelSize: Style.font.caption
            wrapMode: Text.Wrap
          }

          // ---- password + actions ---------------------------------------------
          Row {
            width: parent.width
            spacing: Style.space(10)

            TextField {
              id: pwField
              width: parent.width - denyBtn.width - approveBtn.width - 2 * parent.spacing
              password: true
              foreground: root.fg
              placeholderText: "输入密码，回车 = 批准"
              font.family: root.fontFam
              enabled: root.current !== null && !root.actionBusy
              onAccepted: root.approve()
              Keys.onEscapePressed: root.close()
            }

            Button {
              id: denyBtn
              anchors.verticalCenter: parent.verticalCenter
              text: "拒绝"
              foreground: root.urgentColor
              fontFamily: root.fontFam
              focusable: true
              enabled: root.current !== null && !root.actionBusy
              onClicked: root.deny()
            }

            Button {
              id: approveBtn
              anchors.verticalCenter: parent.verticalCenter
              text: root.actionBusy ? "…" : "批准"
              foreground: root.okColor
              fontFamily: root.fontFam
              focusable: true
              enabled: root.current !== null && !root.actionBusy
              onClicked: root.approve()
            }
          }

          // ---- status line -------------------------------------------------------
          Text {
            visible: root.notice !== ""
            width: parent.width
            text: root.notice
            color: root.okColor
            font.family: root.fontFam
            font.pixelSize: Style.font.caption
            wrapMode: Text.Wrap
          }

          Text {
            visible: root.actionError !== ""
            width: parent.width
            text: "⚠ " + root.actionError
            color: root.urgentColor
            font.family: root.fontFam
            font.pixelSize: Style.font.caption
            wrapMode: Text.Wrap
          }

          // ---- empty / server-down states ------------------------------------
          Text {
            visible: root.current === null && root.stateKnown && root.notice === ""
            width: parent.width
            text: "无待批请求"
            color: root.dim
            font.family: root.fontFam
            font.pixelSize: Style.font.body
          }

          Text {
            visible: !root.demo && !root.stateKnown
            width: parent.width
            text: "sudogate server 未运行（读不到 " + root.statePath + "）。\n批准/拒绝需要 sudogate-server 在运行。"
            color: root.dim
            font.family: root.fontFam
            font.pixelSize: Style.font.caption
            wrapMode: Text.Wrap
          }

          // ---- other pending entries ------------------------------------------
          Column {
            width: parent.width
            visible: root.others.length > 0
            spacing: Style.space(6)

            Text {
              text: "另 " + root.others.length + " 条待批："
              color: root.dim
              font.family: root.fontFam
              font.pixelSize: Style.font.caption
            }

            Repeater {
              model: root.others

              delegate: Rectangle {
                id: otherCard

                required property var modelData

                width: parent.width
                implicitHeight: otherRow.implicitHeight + Style.space(8)
                radius: Style.cornerRadius
                color: Qt.rgba(root.fg.r, root.fg.g, root.fg.b, 0.04)

                Row {
                  id: otherRow
                  x: Style.space(6)
                  width: parent.width - Style.space(12)
                  anchors.verticalCenter: parent.verticalCenter
                  spacing: Style.space(8)

                  Text {
                    id: otherWho
                    text: otherCard.modelData.user + "@" + otherCard.modelData.host
                    color: root.dim
                    font.family: root.fontFam
                    font.pixelSize: Style.font.caption
                    anchors.verticalCenter: parent.verticalCenter
                  }

                  Text {
                    width: parent.width - otherWho.implicitWidth - parent.spacing
                    text: otherCard.modelData.command
                    color: root.fg
                    font.family: "monospace"
                    font.pixelSize: Style.font.caption
                    // ElideLeft: concurrent subagent commands share a long
                    // boring prefix; the distinguishing tail is the part a
                    // human needs to see at a glance.
                    elide: Text.ElideLeft
                    anchors.verticalCenter: parent.verticalCenter
                  }
                }
              }
            }
          }
        }
      }
    }
  }

  component DetailRow: Row {
    property string label: ""
    property string value: "—"

    spacing: Style.space(8)

    Text {
      text: label
      color: root.dim
      font.family: root.fontFam
      font.pixelSize: Style.font.caption
      anchors.verticalCenter: parent.verticalCenter
    }

    Text {
      text: value
      color: root.fg
      font.family: root.fontFam
      font.pixelSize: Style.font.body
      anchors.verticalCenter: parent.verticalCenter
      elide: Text.ElideMiddle
    }
  }
}
