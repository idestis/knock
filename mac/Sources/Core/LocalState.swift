import Foundation

/// Files under ~/.knock shared with the CLI. See docs/LOCAL_STATE.md.
nonisolated enum LocalState {
    static var root: URL {
        if let override = ProcessInfo.processInfo.environment["KNOCK_HOME"] { return URL(fileURLWithPath: override) }
        return FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".knock")
    }

    static func ensureRoot() { ensureDirectory(root, root: root) }

    /// ~/.knock/relays/<relay>: the door, Recent list and anything else that belongs to one relay.
    static func relayDirectory(_ relay: RelayOrigin, root: URL = LocalState.root) -> URL {
        root.appendingPathComponent("relays", isDirectory: true).appendingPathComponent(relay.folder, isDirectory: true)
    }

    /// Creates `dir` and every folder between it and `root` with mode 0700.
    static func ensureDirectory(_ dir: URL, root: URL = LocalState.root) {
        let fm = FileManager.default
        try? fm.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        var u = dir.standardizedFileURL
        let top = root.standardizedFileURL.path
        while u.path.hasPrefix(top) {
            try? fm.setAttributes([.posixPermissions: 0o700], ofItemAtPath: u.path)
            if u.path == top { break }
            u = u.deletingLastPathComponent()
        }
    }

    static let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.keyEncodingStrategy = .convertToSnakeCase
        e.dateEncodingStrategy = .iso8601
        e.outputFormatting = [.prettyPrinted, .sortedKeys]
        return e
    }()

    static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        d.dateDecodingStrategy = .iso8601
        return d
    }()

    static func read<T: Decodable>(_ name: String, as: T.Type, in dir: URL? = nil) -> T? {
        guard let d = try? Data(contentsOf: (dir ?? root).appendingPathComponent(name)) else { return nil }
        return try? decoder.decode(T.self, from: d)
    }

    static func write(_ name: String, _ value: some Encodable, in dir: URL? = nil, root: URL = LocalState.root) {
        let dir = dir ?? root
        ensureDirectory(dir, root: root)
        guard let d = try? encoder.encode(value) else { return }
        let url = dir.appendingPathComponent(name)
        let tmp = dir.appendingPathComponent(".\(name).tmp")
        FileManager.default.createFile(atPath: tmp.path, contents: d, attributes: [.posixPermissions: 0o600])
        _ = try? FileManager.default.replaceItemAt(url, withItemAt: tmp)
        if !FileManager.default.fileExists(atPath: url.path) { try? FileManager.default.moveItem(at: tmp, to: url) }
        try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }
}

/// This device's door on one relay. The code is kept so the app and CLI show the same one.
nonisolated struct DoorFile: Codable, Equatable, Sendable {
    var doorId: String
    var doorKey: String
    var code: String
    var codeVersion: Int64
    var salt: Data
    var codeHash: Data
    /// The relay's origin. A file naming another relay is ignored.
    var relay: String? = nil
    /// When this code was made; a file without one starts the clock on load.
    var codeCreatedAt: Date? = nil
    /// The relay's proof that this key owns the door id, from door.opened. Sent with every door.open.
    var claim: String? = nil

    var record: CodeRecord {
        CodeRecord(salt: salt, codeHash: codeHash, codeVersion: codeVersion, kdf: Codes.kdf, iterations: Codes.iterations)
    }

    var share: ShareString { ShareString(doorId: doorId, code: code) }

    /// nil for a file that doesn't decode or names another relay.
    static func decode(_ data: Data, relay: RelayOrigin) -> DoorFile? {
        guard let d = try? LocalState.decoder.decode(DoorFile.self, from: data), relay.matches(stored: d.relay) else { return nil }
        return d
    }

    static func load(relay: RelayOrigin, root: URL = LocalState.root) -> DoorFile? {
        let url = LocalState.relayDirectory(relay, root: root).appendingPathComponent("door.json")
        guard let data = try? Data(contentsOf: url) else { return nil }
        return decode(data, relay: relay)
    }

    /// When door.json last changed, so a watcher only rereads it after a write.
    static func modificationDate(relay: RelayOrigin) -> Date? {
        let path = LocalState.relayDirectory(relay).appendingPathComponent("door.json").path
        return (try? FileManager.default.attributesOfItem(atPath: path))?[.modificationDate] as? Date
    }

    /// Enough shape to send to the relay. A torn or hand-edited file fails this and is ignored.
    var isWellFormed: Bool {
        (4...32).contains(doorId.count) && doorId.allSatisfy { Codes.doorAlphabet.contains($0) } && doorKey.count >= 16
            && Codes.isValidCode(code) && salt.count == Codes.saltLength && codeHash.count == Codes.hashLength && codeVersion > 0
    }

    /// Writes door.json in the relay's folder, stamped with its origin.
    func save(relay: RelayOrigin) {
        var d = self
        d.relay = relay.origin
        LocalState.write("door.json", d, in: LocalState.relayDirectory(relay))
    }

    /// Slow (PBKDF2): call off the main actor.
    static func create(relay: RelayOrigin, now: Date = Date()) -> DoorFile {
        let code = Codes.newCode(), salt = Codes.newSalt()
        return DoorFile(doorId: Codes.newDoorId(), doorKey: Codes.newDoorKey(), code: code, codeVersion: 1,
                        salt: salt, codeHash: Codes.ownHash(code: code, salt: salt), relay: relay.origin,
                        codeCreatedAt: now.wholeSeconds)
    }

    /// Slow (PBKDF2): call off the main actor. The door id, key and claim stay.
    func withNewCode(now: Date = Date()) -> DoorFile {
        var d = self
        d.code = Codes.newCode()
        d.salt = Codes.newSalt()
        d.codeHash = Codes.ownHash(code: d.code, salt: d.salt)
        d.codeVersion += 1
        d.codeCreatedAt = now.wholeSeconds
        return d
    }
}

