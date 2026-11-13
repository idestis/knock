import SwiftUI

struct SettingsView: View {
    var body: some View {
        TabView {
            Tab("General", systemImage: "gearshape") { GeneralSettings() }
            Tab("Account", systemImage: "person.crop.circle") { AccountSettings() }
            Tab("Sessions", systemImage: "terminal") { SessionSettings() }
        }
        .frame(width: 520)
        .scenePadding()
    }
}

private struct GeneralSettings: View {
    @Environment(AppModel.self) private var model
    @State private var relay = ""

    var body: some View {
        @Bindable var s = model.settings
        Form {
            Section {
                TextField("Relay", text: $relay, prompt: Text("https://knock.example.ts.net"))
                    .onSubmit(applyRelay)
                    .disabled(s.isManaged("relay.url"))
                HStack {
                    ConnectionBadge()
                    Spacer()
                    Button("Reconnect", action: applyRelay)
                }
            } footer: {
                Text(s.isManaged("relay.url") ? "Your organisation manages the relay." : "The knockd relay that brokers knocks. Quit Knock and nobody can reach you.")
                    .foregroundStyle(.secondary)
            }
            Section {
                Toggle("Open to knocks when Knock starts", isOn: $s.openOnLaunch)
                Toggle("Drop knocks from the notch", isOn: $s.useNotch)
                Toggle("Show knocks as notifications", isOn: $s.useNotifications)
            }
            Section {
                LabeledContent("Version", value: "\(AppInfo.version) (\(AppInfo.build))")
                Button("Check for Updates…") { Updater.shared.checkForUpdates() }
                    .disabled(!Updater.shared.isEnabled)
            }
        }
        .formStyle(.grouped)
        .onAppear { relay = s.relayURLString }
    }

    private func applyRelay() {
        model.settings.relayURLString = relay
        model.relay.stop()
        model.connectRelay()
    }
}

private struct AccountSettings: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        @Bindable var s = model.settings
        Form {
            Section {
                if let me = model.me, me.verified {
                    LabeledContent("Signed in as") {
                        VStack(alignment: .trailing) {
                            Text(me.name)
                            Text(me.email ?? me.provider.capitalized).foregroundStyle(.secondary)
                        }
                    }
                    Button("Sign Out", role: .destructive, action: model.signOut)
                } else {
                    TextField("Your name", text: $s.displayName)
                    Text("You're using Knock without an account. People you knock on see this name marked \"not verified\".")
                        .font(.callout).foregroundStyle(.secondary)
                    HStack {
                        ForEach(model.providers, id: \.self) { p in
                            Button {
                                model.signIn(p)
                            } label: {
                                Label("Sign in with \(p == "github" ? "GitHub" : "Google")", systemImage: p == "github" ? "chevron.left.forwardslash.chevron.right" : "g.circle")
                            }
                            .buttonStyle(.glass)
                        }
                        if model.signingIn { ProgressView().controlSize(.small) }
                    }
                    if model.providers.isEmpty {
                        Text("This relay doesn't offer sign-in.").foregroundStyle(.secondary)
                    }
                }
                if let e = model.authError { Text(e).foregroundStyle(.orange) }
            } footer: {
                Text("Signing in puts your verified name on every knock. You stay signed in on this Mac; the session renews itself while you use Knock.")
                    .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
    }
}

private struct SessionSettings: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        @Bindable var s = model.settings
        Form {
            Section("When someone is in your shell") {
                Toggle("Let guests type by default", isOn: $s.guestInputDefault)
                Picker("Record", selection: $s.hostRecording) {
                    ForEach(Recorder.Mode.allCases) { Text($0.label).tag($0) }
                }
            }
            Section("When you're in someone's shell") {
                Picker("Record", selection: $s.guestRecording) {
                    ForEach([Recorder.Mode.off, .output]) { Text($0.label).tag($0) }
                }
            }
            Section {
                LabeledContent("Recordings") {
                    Button("Show in Finder") {
                        try? FileManager.default.createDirectory(at: s.sessionsDirectory, withIntermediateDirectories: true)
                        NSWorkspace.shared.open(s.sessionsDirectory)
                    }
                }
            } footer: {
                Text("Recordings stay on this Mac for \(s.retentionDays) days. Recording keystrokes captures things that never appear on screen, such as passwords, so it's off unless you choose it.")
                    .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
    }
}
