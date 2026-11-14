import AppKit
import SwiftUI

/// The shared-shell window. AppKit-managed so it opens and closes
/// deterministically with the session, whatever else is on screen.
@MainActor
final class SessionWindowController: NSObject, NSWindowDelegate {
    private var window: NSWindow?
    private weak var model: AppModel?
    private var session: ActiveSession?

    func show(session: ActiveSession, model: AppModel) {
        self.model = model
        self.session = session
        let w = window ?? makeWindow()
        w.title = session.isHost ? "\(session.peer.name) is in your shell" : "\(session.peer.name)'s shell"
        w.contentView = NSHostingView(rootView: SessionView(session: session).environment(model))
        window = w
        NSApp.setActivationPolicy(.regular)
        NSApp.activate()
        w.makeKeyAndOrderFront(nil)
        if let t = session.host?.view ?? session.guest?.view { w.makeFirstResponder(t) }
    }

    func close(reason: String) {
        session = nil
        guard let w = window else { return }
        window = nil
        w.delegate = nil
        w.close()
        WindowPolicy.update()
        let alert = NSAlert()
        alert.messageText = "Session ended"
        alert.informativeText = reason
        alert.alertStyle = .informational
        alert.addButton(withTitle: "OK")
        NSApp.activate()
        alert.runModal()
    }

    private func makeWindow() -> NSWindow {
        let w = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 960, height: 600),
                         styleMask: [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView],
                         backing: .buffered, defer: false)
        w.identifier = NSUserInterfaceItemIdentifier("session")
        w.titlebarAppearsTransparent = true
        w.titleVisibility = .hidden
        w.appearance = NSAppearance(named: .darkAqua)
        w.backgroundColor = TerminalStyle.background
        w.minSize = NSSize(width: 520, height: 320)
        w.isReleasedWhenClosed = false
        w.center()
        w.setFrameAutosaveName("KnockSession")
        w.delegate = self
        return w
    }

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        guard let session, let model else { return true }
        let alert = NSAlert()
        alert.messageText = "End the session?"
        alert.informativeText = session.isHost ? "\(session.peer.name) will be disconnected from your shell." : "You'll leave \(session.peer.name)'s shell."
        alert.addButton(withTitle: "End Session")
        alert.addButton(withTitle: "Cancel")
        alert.buttons[0].hasDestructiveAction = true
        guard alert.runModal() == .alertFirstButtonReturn else { return false }
        model.endSession(session)
        return false // close() is called by the model
    }
}

/// Mock 04: dark terminal, who is in it, a timer, End session, typing indicator, keystroke ribbon.
struct SessionView: View {
    @Bindable var session: ActiveSession
    @Environment(AppModel.self) private var model

    var body: some View {
        ZStack(alignment: .top) {
            Color(nsColor: TerminalStyle.background).ignoresSafeArea()
            LinearGradient(colors: [Color(red: 0.05, green: 0.13, blue: 0.36).opacity(0.9), .clear], startPoint: .topLeading, endPoint: .center)
                .ignoresSafeArea()
                .allowsHitTesting(false)

            VStack(spacing: 0) {
                header
                terminal
                    .padding(.horizontal, 14)
                    .padding(.top, 6)
                footer
            }
        }
        .preferredColorScheme(.dark)
    }