nonisolated struct RecentDoor: Codable, Equatable, Identifiable, Sendable {
    var doorId: String
    var ownerName: String
    var ownerEmail: String?
    var provider: String?
    var verified: Bool?
    var device: String
    var codeVersion: Int64
    var codeHash: Data
    var lastSessionAt: Date
    var mismatchSince: Date?

    var id: String { doorId }
}

nonisolated struct RecentFile: Codable, Sendable {
    var items: [RecentDoor]

    /// Doors joined through this relay only: their hashes are never sent to another one.
    static func load(relay: RelayOrigin) -> [RecentDoor] {
        let items = LocalState.read("recent.json", as: RecentFile.self, in: LocalState.relayDirectory(relay))?.items ?? []
        // Rows that have mismatched for 30 days are pruned.
        let cutoff = Date().addingTimeInterval(-30 * 86_400)
        return items.filter { ($0.mismatchSince ?? .distantFuture) > cutoff }
    }

    static func save(_ items: [RecentDoor], relay: RelayOrigin) {
        LocalState.write("recent.json", RecentFile(items: items), in: LocalState.relayDirectory(relay))
    }
}

/// Minimal reader for ~/.knock/config.toml: `[table]` headers and `key = value` lines.
nonisolated enum ConfigFile {
    static func load() -> [String: String] {
        guard let text = try? String(contentsOf: LocalState.root.appendingPathComponent("config.toml"), encoding: .utf8) else {
            return [:]
        }
        return parse(text)
    }

    static func parse(_ text: String) -> [String: String] {
        var out: [String: String] = [:]
        var table = ""
        for raw in text.split(whereSeparator: \.isNewline) {
            // Drop a comment that starts outside quotes.
            var inQuotes = false
            var line = ""
            for ch in raw {
                if ch == "\"" { inQuotes.toggle() }
                if ch == "#" && !inQuotes { break }
                line.append(ch)
            }
            line = line.trimmingCharacters(in: .whitespaces)
            if line.isEmpty { continue }
            if line.hasPrefix("[") && line.hasSuffix("]") {
                table = String(line.dropFirst().dropLast()).trimmingCharacters(in: .whitespaces)
                continue
            }
            guard let eq = line.firstIndex(of: "=") else { continue }
            let key = line[..<eq].trimmingCharacters(in: .whitespaces)
            var value = line[line.index(after: eq)...].trimmingCharacters(in: .whitespaces)
            if value.hasPrefix("\"") && value.hasSuffix("\"") && value.count >= 2 { value = String(value.dropFirst().dropLast()) }
            out[table.isEmpty ? key : "\(table).\(key)"] = value
        }
        return out
    }
}
