import SwiftUI

/// Window backdrop behind the glass sidebar (mock 01).
struct KnockBackdrop: View {
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        LinearGradient(
            colors: scheme == .dark
                ? [Color(red: 0.11, green: 0.12, blue: 0.20), Color(red: 0.09, green: 0.09, blue: 0.11)]
                : [Color(red: 0.86, green: 0.88, blue: 0.97), Color(red: 0.95, green: 0.95, blue: 0.97)],
            startPoint: .topLeading, endPoint: .bottomTrailing)
        .ignoresSafeArea()
    }
}

/// Solid surface for lists and body text (glass is for navigation only).
struct Card<Content: View>: View {
    @ViewBuilder var content: Content
    var body: some View {
        content
            .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 22, style: .continuous))
            .shadow(color: .black.opacity(0.06), radius: 10, y: 2)
    }
}

/// Mock 01: glass sidebar card (open switch, code, account) and the list.
struct HomeView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        ZStack {
            KnockBackdrop()
            if model.settings.onboarded {
                HStack(spacing: 0) {
                    SidebarView()
                        .frame(width: 300)
                        .frame(maxHeight: .infinity)
                        .glassEffect(.regular.tint(Color.white.opacity(0.35)), in: .rect(cornerRadius: 26))
                        .padding(10)
                    MainColumn()
                        .padding(.leading, 22)
                        .padding(.trailing, 26)
                        .padding(.top, 26)
                        .padding(.bottom, 22)
                }
                InWindowKnockAlert()
            } else {
                OnboardingView()
            }
        }
        .ignoresSafeArea()
        .frame(minWidth: 860, minHeight: 560)
        .animation(.smooth, value: model.incoming.first?.knockId)
        .animation(.smooth, value: model.settings.onboarded)
    }
}

extension Notification.Name {
    static let knockFocusSearch = Notification.Name("knock.focusSearch")
}

struct KnockLogo: View {
    var size: CGFloat = 52
    var body: some View {
        Image("KnockMark")
            .resizable()
            .scaledToFit()
            .padding(size * 0.2)
            .frame(width: size, height: size)
            .background(.white, in: RoundedRectangle(cornerRadius: size * 0.25, style: .continuous))
            .shadow(color: .black.opacity(0.10), radius: 5, y: 2)
    }
}

struct SidebarView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Spacer().frame(height: 52) // traffic lights sit here

            HStack(spacing: 14) {
                KnockLogo()
                VStack(alignment: .leading, spacing: 2) {
                    Text("Knock").font(.system(size: 22, weight: .semibold))
                    Text("Knock, knock. Who's there?").font(.callout).foregroundStyle(.secondary)
                }
            }
            .padding(.bottom, 30)

            HStack {
                Text("Open to knocks").font(.headline)
                Spacer()
                Toggle("Open to knocks", isOn: Binding(get: { model.isOpen }, set: { model.setOpen($0) }))
                    .labelsHidden()
                    .toggleStyle(.switch)
                    .controlSize(.large)
                    .disabled(!model.doorReady)
            }
            Text(model.isOpen ? "People with your code can see you're here and knock. You choose who comes in."
                              : "Nobody can knock. Your code works again when you open up.")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.top, 12)

            Divider().padding(.vertical, 22)

            HStack {
                Text("Your code").font(.callout).foregroundStyle(.secondary)
                Spacer()
                Button { model.newCode() } label: {
                    Image(systemName: "arrow.trianglehead.2.clockwise")
                        .font(.system(size: 15, weight: .semibold))
                        .frame(width: 34, height: 34)
                        .rotationEffect(.degrees(model.rotatingCode ? 180 : 0))
                }
                .buttonStyle(.plain)
                .background(Circle().fill(Color(nsColor: .controlBackgroundColor)).shadow(color: .black.opacity(0.10), radius: 4, y: 1))
                .help("New code. The old one stops working straight away.")
                .disabled(model.rotatingCode)
            }
            Button(action: model.copyCode) {
                CodeText(share: model.door?.share, size: 34)
                    .opacity(model.rotatingCode ? 0.35 : 1)
            }
            .buttonStyle(.plain)
            .help("Copy \"\(model.door?.share.text ?? "")\"")
            .padding(.top, 2)

            Group {
                if model.copiedAt != nil {
                    Label("Copied", systemImage: "checkmark.circle.fill")
                        .foregroundStyle(.green)
                } else {
                    Text("Click the code to copy it. A new code stops the old one working straight away.")
                        .foregroundStyle(.secondary)
                }
            }
            .font(.callout)
            .fixedSize(horizontal: false, vertical: true)
            .padding(.top, 12)

            if let notice = model.doorNotice {
                Label(notice, systemImage: "info.circle")
                    .font(.callout)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.top, 12)
            }

            Spacer(minLength: 20)
            AccountRow()
        }
        .padding(.horizontal, 24)
        .padding(.bottom, 22)
        .animation(.snappy, value: model.door?.code)
        .animation(.default, value: model.copiedAt)
    }
}

