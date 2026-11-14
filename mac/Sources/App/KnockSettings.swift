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
        get {
            access(keyPath: \.displayName)
            #if DEBUG
            if let demo = ProcessInfo.processInfo.environment["KNOCK_DEMO_NAME"] { return demo }
            #endif
            return d.string(forKey: "display_name") ?? NSFullUserName()
        }
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

    /// Turn away knocks from people who aren't signed in, before the host is asked.
    var verifiedGuestsOnly: Bool {
        get { access(keyPath: \.verifiedGuestsOnly); return bool("guest.verified_only", file: "guest.verified_only", false) }
        set { withMutation(keyPath: \.verifiedGuestsOnly) { d.set(newValue, forKey: "guest.verified_only") } }
    }

    /// `door.code_max_age_days`: a code older than this is replaced when the door opens or
    /// sits idle, never during a session. 0 keeps a code until "New code".
    var codeMaxAgeDays: Int {
        get {
            access(keyPath: \.codeMaxAgeDays)
            if d.object(forKey: "door.code_max_age_days") != nil { return max(0, d.integer(forKey: "door.code_max_age_days")) }
            return max(0, Int(file["door.code_max_age_days"] ?? "") ?? CodeAge.defaultDays)
        }
        set { withMutation(keyPath: \.codeMaxAgeDays) { d.set(max(0, newValue), forKey: "door.code_max_age_days") } }
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
