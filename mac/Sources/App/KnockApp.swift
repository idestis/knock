import AppKit
import Sparkle
import SwiftUI

@main
struct KnockApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    private let model = AppModel.shared

    var body: some Scene {
        Window("Knock", id: "main") {
            HomeView()
                .environment(model)
                .background(WindowAccessor())
        }
        .windowStyle(.hiddenTitleBar)
        .windowBackgroundDragBehavior(.enabled)
        .defaultSize(width: 980, height: 660)
        .commands {
            CommandGroup(replacing: .appInfo) {
                Button("About Knock") { AboutPanel.show() }
            }
            CommandGroup(after: .appInfo) {
                Button("Check for Updates…") { Updater.shared.checkForUpdates() }
                    .disabled(!Updater.shared.isEnabled)
            }
            CommandGroup(replacing: .newItem) {
                Button("Knock…") { model.showMainWindow(focusSearch: true) }
                    .keyboardShortcut("k")
            }
            CommandMenu("Door") {
                Toggle("Open to Knocks", isOn: Binding(get: { model.isOpen }, set: { model.setOpen($0) }))
                    .keyboardShortcut("o", modifiers: [.command, .shift])
                Button("Copy Code") { model.copyCode() }
                    .keyboardShortcut("c", modifiers: [.command, .shift])
                Button("New Code") { model.newCode() }
                    .keyboardShortcut("r", modifiers: [.command, .shift])
            }
        }

        MenuBarExtra {
            TrayMenu().environment(model)
        } label: {
            TrayLabel().environment(model)
        }
        .menuBarExtraStyle(.menu)

        Settings {
            SettingsView().environment(model)
        }
    }
}

/// The menu bar item: the K mark as a template image.
struct TrayLabel: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Image(nsImage: Self.image(dimmed: !model.isOpen || !model.connected))
            .accessibilityLabel(model.isOpen ? "Knock, open to knocks" : "Knock, door closed")
            .task { model.openWindow = openWindow }
    }

    static func image(dimmed: Bool) -> NSImage {
        let height: CGFloat = 15
        guard let src = NSImage(named: "TrayMark") else { return NSImage() }
        let size = NSSize(width: (height * src.size.width / src.size.height).rounded(), height: height)
        let img = NSImage(size: size, flipped: false) { rect in
            src.draw(in: rect, from: .zero, operation: .sourceOver, fraction: dimmed ? 0.45 : 1)
            return true
        }
        img.isTemplate = true
        return img
    }
}

/// Native NSMenu (MenuBarExtra .menu style).
struct TrayMenu: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Text(statusLine)
        if let k = model.incoming.first {
            Button("Let \(k.from.firstName) In…") { model.showMainWindow() }
        }
        if let s = model.session {
            Button(s.isHost ? "Show Shared Shell" : "Show \(s.peer.firstName)'s Shell") { model.sessionWindow.show(session: s, model: model) }
            Button(s.isHost ? "End Session" : "Leave Session") { model.endSession(s) }
        }
        Divider()
        Toggle("Open to Knocks", isOn: Binding(get: { model.isOpen }, set: { model.setOpen($0) }))
            .disabled(!model.doorReady)
        Button("Copy Code") { model.copyCode() }
            .keyboardShortcut("c")
            .disabled(model.door == nil)
        Button("New Code") { model.newCode() }
            .disabled(model.door == nil || model.rotatingCode)
        Divider()
        Button("Knock…") { model.showMainWindow(focusSearch: true) }
            .keyboardShortcut("k")
        let online = model.recent.filter { model.presence[$0.doorId] == .online }
        if !online.isEmpty {
            Menu("Knock On") {
                ForEach(online) { r in
                    Button("\(r.ownerName) · \(r.device)") { model.knock(recent: r); model.showMainWindow() }
                }
            }
        }
        Button("Open Knock") { model.showMainWindow() }
            .keyboardShortcut("o")
        Divider()
        SettingsLink { Text("Settings…") }
            .keyboardShortcut(",")
        Button("Check for Updates…") { Updater.shared.checkForUpdates() }
            .disabled(!Updater.shared.isEnabled)
        Button("About Knock") { AboutPanel.show() }
        Divider()
        Button("Quit Knock") { NSApp.terminate(nil) }
            .keyboardShortcut("q")
    }

    private var statusLine: String {
        if let s = model.session { return s.isHost ? "\(s.peer.name) is in your shell" : "In \(s.peer.firstName)'s shell" }
        switch model.connection {
        case .connected: return model.isOpen ? "Open to knocks" : "Door closed"
        case .connecting: return "Connecting…"
        case .offline(let m): return m
        case .idle: return "Not connected"
        }
    }
}

extension AppModel {
    func showMainWindow(focusSearch: Bool = false) {
        NSApp.setActivationPolicy(.regular)
        NSApp.activate()
        if let w = NSApp.windows.first(where: { $0.identifier?.rawValue == "main" && $0.isVisible }) {
            w.makeKeyAndOrderFront(nil)
        } else {
            openWindow?(id: "main")
        }
        if focusSearch {
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) { NotificationCenter.default.post(name: .knockFocusSearch, object: nil) }
        }
    }
}

/// Tags the SwiftUI main window so AppKit code can find it, keeps the Dock
/// icon in step with visible windows, and seats the traffic lights inside the
/// sidebar card (mock 01).
struct WindowAccessor: NSViewRepresentable {
    func makeNSView(context: Context) -> NSView {
        let v = NSView()
        DispatchQueue.main.async {
            guard let w = v.window else { return }
            w.identifier = NSUserInterfaceItemIdentifier("main")
            TrafficLights.attach(to: w)
            WindowPolicy.update()
        }
        return v
    }

    func updateNSView(_ nsView: NSView, context: Context) {}
}