struct AccountRow: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Menu {
            if model.signedIn {
                Button("Sign Out", action: model.signOut)
            } else {
                ForEach(model.providers, id: \.self) { p in
                    Button("Sign in with \(p == "github" ? "GitHub" : "Google")") { model.signIn(p) }
                }
                if model.providers.isEmpty { Text("This relay doesn't offer sign-in") }
            }
            Divider()
            SettingsLink { Text("Settings…") }
        } label: {
            HStack(spacing: 12) {
                Avatar(identity: nil, initials: initials, size: 40)
                VStack(alignment: .leading, spacing: 1) {
                    Text(model.me?.name ?? model.settings.displayName).font(.headline).lineLimit(1)
                    Text(subtitle).font(.callout).foregroundStyle(.secondary).lineLimit(1)
                }
                Spacer(minLength: 0)
            }
            .contentShape(Rectangle())
        }
        .menuStyle(.button)
        .buttonStyle(.plain)
        .menuIndicator(.hidden)
    }

    private var initials: String {
        Identity(id: "", name: model.me?.name ?? model.settings.displayName, provider: "", verified: false).initials
    }

    private var subtitle: String {
        if let me = model.me, me.verified { return me.email ?? AppInfo.deviceName }
        return AppInfo.deviceName
    }
}

struct MainColumn: View {
    @Environment(AppModel.self) private var model
    @State private var query = ""
    @FocusState private var searchFocused: Bool

    private var share: ShareString? { ShareString.parse(query) }

    private var rows: [RecentDoor] {
        guard !query.isEmpty, share == nil else { return model.recent }
        return model.recent.filter { $0.ownerName.localizedCaseInsensitiveContains(query) || $0.device.localizedCaseInsensitiveContains(query) }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 20) {
            HStack(alignment: .center) {
                Text(model.recent.isEmpty ? "Knock on a door" : "Online now")
                    .font(.system(size: 32, weight: .bold))
                Spacer(minLength: 20)
                SearchCapsule(text: $query, focused: $searchFocused, onSubmit: submit)
                    .frame(width: 280)
            }

            if let out = model.outgoing { OutgoingBanner(outgoing: out) }

            if let share {
                Button { knock(share) } label: {
                    Label("Knock with \(share.text)", systemImage: "hand.wave")
                        .font(.headline)
                        .frame(maxWidth: .infinity)
                        .padding(.vertical, 4)
                }
                .buttonStyle(.glassProminent)
                .controlSize(.extraLarge)
                .disabled(!model.connected || model.outgoing != nil)
            }

            if model.recent.isEmpty {
                Card {
                    VStack(spacing: 14) {
                        Image(systemName: "door.left.hand.closed")
                            .font(.system(size: 34))
                            .foregroundStyle(Color.accentColor)
                        Text("No one yet").font(.title3.weight(.semibold))
                        Text("Ask the person you're helping for their code, like **BVJY 4821 KQ**, and type it in the search field. After they let you in once, they show up here and you can knock with one click.")
                            .multilineTextAlignment(.center)
                            .foregroundStyle(.secondary)
                            .frame(maxWidth: 420)
                    }
                    .padding(36)
                    .frame(maxWidth: .infinity)
                }
            } else {
                Card {
                    if rows.count <= 6 {
                        list
                    } else {
                        ScrollView { list }.scrollBounceBehavior(.basedOnSize)
                    }
                }
            }

            Spacer(minLength: 0)

            HStack {
                Text("Quit Knock and nobody can knock.").foregroundStyle(.secondary)
                Spacer()
                ConnectionBadge()
            }
            .font(.callout)
        }
        .onReceive(NotificationCenter.default.publisher(for: .knockFocusSearch)) { _ in searchFocused = true }
    }

    private var list: some View {
        VStack(spacing: 0) {
            ForEach(rows) { r in
                RecentRow(row: r, presence: model.presence[r.doorId])
                if r.id != rows.last?.id { Divider().padding(.horizontal, 26) }
            }
        }
        .padding(.vertical, 6)
    }

    private func knock(_ s: ShareString) {
        model.knock(share: s)
        query = ""
    }

    private func submit() {
        if let share { knock(share); return }
        if let match = rows.first, model.presence[match.doorId] == .online, match.mismatchSince == nil {
            model.knock(recent: match)
            query = ""
        }
    }
}

