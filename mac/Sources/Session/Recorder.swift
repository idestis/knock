import Foundation

/// Writes ~/.knock/sessions/<ts>_<peer>_<id>/{meta.json,session.cast,keys.jsonl}.
/// session.cast is asciicast v2, replayable with `knock sessions play` or asciinema.
final class Recorder {
    enum Mode: String, CaseIterable, Identifiable, Sendable {
        case off, output
        case outputAndInput = "output+input"
        var id: String { rawValue }
        var label: String {
            switch self {
            case .off: "Off"
            case .output: "Terminal output"
            case .outputAndInput: "Output and guest keystrokes"
            }
        }
    }

    let directory: URL
    private let cast: FileHandle
    private var keys: FileHandle?
    private let started = Date()
    private var carry = Data()
    private var meta: [String: Any]

    init?(mode: Mode, root: URL, peer: Identity, peerDevice: String, doorId: String, role: String, sessionId: String,
          cols: Int, rows: Int) {
        guard mode != .off else { return nil }
        let fm = FileManager.default
        let ts = ISO8601DateFormatter().string(from: Date()).replacingOccurrences(of: ":", with: "-")
        let slug = peer.name.lowercased().split(whereSeparator: { !$0.isLetter && !$0.isNumber }).joined(separator: "-")
        directory = root.appendingPathComponent("\(ts)_\(slug.isEmpty ? "guest" : slug)_\(sessionId.suffix(4))")
        do {
            try fm.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            try fm.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        } catch { return nil }
        let castURL = directory.appendingPathComponent("session.cast")
        fm.createFile(atPath: castURL.path, contents: nil, attributes: [.posixPermissions: 0o600])
        guard let h = try? FileHandle(forWritingTo: castURL) else { return nil }
        cast = h
        if mode == .outputAndInput {
            let k = directory.appendingPathComponent("keys.jsonl")
            fm.createFile(atPath: k.path, contents: nil, attributes: [.posixPermissions: 0o600])
            keys = try? FileHandle(forWritingTo: k)
        }
        meta = ["session_id": sessionId, "role": role, "door_id": doorId, "peer": peer.name, "peer_email": peer.email ?? "",
                "peer_verified": peer.verified, "peer_device": peerDevice, "start": ISO8601DateFormatter().string(from: started),
                "client": "mac \(AppInfo.version)", "recording": mode.rawValue]
        let header: [String: Any] = ["version": 2, "width": cols, "height": rows, "timestamp": Int(started.timeIntervalSince1970),
                                     "env": ["TERM": "xterm-256color"], "title": "Knock: \(peer.name)"]
        write(cast, header)
        writeMeta()
    }

    private var elapsed: Double { (Date().timeIntervalSince(started) * 1_000_000).rounded() / 1_000_000 }

    func output(_ bytes: some Collection<UInt8>) {
        carry.append(contentsOf: bytes)
        // Keep an incomplete UTF-8 sequence for the next chunk.
        var cut = carry.count
        var back = 0
        while back < 3 && cut - back - 1 >= 0 {
            let b = carry[carry.startIndex + cut - back - 1]
            if b & 0xC0 == 0x80 { back += 1; continue }
            if b & 0x80 != 0 {
                let need = b & 0xE0 == 0xC0 ? 2 : b & 0xF0 == 0xE0 ? 3 : 4
                if back + 1 < need { cut -= back + 1 }
            }
            break
        }
        let chunk = carry.prefix(cut)
        carry = Data(carry.suffix(from: carry.startIndex + cut))
        guard !chunk.isEmpty else { return }
        writeArray(cast, [elapsed, "o", String(decoding: chunk, as: UTF8.self)])
    }

    func resize(cols: Int, rows: Int) {
        writeArray(cast, [elapsed, "r", "\(cols)x\(rows)"])
    }

    func input(_ bytes: some Collection<UInt8>, source: String) {
        guard let keys else { return }
        write(keys, ["t": elapsed, "source": source, "data": String(decoding: Array(bytes), as: UTF8.self)])
    }

    func finish(reason: String) {
        meta["end"] = ISO8601DateFormatter().string(from: Date())
        meta["end_reason"] = reason
        writeMeta()
        try? cast.close()
        try? keys?.close()
    }

    private func writeMeta() {
        guard let d = try? JSONSerialization.data(withJSONObject: meta, options: [.prettyPrinted, .sortedKeys]) else { return }
        let url = directory.appendingPathComponent("meta.json")
        FileManager.default.createFile(atPath: url.path, contents: d, attributes: [.posixPermissions: 0o600])
    }

    private func write(_ h: FileHandle, _ obj: [String: Any]) {
        guard var d = try? JSONSerialization.data(withJSONObject: obj, options: [.sortedKeys]) else { return }
        d.append(0x0A)
        try? h.write(contentsOf: d)
    }

    private func writeArray(_ h: FileHandle, _ arr: [Any]) {
        guard var d = try? JSONSerialization.data(withJSONObject: arr) else { return }
        d.append(0x0A)
        try? h.write(contentsOf: d)
    }

    /// Deletes recordings older than `days`.
    static func prune(root: URL, days: Int) {
        let cutoff = Date().addingTimeInterval(-Double(days) * 86_400)
        let fm = FileManager.default
        guard let items = try? fm.contentsOfDirectory(at: root, includingPropertiesForKeys: [.creationDateKey]) else { return }
        for url in items {
            let created = (try? url.resourceValues(forKeys: [.creationDateKey]))?.creationDate ?? Date()
            if created < cutoff { try? fm.removeItem(at: url) }
        }
    }
}
