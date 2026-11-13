import AppKit
import Darwin
import SwiftTerm
import SwiftUI

enum TerminalStyle {
    static let background = NSColor(srgbRed: 0.09, green: 0.10, blue: 0.12, alpha: 1)
    static let foreground = NSColor(srgbRed: 0.90, green: 0.91, blue: 0.93, alpha: 1)
    static let caret = NSColor(srgbRed: 0.53, green: 0.67, blue: 1.0, alpha: 1)

    static func apply(_ v: TerminalView) {
        v.font = NSFont.monospacedSystemFont(ofSize: 13, weight: .regular)
        v.nativeBackgroundColor = background
        v.nativeForegroundColor = foreground
        v.caretColor = caret
        v.optionAsMetaKey = true
    }
}

/// Host side: runs the login shell in a PTY, renders it locally, mirrors
/// output to the relay and writes guest input into the PTY so it echoes
/// exactly like local typing.
@MainActor
final class HostTerminal: NSObject {
    let view: TerminalView
    private var process: LocalProcess!
    private let toRelay: (Data) -> Void
    private let onResize: (Int, Int) -> Void
    private let onExit: () -> Void
    var recorder: Recorder?
    private(set) var cols = 80, rows = 24

    init(toRelay: @escaping (Data) -> Void, onResize: @escaping (Int, Int) -> Void, onExit: @escaping () -> Void) {
        self.toRelay = toRelay
        self.onResize = onResize
        self.onExit = onExit
        view = TerminalView(frame: NSRect(x: 0, y: 0, width: 800, height: 500))
        super.init()
        TerminalStyle.apply(view)
        view.terminalDelegate = self
        process = LocalProcess(delegate: self, dispatchQueue: .main)
    }

    func start(shellCommand: String?) {
        let shell = shellCommand?.isEmpty == false ? shellCommand! : Self.loginShell()
        var env = Terminal.getEnvironmentVariables(termName: "xterm-256color")
        env.append("KNOCK_SESSION=1")
        env.append("LANG=\(ProcessInfo.processInfo.environment["LANG"] ?? "en_US.UTF-8")")
        let name = "-" + (shell as NSString).lastPathComponent // login shell
        process.startProcess(executable: shell, args: [], environment: env, execName: name,
                             currentDirectory: FileManager.default.homeDirectoryForCurrentUser.path)
    }

    static func loginShell() -> String {
        if let pw = getpwuid(getuid()), let sh = pw.pointee.pw_shell { return String(cString: sh) }
        return "/bin/zsh"
    }

    /// Guest keystrokes from the relay.
    func guestInput(_ d: Data) {
        process.send(data: ArraySlice(d))
        recorder?.input(d, source: "guest")
    }

    func terminate() { process.terminate() }
}

extension HostTerminal: @preconcurrency LocalProcessDelegate {
    func processTerminated(_ source: LocalProcess, exitCode: Int32?) { onExit() }

    func dataReceived(slice: ArraySlice<UInt8>) {
        view.feed(byteArray: slice)
        toRelay(Data(slice))
        recorder?.output(slice)
    }

    func getWindowSize() -> winsize {
        winsize(ws_row: UInt16(rows), ws_col: UInt16(cols), ws_xpixel: 0, ws_ypixel: 0)
    }
}

extension HostTerminal: TerminalViewDelegate {
    func sizeChanged(source: TerminalView, newCols: Int, newRows: Int) {
        guard newCols > 0, newRows > 0, newCols != cols || newRows != rows else { return }
        cols = newCols
        rows = newRows
        var size = getWindowSize()
        process.updateWindowSize(&size)
        recorder?.resize(cols: cols, rows: rows)
        onResize(cols, rows)
    }

    func send(source: TerminalView, data: ArraySlice<UInt8>) {
        process.send(data: data)
    }