struct SearchCapsule: View {
    @Binding var text: String
    var focused: FocusState<Bool>.Binding
    var onSubmit: () -> Void

    var body: some View {
        HStack(spacing: 10) {
            Image(systemName: "magnifyingglass").foregroundStyle(.secondary).font(.system(size: 15, weight: .medium))
            TextField("Search or enter a code", text: $text)
                .textFieldStyle(.plain)
                .font(.system(size: 15))
                .focused(focused)
                .onSubmit(onSubmit)
            if !text.isEmpty {
                Button { text = "" } label: { Image(systemName: "xmark.circle.fill").foregroundStyle(.tertiary) }
                    .buttonStyle(.plain)
            }
        }
        .padding(.horizontal, 16)
        .frame(height: 42)
        .background(Color(nsColor: .controlBackgroundColor), in: Capsule())
        .overlay(Capsule().strokeBorder(.separator.opacity(0.5)))
        .shadow(color: .black.opacity(0.04), radius: 3, y: 1)
    }
}

struct ConnectionBadge: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        HStack(spacing: 6) {
            Circle().fill(color).frame(width: 8, height: 8)
            Text(text)
        }
        .help(help)
    }

    private var color: Color {
        switch model.connection {
        case .connected: .green
        case .connecting: .yellow
        default: .red
        }
    }

    private var text: String {
        switch model.connection {
        case .connected: "Connected"
        case .connecting: "Connecting…"
        case .idle: "Not connected"
        case .offline: "Offline"
        }
    }

    private var help: String {
        if case .offline(let m) = model.connection { return m }
        return model.settings.relayURLString
    }
}

struct RecentRow: View {
    @Environment(AppModel.self) private var model
    let row: RecentDoor
    let presence: Presence?
    @State private var hover = false

    var body: some View {
        HStack(spacing: 16) {
            Avatar(identity: nil, initials: Identity(id: "", name: row.ownerName, provider: "", verified: false).initials, size: 48, neutral: true)
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 6) {
                    Text(row.ownerName).font(.system(size: 16, weight: .semibold))
                    if row.verified == false {
                        Text("not verified").font(.caption).foregroundStyle(.orange)
                    }
                }
                HStack(spacing: 7) {
                    Circle().fill(dot).frame(width: 9, height: 9)
                    Text("\(status) · \(row.device)").foregroundStyle(.secondary)
                }
                .font(.system(size: 14))
            }
            Spacer()
            trailing
        }
        .padding(.horizontal, 26)
        .padding(.vertical, 14)
        .contentShape(Rectangle())
        .onHover { hover = $0 }
        .contextMenu {
            Button("Remove from List", role: .destructive) { model.removeRecent(row) }
        }
    }

    @ViewBuilder private var trailing: some View {
        switch effective {
        case .online:
            Button { model.knock(recent: row) } label: {
                Text("Knock").font(.system(size: 16, weight: .semibold)).padding(.horizontal, 14).padding(.vertical, 6)
            }
            .buttonStyle(.plain)
            .foregroundStyle(Color.accentColor)
            .background(Color.accentColor.opacity(0.10), in: Capsule())
            .disabled(model.outgoing != nil || model.session != nil)
        case .codeChanged:
            Text("Ask for a new code").foregroundStyle(.secondary)
        case .busy:
            Text("Busy").foregroundStyle(.secondary).padding(.trailing, 12)
        case .offline, nil:
            if hover {
                Button("Remove") { model.removeRecent(row) }.buttonStyle(.borderless).foregroundStyle(.secondary)
            }
        }
    }

    private var effective: Presence? { row.mismatchSince != nil ? .codeChanged : presence }

    private var status: String {
        switch effective {
        case .online: "Open"
        case .busy: "In a session"
        case .codeChanged: "Code changed"
        case .offline: "Door closed"
        case nil: model.connected ? "Checking…" : "Unknown"
        }
    }

    private var dot: Color {
        switch effective {
        case .online: .green
        case .busy, .codeChanged: .orange
        default: .gray
        }
    }
}

