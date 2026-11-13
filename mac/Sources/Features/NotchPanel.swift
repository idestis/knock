import AppKit
import SwiftUI

/// A knock drops out of the notch (or shows as a glass capsule at the top
/// centre when there is no notch). Non-activating, on all Spaces and over
/// full-screen apps. No default button: Return never lets anyone in.
@MainActor
final class NotchPanelController {
    private var panel: KnockPanel?

    func show(knock: KnockIncoming, model: AppModel) {
        dismiss()
        guard let screen = NSScreen.screens.first(where: { $0.safeAreaInsets.top > 0 }) ?? NSScreen.main else { return }
        let hasNotch = screen.safeAreaInsets.top > 0
        let notchWidth: CGFloat = {
            guard hasNotch, let l = screen.auxiliaryTopLeftArea, let r = screen.auxiliaryTopRightArea else { return 200 }
            return max(160, r.minX - l.maxX)
        }()
        let size = NSSize(width: 460, height: hasNotch ? 168 : 150)
        let top = hasNotch ? screen.frame.maxY : screen.visibleFrame.maxY - 8
        let frame = NSRect(x: screen.frame.midX - size.width / 2, y: top - size.height, width: size.width, height: size.height)

        let p = KnockPanel(contentRect: frame, styleMask: [.borderless, .nonactivatingPanel], backing: .buffered, defer: false)
        p.level = .statusBar + 1
        p.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .stationary, .ignoresCycle]
        p.isOpaque = false
        p.backgroundColor = .clear
        p.hasShadow = false
        p.isMovable = false
        p.hidesOnDeactivate = false
        p.onEscape = { [weak model] in model?.answer(knock, allow: false) }
        let view = KnockPromptPanelView(knock: knock, notch: hasNotch, notchWidth: notchWidth, notchHeight: screen.safeAreaInsets.top,
                                        onAllow: { [weak model] in model?.answer(knock, allow: true) },
                                        onDeny: { [weak model] in model?.answer(knock, allow: false) })
        p.contentView = NSHostingView(rootView: view.frame(width: size.width, height: size.height, alignment: .top))
        p.orderFrontRegardless()
        p.makeKey()
        panel = p
    }

    func dismiss() {
        panel?.orderOut(nil)
        panel = nil
    }
}

final class KnockPanel: NSPanel {
    var onEscape: () -> Void = {}
    override var canBecomeKey: Bool { true }
    override func cancelOperation(_ sender: Any?) { onEscape() }
}

struct KnockPromptPanelView: View {
    let knock: KnockIncoming
    let notch: Bool
    let notchWidth: CGFloat
    let notchHeight: CGFloat
    let onAllow: () -> Void
    let onDeny: () -> Void
    @State private var open = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        let content = KnockPromptContent(knock: knock, dark: notch, onAllow: onAllow, onDeny: onDeny)
            .padding(.horizontal, 22)
            .padding(.top, notch ? notchHeight + 12 : 18)
            .padding(.bottom, 18)
            .opacity(open ? 1 : 0)

        Group {
            if notch {
                content
                    .frame(width: open ? 460 : notchWidth, height: open ? nil : notchHeight, alignment: .top)
                    .background(.black, in: UnevenRoundedRectangle(bottomLeadingRadius: open ? 32 : 10, bottomTrailingRadius: open ? 32 : 10))
                    .clipShape(UnevenRoundedRectangle(bottomLeadingRadius: open ? 32 : 10, bottomTrailingRadius: open ? 32 : 10))
                    .environment(\.colorScheme, .dark)
            } else {
                content
                    .frame(width: 440)
                    .glassEffect(.regular, in: .rect(cornerRadius: 28))
                    .scaleEffect(open ? 1 : 0.9, anchor: .top)
            }
        }
        .frame(maxWidth: .infinity, alignment: .top)
        .onAppear {
            withAnimation(reduceMotion ? .easeOut(duration: 0.15) : .spring(response: 0.42, dampingFraction: 0.78)) { open = true }
        }
    }
}

/// Shared by the notch panel and the in-window alert.
struct KnockPromptContent: View {
    let knock: KnockIncoming
    var dark = false
    let onAllow: () -> Void
    let onDeny: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack(spacing: 14) {
                Avatar(identity: knock.from, size: 46, filled: true)
                VStack(alignment: .leading, spacing: 3) {
                    Text("Knock, knock.").font(.headline)
                    Text("\(knock.from.name) wants to come in from \(knock.device).")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                        .lineLimit(2)
                    if !knock.from.verified {
                        Label("Not verified. Anyone can type any name.", systemImage: "exclamationmark.triangle.fill")
                            .font(.caption)
                            .foregroundStyle(.orange)
                    } else if let email = knock.from.email {
                        Label(email, systemImage: knock.from.provider == "github" ? "checkmark.seal" : "checkmark.seal.fill")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                Spacer(minLength: 0)
            }
            HStack(spacing: 12) {
                Button(action: onDeny) {
                    Text("Not now").frame(maxWidth: .infinity)
                }
                .buttonStyle(.glass)
                .controlSize(.large)
                Button(action: onAllow) {
                    Text("Let \(knock.from.firstName) in").frame(maxWidth: .infinity)
                }
                .buttonStyle(.glassProminent)
                .controlSize(.large)
                // Deliberately no .keyboardShortcut(.defaultAction): Return must not let anyone in.
            }
        }
    }
}

struct Avatar: View {
    let identity: Identity?
    var initials: String? = nil
    var size: CGFloat = 36
    var filled = false
    var neutral = false

    var body: some View {
        let text = initials ?? identity?.initials ?? "?"
        Text(text)
            .font(.system(size: size * 0.36, weight: .semibold, design: .rounded))
            .foregroundStyle(filled ? Color.white : neutral ? Color.primary.opacity(0.75) : Color.accentColor)
            .frame(width: size, height: size)
            .background(Circle().fill(filled ? AnyShapeStyle(Color.accentColor.gradient)
                                             : neutral ? AnyShapeStyle(Color.secondary.opacity(0.14)) : AnyShapeStyle(Color.accentColor.opacity(0.12))))
            .accessibilityHidden(true)
    }
}
