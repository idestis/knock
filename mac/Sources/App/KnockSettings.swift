import Foundation
import Observation

/// Settings: managed preferences (MDM) > UserDefaults > ~/.knock/config.toml > built-in default.
/// UserDefaults already layers the managed domain on top, so `isManaged` only drives the UI.
@Observable
@MainActor
final class KnockSettings {
    private let d = UserDefaults.standard
    private let file = ConfigFile.load()

    private func string(_ key: String, file fileKey: String, _ def: String) -> String {
        d.string(forKey: key) ?? file[fileKey] ?? def
    }

    private func bool(_ key: String, file fileKey: String, _ def: Bool) -> Bool {
        if d.object(forKey: key) != nil { return d.bool(forKey: key) }
        if let v = file[fileKey] { return v == "true" }
        return def
    }

    func isManaged(_ key: String) -> Bool { d.objectIsForced(forKey: key) }

    var relayURLString: String {
        get { access(keyPath: \.relayURLString); return string("relay.url", file: "relay.url", Bundle.main.object(forInfoDictionaryKey: "KnockRelayURL") as? String ?? "") }
        set { withMutation(keyPath: \.relayURLString) { d.set(newValue.trimmingCharacters(in: .whitespaces), forKey: "relay.url") } }
    }

    var relayURL: URL? {
        guard let u = URL(string: relayURLString), let s = u.scheme, ["http", "https"].contains(s), u.host != nil else { return nil }
        return u
    }

    var displayName: String {
        get { access(keyPath: \.displayName); return d.string(forKey: "display_name") ?? NSFullUserName() }
        set { withMutation(keyPath: \.displayName) { d.set(newValue, forKey: "display_name") } }
    }

    var onboarded: Bool {
        get { access(keyPath: \.onboarded); return d.bool(forKey: "onboarded") }
        set { withMutation(keyPath: \.onboarded) { d.set(newValue, forKey: "onboarded") } }
    }

    var openOnLaunch: Bool {
        get { access(keyPath: \.openOnLaunch); return bool("door.open_on_launch", file: "knock.open_on_launch", true) }
        set { withMutation(keyPath: \.openOnLaunch) { d.set(newValue, forKey: "door.open_on_launch") } }
    }

    var useNotch: Bool {
        get { access(keyPath: \.useNotch); return bool("knock.notch", file: "knock.notch", true) }
        set { withMutation(keyPath: \.useNotch) { d.set(newValue, forKey: "knock.notch") } }
    }

    var useNotifications: Bool {
        get { access(keyPath: \.useNotifications); return bool("knock.notifications", file: "knock.notifications", true) }
        set { withMutation(keyPath: \.useNotifications) { d.set(newValue, forKey: "knock.notifications") } }
    }

    var guestInputDefault: Bool {
        get { access(keyPath: \.guestInputDefault); return string("guest.input", file: "guest.input", "allow") != "view-only" }
        set { withMutation(keyPath: \.guestInputDefault) { d.set(newValue ? "allow" : "view-only", forKey: "guest.input") } }
    }

    var hostRecording: Recorder.Mode {
        get { access(keyPath: \.hostRecording); return Recorder.Mode(rawValue: string("sessions.save", file: "sessions.save", "output")) ?? .output }
        set { withMutation(keyPath: \.hostRecording) { d.set(newValue.rawValue, forKey: "sessions.save") } }
    }

    var guestRecording: Recorder.Mode {
        get { access(keyPath: \.guestRecording); return Recorder.Mode(rawValue: d.string(forKey: "sessions.save_guest") ?? "off") ?? .off }
        set { withMutation(keyPath: \.guestRecording) { d.set(newValue.rawValue, forKey: "sessions.save_guest") } }
    }

    func recording(host: Bool) -> Recorder.Mode { host ? hostRecording : guestRecording }

    var sessionsDirectory: URL {
        let p = string("sessions.dir", file: "sessions.dir", "~/.knock/sessions")
        return URL(fileURLWithPath: (p as NSString).expandingTildeInPath)
    }

    var retentionDays: Int {
        Int(string("sessions.retention_days", file: "sessions.retention_days", "30")) ?? 30
    }

    var shellCommand: String? {
        let s = string("shell.command", file: "shell.command", "")
        return s.isEmpty ? nil : s
    }
}
