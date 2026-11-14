import SwiftUI
import UserNotifications

/// First-run wizard: welcome, your name, optional sign-in, notifications, Full Disk Access, your code.
struct OnboardingView: View {
    @Environment(AppModel.self) private var model
    @State private var step = 0
    @State private var name = ""
    @State private var relay = ""
    @State private var showRelay = false
    @State private var notifications: UNAuthorizationStatus = .notDetermined
    @State private var openNow = true
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private let steps = 6

    init(step: Int = 0) { _step = State(initialValue: step) }

    var body: some View {
        VStack(spacing: 0) {
            Spacer().frame(height: 52)
            HStack(spacing: 6) {
                ForEach(0..<steps, id: \.self) { i in
                    Capsule()
                        .fill(i <= step ? Color.accentColor : Color.secondary.opacity(0.25))
                        .frame(width: i == step ? 22 : 8, height: 8)
                }
            }
            .animation(reduceMotion ? nil : .snappy, value: step)

            Spacer(minLength: 20)
            Group {
                switch step {
                case 0: welcome
                case 1: about
                case 2: signIn
                case 3: notify
                case 4: diskAccess
                default: ready
                }
            }
            .frame(maxWidth: 520)
            .padding(36)
            .glassEffect(.regular.tint(Color.white.opacity(0.35)), in: .rect(cornerRadius: 30))
            .transition(reduceMotion ? .opacity : .asymmetric(insertion: .move(edge: .trailing).combined(with: .opacity),
                                                         removal: .move(edge: .leading).combined(with: .opacity)))
            .id(step)
            Spacer(minLength: 20)
        }
        .padding(.horizontal, 40)
        .padding(.bottom, 40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .onAppear {
            name = model.settings.displayName
            relay = model.settings.relayURLString
            refreshNotifications()
        }
        .onChange(of: model.signedIn) { if model.signedIn && step == 2 { next() } }
    }

    // MARK: Steps

    private var welcome: some View {
        VStack(spacing: 18) {
            KnockLogo(size: 84)
            Text("Knock, knock.").font(.system(size: 34, weight: .bold))
            Text("Share one shell on your Mac with the person helping you. They knock, you let them in, and you both type in the same terminal.")
                .font(.title3)
                .multilineTextAlignment(.center)
                .foregroundStyle(.secondary)
            VStack(alignment: .leading, spacing: 12) {
                Rule(icon: "hand.raised.fill", text: "Nothing happens until you click Let in. Every time.")
                Rule(icon: "eye.fill", text: "You see every key they type, and you can end it at any moment.")
                Rule(icon: "power", text: "Quit Knock and nobody can reach you.")
                Rule(icon: "arrow.trianglehead.2.clockwise", text: "A new code cuts everyone off straight away.")
            }
            .padding(.top, 8)
            primary("Get started")
        }
    }

    private var about: some View {
        VStack(alignment: .leading, spacing: 16) {
            Title("What should people see?", "Your name shows on every knock you make. Sign in next to make it verified.")
            VStack(alignment: .leading, spacing: 6) {
                TextField("Your name", text: $name)
                    .textFieldStyle(.roundedBorder)
                    .controlSize(.extraLarge)
                Label("This Mac: \(AppInfo.deviceName)", systemImage: "laptopcomputer")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(.leading, 4)
            }
            Button {
                withAnimation(.snappy) { showRelay.toggle() }
            } label: {
                HStack(spacing: 8) {
                    Image(systemName: "chevron.right")
                        .font(.caption.weight(.semibold))
                        .rotationEffect(.degrees(showRelay ? 90 : 0))
                        .foregroundStyle(.secondary)
                    Text("Relay")
                    Spacer()
                    Text(relayHost).foregroundStyle(.secondary).lineLimit(1)
                }
                .padding(.vertical, 6)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            if showRelay {
                VStack(alignment: .leading, spacing: 6) {
                    TextField("https://knock.example.ts.net", text: $relay)
                        .textFieldStyle(.roundedBorder)
                        .disabled(model.settings.isManaged("relay.url"))
                    Text("The knockd server that passes knocks along. Your team gives you this; leave it as is if you're not sure.")
                        .font(.caption).foregroundStyle(.secondary)
                }
                .transition(.opacity.combined(with: .move(edge: .top)))
            }
            HStack {
                back
                Spacer()
                primary("Continue") {
                    model.settings.displayName = name.trimmingCharacters(in: .whitespaces).isEmpty ? NSFullUserName() : name
                    if relay != model.settings.relayURLString {
                        model.settings.relayURLString = relay
                        model.relay.stop()
                        model.connectRelay()
                    }
                    next()
                }
            }
        }
    }

    private var signIn: some View {
        VStack(alignment: .leading, spacing: 16) {
            Title("Sign in, if you like", "A verified name and email show on your knocks, so people know it's really you. Knock works without it too.")
            if let me = model.me, me.verified {
                Label("Signed in as \(me.name)", systemImage: "checkmark.seal.fill").foregroundStyle(.green)
            } else {
                VStack(spacing: 10) {
                    ForEach(model.providers, id: \.self) { p in
                        Button { model.signIn(p) } label: {
                            Label("Continue with \(p == "github" ? "GitHub" : "Google")",
                                  systemImage: p == "github" ? "chevron.left.forwardslash.chevron.right" : "g.circle.fill")
                                .frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.glass)
                        .controlSize(.extraLarge)
                    }
                    if model.providers.isEmpty {
                        Text(model.connected ? "This relay doesn't offer sign-in. You'll knock as \"\(model.settings.displayName)\", marked not verified."
                                             : "Can't reach the relay yet. You can sign in later from Settings.")
                            .foregroundStyle(.secondary)
                    }
                    if model.signingIn { ProgressView().controlSize(.small) }
                    if let e = model.authError { Text(e).foregroundStyle(.orange).font(.callout) }
                }
                Text("You stay signed in on this Mac. The sign-in renews itself while you use Knock.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            HStack {
                back
                Spacer()
                if model.signedIn { primary("Continue") } else {
                    Button("Skip for now") { next() }.buttonStyle(.glass).controlSize(.large)
                }
            }
        }
    }

    private var notify: some View {
        VStack(alignment: .leading, spacing: 16) {
            Title("Don't miss a knock", "Knocks drop from the notch and show as notifications, even when Knock is in the background. A knock waits 60 seconds.")
            HStack(spacing: 14) {
                Image(systemName: notifications == .authorized ? "bell.badge.fill" : "bell.fill")
                    .font(.title)
                    .foregroundStyle(Color.accentColor)
                VStack(alignment: .leading) {
                    Text("Notifications").font(.headline)
                    Text(notificationText).foregroundStyle(.secondary).font(.callout)
                }
                Spacer()
                if notifications == .notDetermined {
                    Button("Allow") {
                        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { _, _ in
                            Task { @MainActor in refreshNotifications() }
                        }
                    }
                    .buttonStyle(.glassProminent)
                } else if notifications == .denied {
                    Button("Open Settings") {
                        NSWorkspace.shared.open(URL(string: "x-apple.systempreferences:com.apple.Notifications-Settings.extension")!)
                    }
                    .buttonStyle(.glass)
                }
            }
            Toggle("Drop knocks from the notch", isOn: Bindable(model.settings).useNotch)
            HStack {
                back
                Spacer()
                primary("Continue")
            }
        }
    }

    private var diskAccess: some View {
        VStack(alignment: .leading, spacing: 16) {
            Title("Let the shell reach your files",
                  "The shared shell runs inside Knock, so macOS asks about Knock. Without Full Disk Access, commands like ls ~/Documents fail, and macOS can stop you mid-session to ask.")
            HStack(spacing: 14) {
                Image(systemName: model.fullDiskAccess ? "externaldrive.fill.badge.checkmark" : "externaldrive.fill")
                    .font(.title)
                    .foregroundStyle(Color.accentColor)
                VStack(alignment: .leading) {
                    Text("Full Disk Access").font(.headline)
                    Text(model.fullDiskAccess ? "On. The shell can reach Documents, Desktop, Downloads, iCloud Drive and Mail."
                                              : "Off. Turn on Knock in the list, then come back here.")
                        .foregroundStyle(.secondary).font(.callout)
                        .fixedSize(horizontal: false, vertical: true)
                }
                Spacer()
                if !model.fullDiskAccess {
                    Button("Open Settings") { NSWorkspace.shared.open(FullDiskAccess.settingsURL) }
                        .buttonStyle(.glass)
                }
            }
            Text(model.fullDiskAccess
                 ? "Anyone you let in can reach these files too, just as you can."
                 : "macOS may ask you to quit and reopen Knock. Shells already running won't see the change. You can turn it on later in Settings > Security.")
                .font(.caption).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            HStack {
                back
                Spacer()
                if model.fullDiskAccess { primary("Continue") } else {
                    Button("Not now") { next() }.buttonStyle(.glass).controlSize(.large)
                }
            }
        }
        .onAppear(perform: model.refreshFullDiskAccess)
    }

    private var ready: some View {
        VStack(spacing: 16) {
            Text("You're ready").font(.system(size: 28, weight: .bold))
            Text("This is your code. Send it to someone you trust when you want help.")
                .multilineTextAlignment(.center).foregroundStyle(.secondary)
            Button(action: model.copyCode) {
                VStack(spacing: 4) {
                    CodeText(share: model.door?.share, size: 44)
                    Text("Click to copy").font(.callout).foregroundStyle(.secondary)
                }
                .padding(.vertical, 14).padding(.horizontal, 30)
                .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 20, style: .continuous))
            }
            .buttonStyle(.plain)
            if model.copiedAt != nil {
                Label("Copied", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
            }
            Toggle("Open to knocks now", isOn: $openNow).toggleStyle(.switch)
            Text("Knock lives in your menu bar. Closing this window keeps your door as it is; quitting closes it.")
                .font(.caption).foregroundStyle(.secondary).multilineTextAlignment(.center)
            HStack {
                back
                Spacer()
                primary("Start using Knock") {
                    model.settings.onboarded = true
                    model.setOpen(openNow)
                }
            }
        }
    }

    // MARK: Pieces

    private var relayHost: String { URL(string: relay)?.host ?? "not set" }

    private var notificationText: String {
        switch notifications {
        case .authorized, .provisional, .ephemeral: "On. You'll get Let in and Not now buttons."
        case .denied: "Off. Turn them on in System Settings to see knocks in the background."
        default: "Lets Knock tell you when someone knocks."
        }
    }

    private func refreshNotifications() {
        UNUserNotificationCenter.current().getNotificationSettings { s in
            let status = s.authorizationStatus
            Task { @MainActor in notifications = status }
        }
    }

    private func next() { withAnimation(reduceMotion ? nil : .smooth) { step = min(step + 1, steps - 1) } }

    private var back: some View {
        Button("Back") { withAnimation(reduceMotion ? nil : .smooth) { step = max(step - 1, 0) } }
            .buttonStyle(.borderless)
            .opacity(step == 0 ? 0 : 1)
    }

    private func primary(_ title: String, action: (() -> Void)? = nil) -> some View {
        Button { if let action { action() } else { next() } } label: {
            Text(title).font(.headline).padding(.horizontal, 10)
        }
        .buttonStyle(.glassProminent)
        .controlSize(.extraLarge)
        .keyboardShortcut(.defaultAction)
    }
}

private struct Title: View {
    let title: String
    let subtitle: String
    init(_ title: String, _ subtitle: String) { self.title = title; self.subtitle = subtitle }
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(title).font(.system(size: 26, weight: .bold))
            Text(subtitle).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
        .padding(.bottom, 6)
    }
}

private struct Rule: View {
    let icon: String
    let text: String
    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: icon).foregroundStyle(Color.accentColor).frame(width: 22)
            Text(text)
        }
    }
}