@MainActor
enum TrafficLights {
    /// Centre of the buttons from the window's top-left, in points.
    static let inset = CGPoint(x: 34, y: 34)
    private static var observed = Set<ObjectIdentifier>()

    static func attach(to w: NSWindow) {
        place(w)
        guard observed.insert(ObjectIdentifier(w)).inserted else { return }
        for name in [NSWindow.didResizeNotification, NSWindow.didEndLiveResizeNotification, NSWindow.didBecomeKeyNotification,
                     NSWindow.didResignKeyNotification, NSWindow.didExitFullScreenNotification] {
            NotificationCenter.default.addObserver(forName: name, object: w, queue: .main) { [weak w] _ in
                MainActor.assumeIsolated { if let w { place(w) } }
            }
        }
    }

    static func place(_ w: NSWindow) {
        guard !w.styleMask.contains(.fullScreen),
              let close = w.standardWindowButton(.closeButton),
              let container = close.superview?.superview else { return }
        let height = inset.y * 2
        // Grow the titlebar container so the buttons aren't clipped, but only over the sidebar.
        container.frame = NSRect(x: 0, y: w.frame.height - height, width: 320, height: height)
        let buttons: [NSWindow.ButtonType] = [.closeButton, .miniaturizeButton, .zoomButton]
        for (i, type) in buttons.enumerated() {
            guard let b = w.standardWindowButton(type) else { continue }
            let size = b.frame.size
            b.setFrameOrigin(NSPoint(x: inset.x - size.width / 2 + CGFloat(i) * 20,
                                     y: height - inset.y - size.height / 2))
        }
    }
}

/// Knock lives in the menu bar; it shows in the Dock only while a window is open.
enum WindowPolicy {
    @MainActor static func update() {
        let visible = NSApp.windows.contains { w in
            w.isVisible && (w.identifier?.rawValue == "main" || w.identifier?.rawValue == "session" || w.identifier?.rawValue.contains("Settings") == true || w.title.contains("Settings"))
        }
        let wanted: NSApplication.ActivationPolicy = visible ? .regular : .accessory
        if NSApp.activationPolicy() != wanted { NSApp.setActivationPolicy(wanted) }
    }
}

enum AboutPanel {
    static let repo = URL(string: "https://github.com/idestis/knock")!

    @MainActor static func show() {
        let center = NSMutableParagraphStyle()
        center.alignment = .center
        center.lineSpacing = 2
        let body: [NSAttributedString.Key: Any] = [.font: NSFont.systemFont(ofSize: 11), .foregroundColor: NSColor.secondaryLabelColor,
                                                    .paragraphStyle: center]
        let credits = NSMutableAttributedString(string: "Share one shell with a helper.\nNothing happens without you.\n\n", attributes: body)

        // GitHub mark, tinted like the text, linking to the repo.
        if let mark = NSImage(named: "GitHubMark") {
            let size = NSSize(width: 15, height: 15)
            let tinted = NSImage(size: size, flipped: false) { rect in
                mark.draw(in: rect)
                NSColor.secondaryLabelColor.set()
                rect.fill(using: .sourceAtop)
                return true
            }
            let attachment = NSTextAttachment()
            attachment.image = tinted
            attachment.bounds = NSRect(x: 0, y: -3, width: size.width, height: size.height)
            let icon = NSMutableAttributedString(attachment: attachment)
            icon.addAttributes([.link: repo, .paragraphStyle: center, .toolTip: "github.com/idestis/knock"],
                               range: NSRange(location: 0, length: icon.length))
            credits.append(icon)
        } else {
            credits.append(NSAttributedString(string: "GitHub", attributes: body.merging([.link: repo]) { $1 }))
        }
        credits.append(NSAttributedString(string: "   🇺🇦", attributes: body.merging([.font: NSFont.systemFont(ofSize: 14)]) { $1 }))

        NSApp.activate()
        NSApp.orderFrontStandardAboutPanel(options: [.credits: credits])
    }
}

/// Sparkle, fed from GitHub Releases (releases/latest/download/appcast.xml).
@MainActor
final class Updater {
    static let shared = Updater()
    private let controller: SPUStandardUpdaterController?

    var isEnabled: Bool { controller != nil }

    private init() {
        let key = Bundle.main.object(forInfoDictionaryKey: "SUPublicEDKey") as? String ?? ""
        controller = key.isEmpty ? nil : SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: nil, userDriverDelegate: nil)
    }

    func checkForUpdates() {
        NSApp.activate()
        controller?.checkForUpdates(nil)
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        MainActor.assumeIsolated {
            _ = Updater.shared
            AppModel.shared.start()
            #if DEBUG
            DebugSnapshot.startIfRequested()
            #endif
            for name in [NSWindow.didBecomeKeyNotification, NSWindow.willCloseNotification, NSWindow.didMiniaturizeNotification] {
                NotificationCenter.default.addObserver(forName: name, object: nil, queue: .main) { _ in
                    DispatchQueue.main.async { MainActor.assumeIsolated { WindowPolicy.update() } }
                }
            }
        }
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        MainActor.assumeIsolated { AppModel.shared.showMainWindow() }
        return true
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        MainActor.assumeIsolated {
            let model = AppModel.shared
            if let s = model.session {
                let alert = NSAlert()
                alert.messageText = "Quit Knock?"
                alert.informativeText = s.isHost ? "\(s.peer.name) is in your shell. Quitting ends the session and closes your door."
                                                 : "You're in \(s.peer.firstName)'s shell. Quitting ends the session."
                alert.addButton(withTitle: "Quit")
                alert.addButton(withTitle: "Cancel")
                if alert.runModal() != .alertFirstButtonReturn { return .terminateCancel }
            }
            model.shutdown()
            return .terminateNow
        }
    }
}