    private var header: some View {
        HStack(spacing: 10) {
            Spacer().frame(width: 64) // traffic lights
            GlassEffectContainer {
                HStack(spacing: 10) {
                    Avatar(identity: session.peer, size: 26, filled: true)
                    Text(session.isHost ? "\(session.peer.name) is in your shell" : "You're in \(session.peer.firstName)'s shell")
                        .font(.callout.weight(.semibold))
                    if !session.peer.verified {
                        Text("not verified").font(.caption).foregroundStyle(.orange)
                    }
                    Divider().frame(height: 14)
                    TimelineView(.periodic(from: session.started, by: 1)) { ctx in
                        Text(Self.elapsed(ctx.date.timeIntervalSince(session.started)))
                            .font(.callout.monospacedDigit())
                            .foregroundStyle(.secondary)
                    }
                    if session.recorder != nil {
                        Image(systemName: "record.circle").foregroundStyle(.red).help("This session is being recorded")
                    }
                }
                .padding(.leading, 4)
                .padding(.trailing, 14)
                .padding(.vertical, 4)
                .glassEffect(.regular, in: .capsule)
            }
            Spacer()
            if session.isHost {
                Toggle(isOn: Binding(get: { !session.guestInput }, set: { model.setGuestInput(!$0, session: session) })) {
                    Label("Hands off", systemImage: session.guestInput ? "hand.raised" : "hand.raised.fill")
                }
                .toggleStyle(.button)
                .buttonStyle(.glass)
                .help("Make \(session.peer.firstName) view-only. Use it before you type a password.")
            }
            Button(role: .destructive) { model.endSession(session) } label: {
                Text(session.isHost ? "End session" : "Leave").fontWeight(.semibold)
            }
            .buttonStyle(.glassProminent)
            .tint(.red)
        }
        .padding(.horizontal, 12)
        .frame(height: 52)
    }

    @ViewBuilder private var terminal: some View {
        if let host = session.host {
            HostedView(view: host.view)
        } else if let guest = session.guest {
            HostedView(view: guest.container)
        }
    }

    private var footer: some View {
        HStack(spacing: 12) {
            TimelineView(.periodic(from: .now, by: 0.5)) { ctx in
                let typing = session.lastPeerKeyAt.map { ctx.date.timeIntervalSince($0) < 1.5 } ?? false
                HStack(spacing: 8) {
                    Circle().fill(typing ? Color.accentColor : Color.secondary.opacity(0.5)).frame(width: 8, height: 8)
                    Text(statusText(typing: typing)).font(.callout.weight(.medium))
                }
                .padding(.horizontal, 12)
                .padding(.vertical, 6)
                .glassEffect(.regular, in: .capsule)
            }
            if session.isHost && !session.ribbon.isEmpty {
                ScrollViewReader { proxy in
                    ScrollView(.horizontal, showsIndicators: false) {
                        HStack(spacing: 4) {
                            ForEach(Array(session.ribbon.enumerated()), id: \.offset) { i, key in
                                Text(key.allSatisfy { $0 == " " } ? "␣" : key)
                                    .font(.caption.monospaced())
                                    .padding(.horizontal, 6)
                                    .padding(.vertical, 2)
                                    .background(.white.opacity(0.08), in: RoundedRectangle(cornerRadius: 5))
                                    .id(i)
                            }
                        }
                    }
                    .onChange(of: session.ribbon.count) { proxy.scrollTo(session.ribbon.count - 1, anchor: .trailing) }
                }
                .frame(maxWidth: 360)
                .help("Keys \(session.peer.firstName) pressed, including ones the shell doesn't show")
            }
            Spacer()
            Text(session.notice ?? (session.isHost ? "You can type too. End the session or quit Knock to close the door."
                                                  : "Everything you type is visible to \(session.peer.firstName)."))
                .font(.callout)
                .foregroundStyle(.secondary)
                .lineLimit(1)
        }
        .padding(.horizontal, 14)
        .frame(height: 50)
    }

    private func statusText(typing: Bool) -> String {
        if session.isHost {
            if !session.guestInput { return "\(session.peer.firstName) is watching" }
            return typing ? "\(session.peer.firstName) is typing" : "\(session.peer.firstName) is connected"
        }
        return session.guestInput ? "Connected" : "View only"
    }

    static func elapsed(_ t: TimeInterval) -> String {
        let s = Int(t)
        return s >= 3600 ? String(format: "%d:%02d:%02d", s / 3600, s / 60 % 60, s % 60) : String(format: "%02d:%02d", s / 60, s % 60)
    }
}
