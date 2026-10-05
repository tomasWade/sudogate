pragma ComponentBehavior: Bound
import QtQuick
import qs.Ui
import qs.Commons

// Bar entry point: the pending-review badge. By default the badge always
// occupies its bar slot (dimmed key + 0 when idle) so the plugin presence —
// and with it the expectation of a working server — stays visible;
// hideWhenEmpty: true restores the silent-bar "no requests" state. All state
// and process plumbing live in Panel.qml, loaded once here; the badge just
// mirrors its count.
BarWidget {
  id: root
  moduleName: "tomaswade.sudogate"

  function injectPanel() {
    var target = panelLoader.item
    if (!target) return
    if ("bar" in target) target.bar = root.bar
    if ("settings" in target) target.settings = root.settings
    if ("anchorItem" in target) target.anchorItem = badgeHost
    if ("hostWidget" in target) target.hostWidget = root
  }

  function togglePanel() {
    if (panelLoader.item && panelLoader.item.toggle) panelLoader.item.toggle()
  }

  // Shape contract for shell.summon/hide/toggle routing (Bar.findPanelWidget
  // requires open/close/opened on the bar-widget root).
  readonly property bool opened: panelLoader.item ? panelLoader.item.opened === true : false

  function open() {
    if (panelLoader.item && panelLoader.item.open) panelLoader.item.open()
  }

  function close() {
    if (panelLoader.item && panelLoader.item.close) panelLoader.item.close()
  }

  // Forwarded so this widget can stand in for the panel as the bar's popout
  // identity (Bar.requestPopout prefers closeForPopoutSwitch over close).
  readonly property bool popoutSwitchClosing: panelLoader.item ? panelLoader.item.popoutSwitchClosing === true : false

  function closeForPopoutSwitch() {
    if (panelLoader.item) panelLoader.item.closeForPopoutSwitch()
  }

  // How wide the bar's open-panel underline should be.
  readonly property real openPanelIndicatorWidth: badgeRow.implicitWidth

  readonly property int pendingCount: panelLoader.item ? panelLoader.item.count : 0

  onBarChanged: {
    injectPanel()
    syncClickRegistration()
  }

  onSettingsChanged: injectPanel()

  Loader {
    id: panelLoader
    active: true
    source: Qt.resolvedUrl("Panel.qml")
    visible: false
    onLoaded: {
      root.injectPanel()
      Qt.callLater(root.injectPanel)
    }
  }

  // Click-target registration, mirroring WidgetButton: while this widget's
  // panel is open, its dismissal overlay forwards clicks that land on the
  // bar strip back to registered targets via triggerPress.
  property var registeredBar: null

  function syncClickRegistration() {
    if (registeredBar && registeredBar.unregisterClickTarget) registeredBar.unregisterClickTarget(root)
    registeredBar = root.bar
    if (registeredBar && registeredBar.registerClickTarget) registeredBar.registerClickTarget(root)
  }

  Component.onCompleted: syncClickRegistration()

  Component.onDestruction: if (registeredBar && registeredBar.unregisterClickTarget) registeredBar.unregisterClickTarget(root)

  function triggerPress(button) {
    if (root.bar) root.bar.hideTooltip(root)
    if (button !== Qt.RightButton) root.togglePanel()
  }

  // Default: the badge always occupies its bar slot (dimmed key + 0 when
  // idle) so the plugin presence — and with it the expectation of a working
  // server — stays visible. hideWhenEmpty: true collapses it whenever the
  // queue is empty.
  readonly property bool hideWhenEmpty: setting("hideWhenEmpty", false) === true

  visible: !hideWhenEmpty || pendingCount > 0
  implicitWidth: badgeRow.implicitWidth + Style.space(12)
  implicitHeight: barSize

  readonly property color badgeColor: pendingCount > 0
    ? (root.bar ? root.bar.urgent : Color.urgent)
    : (root.bar ? root.bar.barForeground : Color.foreground)
  readonly property real badgeOpacity: pendingCount > 0 ? 1 : 0.45

  Item {
    id: badgeHost
    anchors.centerIn: parent
    width: badgeRow.implicitWidth
    height: badgeRow.implicitHeight
    // Idle/urgent dimming animates on the host so the glyph and count fade
    // together; the Texts below stay at full opacity.
    opacity: root.badgeOpacity

    Row {
      id: badgeRow
      anchors.centerIn: parent
      spacing: Style.space(5)

      // Nerd Font key glyph (Font Awesome range, same source as the built-in
      // bar widgets' icons — NF is an omarchy hard dependency, so the glyph
      // renders everywhere this plugin can run). Full name lives in the
      // tooltip.
      Text {
        text: "\uf084"
        color: root.badgeColor
        font.family: root.bar ? root.bar.fontFamily : Style.font.family
        font.pixelSize: Style.font.icon
        anchors.verticalCenter: parent.verticalCenter
      }

      Text {
        text: String(root.pendingCount)
        color: root.badgeColor
        font.family: root.bar ? root.bar.fontFamily : Style.font.family
        font.pixelSize: Style.font.body
        font.bold: true
        anchors.verticalCenter: parent.verticalCenter
      }
    }

    Behavior on opacity {
      NumberAnimation {
        duration: 140
      }
    }
  }

  MouseArea {
    anchors.fill: parent
    acceptedButtons: Qt.LeftButton | Qt.RightButton | Qt.MiddleButton
    hoverEnabled: true
    cursorShape: Qt.PointingHandCursor

    onEntered: {
      if (root.bar && panelLoader.item) root.bar.showTooltip(root, panelLoader.item.barTooltip)
    }

    onExited: {
      if (root.bar) root.bar.hideTooltip(root)
    }

    onClicked: function(mouse) { root.triggerPress(mouse.button) }
  }
}