struct OutgoingBanner: View {
    @Environment(AppModel.self) private var model
    let outgoing: Outgoing

    var body: some View {
        HStack(spacing: 12) {
            switch outgoing.phase {
            case .preparing:
                ProgressView().controlSize(.small)
                Text("Checking the code…")
            case .waiting:
                ProgressView().controlSize(.small)
                Text("Knock, knock. Waiting for \(outgoing.name) to let you in…")
            case .failed(let msg):
                Image(systemName: "exclamationmark.circle.fill").foregroundStyle(.orange)
                Text(msg)
            }
            Spacer()
            Button(isFailed ? "OK" : "Cancel") { model.cancelKnock() }
                .buttonStyle(.glass)
        }
        .font(.callout)
        .padding(14)
        .glassEffect(.regular, in: .rect(cornerRadius: 18))
    }

    private var isFailed: Bool { if case .failed = outgoing.phase { true } else { false } }
}

/// Mock 02: the knock as a centred glass alert inside the window.
struct InWindowKnockAlert: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        if let k = model.incoming.first {
            ZStack {
                Rectangle().fill(.black.opacity(0.22)).ignoresSafeArea()
                KnockAlertCard(knock: k, onAllow: { model.answer(k, allow: true) }, onDeny: { model.answer(k, allow: false) })
                    .onExitCommand { model.answer(k, allow: false) }
            }
            .transition(.opacity)
        }
    }
}

struct KnockAlertCard: View {
    let knock: KnockIncoming
    let onAllow: () -> Void
    let onDeny: () -> Void

    var body: some View {
        VStack(spacing: 0) {
            ZStack(alignment: .bottomTrailing) {
                Avatar(identity: knock.from, size: 84)
                Image("KnockMark")
                    .resizable().scaledToFit()
                    .padding(6)
                    .frame(width: 30, height: 30)
                    .background(Circle().fill(.white))
                    .overlay(Circle().strokeBorder(.white, lineWidth: 2))
                    .shadow(color: .black.opacity(0.15), radius: 2, y: 1)
                    .offset(x: 4, y: 2)
            }
            .padding(.bottom, 18)
            Text("Knock, knock.").font(.system(size: 26, weight: .bold))
            Text("\(knock.from.name) wants to come in from \(knock.device).")
                .font(.system(size: 15))
                .multilineTextAlignment(.center)
                .padding(.top, 8)
            VerifiedLine(identity: knock.from).padding(.top, 6)
            Text("You'll share one shell and see everything \(knock.from.firstName) types. You can end it any time.")
                .font(.system(size: 14))
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.top, 14)
            HStack(spacing: 14) {
                Button(action: onDeny) { Text("Not now").frame(maxWidth: .infinity) }
                    .buttonStyle(.glass)
                Button(action: onAllow) { Text("Let \(knock.from.firstName) in").frame(maxWidth: .infinity) }
                    .buttonStyle(.glassProminent)
                // No default button: Return must not let anyone in.
            }
            .controlSize(.extraLarge)
            .font(.system(size: 15, weight: .semibold))
            .padding(.top, 24)
        }
        .padding(28)
        .frame(width: 380)
        .glassEffect(.regular, in: .rect(cornerRadius: 30))
    }
}

struct VerifiedLine: View {
    let identity: Identity
    var body: some View {
        if !identity.verified {
            Label("Not verified. Anyone can type any name.", systemImage: "exclamationmark.triangle.fill")
                .font(.caption).foregroundStyle(.orange)
        } else {
            Label("\(identity.email ?? identity.name) · \(identity.provider == "github" ? "GitHub" : "Google")", systemImage: "checkmark.seal.fill")
                .font(.caption).foregroundStyle(.secondary)
        }
    }
}

/// "BVJY 1696 ME": the door part in a softer tone, the code in full strength.
struct CodeText: View {
    let share: ShareString?
    var size: CGFloat = 34

    var body: some View {
        let font = Font.system(size: size, weight: .heavy, design: .rounded).monospacedDigit()
        Group {
            if let share {
                Text("\(Text(share.doorId.uppercased()).foregroundStyle(Color.accentColor)) \(Text(Codes.display(share.code)).foregroundStyle(.primary))")
            } else {
                Text("···· ···· ··").foregroundStyle(.tertiary)
            }
        }
        .font(font)
        .lineLimit(1)
        .minimumScaleFactor(0.6)
        .contentTransition(.numericText())
    }
}
