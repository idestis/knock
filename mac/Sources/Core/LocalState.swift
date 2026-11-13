import Foundation

/// Files under ~/.knock shared with the CLI. See docs/LOCAL_STATE.md.
nonisolated enum LocalState {
    static var root: URL {
        if let override = ProcessInfo.processInfo.environment["KNOCK_HOME"] { return URL(fileURLWithPath: override) }
        return FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".knock")
    }

    static func ensureRoot() {
        let fm = FileManager.default
        try? fm.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try? fm.setAttributes([.posixPermissions: 0o700], ofItemAtPath: root.path)
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

    static func read<T: Decodable>(_ name: String, as: T.Type) -> T? {
        guard let d = try? Data(contentsOf: root.appendingPathComponent(name)) else { return nil }
        return try? decoder.decode(T.self, from: d)
    }

    static func write(_ name: String, _ value: some Encodable) {
        ensureRoot()
        guard let d = try? encoder.encode(value) else { return }
        let url = root.appendingPathComponent(name)
        let tmp = root.appendingPathComponent(".\(name).tmp")
        FileManager.default.createFile(atPath: tmp.path, contents: d, attributes: [.posixPermissions: 0o600])
        _ = try? FileManager.default.replaceItemAt(url, withItemAt: tmp)
        if !FileManager.default.fileExists(atPath: url.path) { try? FileManager.default.moveItem(at: tmp, to: url) }
        try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }
}

/// This device's door. The code is kept so the app and CLI show the same one.
nonisolated struct DoorFile: Codable, Equatable, Sendable {
    var doorId: String
    var doorKey: String
    var code: String
    var codeVersion: Int64
    var salt: Data
    var codeHash: Data

    var record: CodeRecord {
        CodeRecord(salt: salt, codeHash: codeHash, codeVersion: codeVersion, kdf: Codes.kdf, iterations: Codes.iterations)
    }

    var share: ShareString { ShareString(doorId: doorId, code: code) }

    static func load() -> DoorFile? { LocalState.read("door.json", as: DoorFile.self) }
    func save() { LocalState.write("door.json", self) }

    /// Slow (PBKDF2): call off the main actor.
    static func create() -> DoorFile {
        let code = Codes.newCode(), salt = Codes.newSalt()
        return DoorFile(doorId: Codes.newDoorId(), doorKey: Codes.newDoorKey(), code: code, codeVersion: 1,
                        salt: salt, codeHash: Codes.hash(code: code, salt: salt))
    }

    /// Slow (PBKDF2): call off the main actor.
    func withNewCode() -> DoorFile {
        var d = self
        d.code = Codes.newCode()
        d.salt = Codes.newSalt()
        d.codeHash = Codes.hash(code: d.code, salt: d.salt)
        d.codeVersion += 1
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

    static func load() -> [RecentDoor] {
        let items = LocalState.read("recent.json", as: RecentFile.self)?.items ?? []
        // Rows that have mismatched for 30 days are pruned.
        let cutoff = Date().addingTimeInterval(-30 * 86_400)
        return items.filter { ($0.mismatchSince ?? .distantFuture) > cutoff }
    }

    static func save(_ items: [RecentDoor]) { LocalState.write("recent.json", RecentFile(items: items)) }
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
