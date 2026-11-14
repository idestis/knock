import SwiftUI

enum SettingsTab: Hashable { case general, account, sessions, security }

struct SettingsView: View {
    @State private var tab = SettingsTab.general

    var body: some View {
        TabView(selection: $tab) {
            Tab("General", systemImage: "gearshape", value: .general) { GeneralSettings() }
            Tab("Account", systemImage: "person.crop.circle", value: .account) { AccountSettings() }
            Tab("Sessions", systemImage: "terminal", value: .sessions) { SessionSettings() }
            Tab("Security", systemImage: "lock.shield", value: .security) { SecuritySettings { tab = .sessions } }
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
                Text(s.isManaged("relay.url") ? "Your organisation manages the relay." : "The knockd relay that brokers knocks. Each relay has its own door, code and sign-in. Quit Knock and nobody can reach you.")
                    .foregroundStyle(.secondary)
            }
            Section {
                Picker("Make a new code after", selection: $s.codeMaxAgeDays) {
                    ForEach(CodeAge.choices(including: s.codeMaxAgeDays), id: \.self) { Text(CodeAge.label(days: $0)).tag($0) }
                }
                .disabled(s.isManaged("door.code_max_age_days"))
            } footer: {
                Text(s.isManaged("door.code_max_age_days")
                     ? "Your organisation sets how long a code lasts."
                     : "An old code is replaced when your door opens or sits idle, never during a session. You'll need to share the new one.")
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
        model.changeRelay(to: relay)
    }
}

private struct AccountSettings: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        @Bindable var s = model.settings
        Form {
            if let me = model.me, me.verified {
                Section {
                    LabeledContent("Signed in as") {
                        VStack(alignment: .trailing) {
                            Text(me.name)
                            Text(me.verifiedDetail ?? me.providerLabel).foregroundStyle(.secondary)
                        }
                    }
                    LabeledContent("With", value: me.providerLabel)
                } footer: {
                    Text("Your verified name and email show on every knock. You stay signed in on this Mac; the sign-in renews itself while you use Knock.")
                        .foregroundStyle(.secondary)
                }
                Section {
                    Button("Sign Out", role: .destructive, action: model.signOut)
                }
            } else {
                Section {
                    TextField("Your name", text: $s.displayName)
                        .onSubmit(model.displayNameChanged)
                        .onDisappear(perform: model.displayNameChanged)
                } footer: {
                    Text("Without an account, people you knock on see this name marked \"not verified\".")
                        .foregroundStyle(.secondary)
                }
                Section {
                    if model.providers.isEmpty {
                        LabeledContent("Sign in") {
                            Text(model.connected ? "Not offered by this relay" : "Relay not connected").foregroundStyle(.secondary)
                        }
                    } else {
                        ForEach(model.providers, id: \.self) { p in
                            LabeledContent(p == "github" ? "GitHub" : "Google") {
                                Button("Sign In") { model.signIn(p) }
                                    .disabled(model.signingIn)
                            }
                        }
                    }
                    if let e = model.authError {
                        Text(e).foregroundStyle(.orange)
                    }
                } header: {
                    HStack {
                        Text("Sign in")
                        if model.signingIn { ProgressView().controlSize(.mini) }
                    }
                } footer: {
                    Text(model.signInRequired
                         ? "This relay needs you to sign in before you can open your door or knock. You stay signed in on this Mac."
                         : "Optional. Signing in puts your verified name on every knock, and you stay signed in on this Mac.")
                        .foregroundStyle(.secondary)
                }
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

/// Honest controls only. Nothing here filters commands or paths: inside a shell
/// that would be trivial to get around, so it would look like protection and not be.
private struct SecuritySettings: View {
    @Environment(AppModel.self) private var model
    var showSessions: () -> Void = {}

    var body: some View {
        @Bindable var s = model.settings
        Form {
            Section {
                LabeledContent("Full Disk Access") {
                    HStack(spacing: 10) {
                        Label(model.fullDiskAccess ? "On" : "Off",
                              systemImage: model.fullDiskAccess ? "checkmark.circle.fill" : "exclamationmark.circle")
                            .foregroundStyle(model.fullDiskAccess ? .green : .orange)
                        Button("Open System Settings") { NSWorkspace.shared.open(FullDiskAccess.settingsURL) }
                    }
                }
            } footer: {
                Text(model.fullDiskAccess
                     ? "The shared shell can reach Documents, Desktop, Downloads, iCloud Drive and Mail, like your own Terminal."
                     : "Without it, the shared shell can't list Documents, Desktop, Downloads, iCloud Drive or Mail, and macOS may ask you mid-session. After you turn it on, quit and reopen Knock.")
                    .foregroundStyle(.secondary)
            }
            Section {
                Picker("When a session starts", selection: $s.guestInputDefault) {
                    Text("Allow typing").tag(true)
                    Text("Watch only").tag(false)
                }
                .disabled(s.isManaged("guest.input"))
                Toggle("Only signed-in guests", isOn: $s.verifiedGuestsOnly)
                    .disabled(s.isManaged("guest.verified_only"))
                LabeledContent("Recording") {
                    HStack(spacing: 10) {
                        Text(s.hostRecording.label).foregroundStyle(.secondary)
                        Button("Change…", action: showSessions)
                    }
                }
            } header: {
                Text("Guests")
            } footer: {
                Text("Watch only starts each session in Hands off; turn it off from the toolbar to let them type. Only signed-in guests turns away anyone who isn't signed in, without asking you. If your relay has no sign-in, that means every knock.")
                    .foregroundStyle(.secondary)
            }
            Section {
                Label("Anyone you let in can do what you can do in that shell. Turn on Hands off before you type a password.", systemImage: "hand.raised.fill")
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .formStyle(.grouped)
        .onAppear(perform: model.refreshFullDiskAccess)
    }
}