    func setTerminalTitle(source: TerminalView, title: String) {}
    func hostCurrentDirectoryUpdate(source: TerminalView, directory: String?) {}
    func scrolled(source: TerminalView, position: Double) {}
    func rangeChanged(source: TerminalView, startY: Int, endY: Int) {}
    func requestOpenLink(source: TerminalView, link: String, params: [String: String]) {
        if let url = URL(string: link), ["http", "https"].contains(url.scheme) { NSWorkspace.shared.open(url) }
    }
    func bell(source: TerminalView) { NSSound.beep() }
    func clipboardCopy(source: TerminalView, content: Data) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(String(decoding: content, as: UTF8.self), forType: .string)
    }
    func iTermContent(source: TerminalView, content: ArraySlice<UInt8>) {}
}

/// Guest side: renders the host's PTY at the host's size (letterboxed) and
/// sends keystrokes to the relay.
@MainActor
final class GuestTerminal: NSObject {
    let view: TerminalView
    let container: LetterboxView
    private let toRelay: (Data) -> Void
    var inputAllowed = true
    var recorder: Recorder?
    var onBlockedInput: () -> Void = {}

    init(toRelay: @escaping (Data) -> Void) {
        self.toRelay = toRelay
        view = TerminalView(frame: NSRect(x: 0, y: 0, width: 800, height: 500))
        container = LetterboxView()
        super.init()
        TerminalStyle.apply(view)
        view.terminalDelegate = self
        container.terminal = view
    }

    func output(_ d: Data) {
        view.feed(byteArray: ArraySlice(d))
        recorder?.output(d)
    }

    func resize(cols: Int, rows: Int) {
        view.resize(cols: cols, rows: rows)
        container.fit()
        recorder?.resize(cols: cols, rows: rows)
    }
}

extension GuestTerminal: TerminalViewDelegate {
    func sizeChanged(source: TerminalView, newCols: Int, newRows: Int) {}
    func send(source: TerminalView, data: ArraySlice<UInt8>) {
        guard inputAllowed else { onBlockedInput(); return }
        toRelay(Data(data))
    }
    func setTerminalTitle(source: TerminalView, title: String) {}
    func hostCurrentDirectoryUpdate(source: TerminalView, directory: String?) {}
    func scrolled(source: TerminalView, position: Double) {}
    func rangeChanged(source: TerminalView, startY: Int, endY: Int) {}
    func requestOpenLink(source: TerminalView, link: String, params: [String: String]) {}
    func bell(source: TerminalView) {}
    func clipboardCopy(source: TerminalView, content: Data) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(String(decoding: content, as: UTF8.self), forType: .string)
    }
    func iTermContent(source: TerminalView, content: ArraySlice<UInt8>) {}
}

/// Holds the guest terminal at exactly the host's grid size, centred.
final class LetterboxView: NSView {
    var terminal: TerminalView? {
        didSet {
            oldValue?.removeFromSuperview()
            if let terminal { addSubview(terminal); fit() }
        }
    }

    override var isFlipped: Bool { true }

    override func layout() {
        super.layout()
        fit()
    }

    func fit() {
        guard let t = terminal else { return }
        // Exactly the host's grid, centred; clipped if the window is smaller.
        let want = t.getOptimalFrameSize().size
        let frame = NSRect(x: max(0, (bounds.width - want.width) / 2), y: max(0, (bounds.height - want.height) / 2),
                           width: want.width, height: want.height).integral
        if t.frame != frame { t.frame = frame }
    }

    var isClipped: Bool {
        guard let t = terminal else { return false }
        let want = t.getOptimalFrameSize().size
        return want.width > bounds.width + 1 || want.height > bounds.height + 1
    }
}

/// SwiftUI wrapper that hosts an existing AppKit view.
struct HostedView: NSViewRepresentable {
    let view: NSView
    var focus = true

    func makeNSView(context: Context) -> NSView {
        let box = NSView()
        view.translatesAutoresizingMaskIntoConstraints = false
        box.addSubview(view)
        NSLayoutConstraint.activate([
            view.leadingAnchor.constraint(equalTo: box.leadingAnchor), view.trailingAnchor.constraint(equalTo: box.trailingAnchor),
            view.topAnchor.constraint(equalTo: box.topAnchor), view.bottomAnchor.constraint(equalTo: box.bottomAnchor),
        ])
        if focus {
            DispatchQueue.main.async { box.window?.makeFirstResponder((view as? LetterboxView)?.terminal ?? view) }
        }
        return box
    }

    func updateNSView(_ nsView: NSView, context: Context) {}
}
