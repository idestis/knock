import Darwin
import Foundation

/// The relay a client talks to, reduced to `scheme://host[:port]`. Door keys, code hashes
/// and sign-ins are bearer secrets for the relay that accepted them, so they are kept per
/// origin and never sent to another relay. See docs/LOCAL_STATE.md, "Per-relay state".
nonisolated struct RelayOrigin: Equatable, Hashable, Sendable, CustomStringConvertible {
    let scheme: String
    /// Lower case; an IPv6 address without its brackets.
    let host: String
    /// nil for the scheme's default port (80 for http, 443 for https).
    let port: Int?

    init?(_ string: String) {
        let s = string.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let c = URLComponents(string: s), let scheme = c.scheme?.lowercased(), ["http", "https"].contains(scheme),
              var host = c.percentEncodedHost else { return nil }
        if host.hasPrefix("[") && host.hasSuffix("]") { host = String(host.dropFirst().dropLast()) }
        host = host.lowercased()
        // Plain ASCII names and addresses only: the host becomes a folder name.
        guard !host.isEmpty, host.unicodeScalars.allSatisfy({ $0.isASCII && (Self.hostCharacters.contains($0)) }),
              host.contains(where: { $0 != "." }) else { return nil }
        if let p = c.port, !(1...65_535).contains(p) { return nil }
        self.scheme = scheme
        self.host = host
        let defaultPort = scheme == "https" ? 443 : 80
        port = c.port == defaultPort ? nil : c.port
    }

    init?(url: URL) { self.init(url.absoluteString) }

    private static let hostCharacters = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyz0123456789-._:")

    private var isIPv6: Bool { host.contains(":") }

    /// `https://knock.example.com`, `http://127.0.0.1:8787`, `http://[::1]:8787`.
    var origin: String {
        "\(scheme)://\(isIPv6 ? "[\(host)]" : host)\(port.map { ":\($0)" } ?? "")"
    }

    /// The folder under ~/.knock/relays: `https_knock.example.com`, `http_127.0.0.1_8787`, `http_--1_8787`.
    var folder: String {
        "\(scheme)_\(host.replacingOccurrences(of: ":", with: "-"))\(port.map { "_\($0)" } ?? "")"
    }

    /// The login Keychain account holding this relay's sign-in.
    var keychainAccount: String { "session:\(origin)" }

    var description: String { origin }

    /// Whether a stored `relay` value names this relay. A missing value is accepted: the
    /// file sits in this relay's folder (a just-migrated file has none yet).
    func matches(stored: String?) -> Bool {
        guard let stored else { return true }
        return RelayOrigin(stored) == self
    }
}

/// Moving state from older builds, which kept one door, one Recent list and one sign-in
/// for whatever relay was configured, into the current relay's folder.
nonisolated enum LegacyMigration {
    /// The legacy Keychain account from before per-relay sign-ins.
    static let keychainAccount = "session"
    static let files = ["door.json", "recent.json"]

    /// A legacy item moves only when it exists and the current relay has none of its own yet.
    static func shouldMove(legacyExists: Bool, currentExists: Bool) -> Bool { legacyExists && !currentExists }
    /// A legacy item next to the relay's own is deleted, so it can't later move into another
    /// relay's folder carrying this relay's door key, hashes or sign-in.
    static func shouldDelete(legacyExists: Bool, currentExists: Bool) -> Bool { legacyExists && currentExists }

    /// Moves ~/.knock/door.json and recent.json into the relay's folder, stamping door.json
    /// with the relay's origin. Idempotent, and safe if the CLI does the same at once: each
    /// move is one rename that refuses to replace a file already there.
    static func migrateFiles(to relay: RelayOrigin, root: URL = LocalState.root) {
        let fm = FileManager.default
        let dir = LocalState.relayDirectory(relay, root: root)
        for name in files {
            let legacy = root.appendingPathComponent(name), current = dir.appendingPathComponent(name)
            let legacyExists = fm.fileExists(atPath: legacy.path), currentExists = fm.fileExists(atPath: current.path)
            if shouldDelete(legacyExists: legacyExists, currentExists: currentExists) {
                try? fm.removeItem(at: legacy)
                continue
            }
            guard shouldMove(legacyExists: legacyExists, currentExists: currentExists) else { continue }
            LocalState.ensureDirectory(dir, root: root)
            guard renamex_np(legacy.path, current.path, UInt32(RENAME_EXCL)) == 0 else { continue } // the other client won
            try? fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: current.path)
            if name == "door.json", let data = try? Data(contentsOf: current), var door = DoorFile.decode(data, relay: relay),
               door.relay == nil {
                door.relay = relay.origin
                LocalState.write(name, door, in: dir, root: root)
            }
        }
    }
}

/// How long one code lives (`door.code_max_age_days`, docs/LOCAL_STATE.md "Code age").
nonisolated enum CodeAge {
    static let defaultDays = 7
    /// What Settings offers, in days; 0 means never.
    static let choices = [7, 14, 30, 0]

    enum Decision: Equatable, Sendable {
        case keep
        /// Rotate now, exactly like "New code".
        case rotate
        /// Expired, but a session is live: rotate as soon as it ends.
        case afterSession
    }

    /// - createdAt: door.json's `code_created_at`; a file without one starts the clock on load.
    /// - maxAgeDays: 0 (or less) keeps a code until "New code".
    static func decide(createdAt: Date?, maxAgeDays: Int, now: Date, sessionLive: Bool) -> Decision {
        guard maxAgeDays > 0, let createdAt, now.timeIntervalSince(createdAt) >= Double(maxAgeDays) * 86_400 else { return .keep }
        return sessionLive ? .afterSession : .rotate
    }

    /// "a week", "2 weeks", "a month", "3 days".
    static func describe(days: Int) -> String {
        switch days {
        case 1: "a day"
        case 7: "a week"
        case 30, 31: "a month"
        case let d where d % 7 == 0: "\(d / 7) weeks"
        default: "\(days) days"
        }
    }

    /// The Settings label for a choice: "A week", "2 weeks", "A month", "Never".
    static func label(days: Int) -> String {
        if days <= 0 { return "Never" }
        let s = describe(days: days)
        return s.prefix(1).uppercased() + s.dropFirst()
    }

    /// The choices Settings offers, plus a value set elsewhere (config.toml, MDM) so the picker can show it.
    static func choices(including days: Int) -> [Int] {
        let d = max(0, days)
        guard !choices.contains(d) else { return choices }
        return (choices.filter { $0 > 0 } + [d]).sorted() + [0]
    }

    static func notice(days: Int) -> String {
        "Your code was \(describe(days: days)) old, so Knock made a new one. Share it again."
    }
}

extension Date {
    /// Whole seconds, as door.json and recent.json store them, so a value read back compares equal.
    nonisolated var wholeSeconds: Date { Date(timeIntervalSince1970: timeIntervalSince1970.rounded(.down)) }
}
