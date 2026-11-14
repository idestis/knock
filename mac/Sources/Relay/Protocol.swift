// Swift copy of protocol/protocol.go. Change both together.
// JSON uses snake_case; property names avoid acronyms so the
// convertTo/FromSnakeCase strategies round-trip (doorId <-> door_id).
import Foundation

nonisolated enum Wire {
    static let version = 1

    static let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.keyEncodingStrategy = .convertToSnakeCase
        return e
    }()

    static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        return d
    }()
}

nonisolated struct WireError: Codable, Error, Equatable, Sendable {
    var code: String
    var message: String?
    /// Set with code_stale: the code version the relay holds.
    var codeVersion: Int?

    static let codeMismatch = "code_mismatch"
    static let doorOffline = "door_offline"
    static let doorBusy = "door_busy"
    static let doorTaken = "door_taken"
    static let codeStale = "code_stale"
    static let rateLimited = "rate_limited"
    static let denied = "denied"
    static let timeout = "timeout"
    static let unauthorised = "unauthorised"
    static let anonymousNotAllowed = "anonymous_not_allowed"

    var text: String { message?.isEmpty == false ? message! : code.replacingOccurrences(of: "_", with: " ").capitalized }
}

/// Raw envelope as it arrives; `data` is decoded lazily per type.
nonisolated struct Envelope: Sendable {
    var type: String
    var id: String?
    var data: Data?
    var error: WireError?

    func decode<T: Decodable>(_: T.Type) throws -> T {
        try Wire.decoder.decode(T.self, from: data ?? Data("{}".utf8))
    }

    static func parse(_ raw: Data) throws -> Envelope {
        guard let obj = try JSONSerialization.jsonObject(with: raw) as? [String: Any],
              let type = obj["type"] as? String else { throw WireError(code: "bad_request", message: "malformed") }
        var env = Envelope(type: type, id: obj["id"] as? String)
        if let d = obj["data"], !(d is NSNull) {
            env.data = try JSONSerialization.data(withJSONObject: d, options: [.fragmentsAllowed])
        }
        if let e = obj["error"] as? [String: Any], let code = e["code"] as? String {
            env.error = WireError(code: code, message: e["message"] as? String,
                                  codeVersion: (e["code_version"] as? NSNumber)?.intValue)
        }
        return env
    }

    static func encode(type: String, id: String?, data: (some Encodable)?) throws -> String {
        var obj: [String: Any] = ["type": type]
        if let id { obj["id"] = id }
        if let data {
            let d = try Wire.encoder.encode(data)
            obj["data"] = try JSONSerialization.jsonObject(with: d, options: [.fragmentsAllowed])
        }
        return String(decoding: try JSONSerialization.data(withJSONObject: obj), as: UTF8.self)
    }
}

nonisolated enum MessageType {
    static let hello = "hello", welcome = "welcome", error = "error", ok = "ok"
    static let doorOpen = "door.open", doorOpened = "door.opened", doorClose = "door.close"
    static let doorCodeSet = "door.code.set", doorSaltGet = "door.salt.get", doorSalt = "door.salt"
    static let doorCodeChanged = "door.code_changed", doorForceNewCode = "door.force_new_code"
    static let recentCheck = "recent.check", recentStatus = "recent.status", presenceUpdate = "presence.update"
    static let knockRequest = "knock.request", knockPending = "knock.pending", knockIncoming = "knock.incoming"
    static let knockAnswer = "knock.answer", knockCancel = "knock.cancel", knockCancelled = "knock.cancelled"
    static let sessionStart = "session.start", sessionEnd = "session.end"
    static let sessionResize = "session.resize", sessionMode = "session.mode"
    static let relayDraining = "relay.draining"
}

nonisolated struct Empty: Codable, Sendable {}

nonisolated struct Hello: Codable, Sendable {
    var token: String?
    var name: String?
    var device: String
    var client: String
    var version: String
    var `protocol`: Int
}

nonisolated struct Identity: Codable, Sendable, Hashable {
    var id: String
    var name: String
    var email: String?
    var login: String?  // GitHub username
    var provider: String
    var verified: Bool
    var avatar: String?

    var providerLabel: String {
        switch provider {
        case "github": "GitHub"
        case "google": "Google"
        default: provider
        }
    }

    /// The verified email, or @login for GitHub without one. nil when anonymous:
    /// nothing an anonymous knocker typed should read as checked.
    var verifiedDetail: String? {
        guard verified else { return nil }
        if let email, !email.isEmpty { return email }
        if let login, !login.isEmpty { return "@" + login }
        return nil
    }

    /// "sam@acme.dev · GitHub", or just "GitHub". nil when anonymous.
    var verifiedLine: String? {
        guard verified else { return nil }
        return [verifiedDetail, providerLabel].compactMap { $0 }.joined(separator: " · ")
    }

    var initials: String {
        let parts = name.split(whereSeparator: { $0 == " " || $0 == "." || $0 == "-" || $0 == "_" })
        let letters = parts.prefix(2).compactMap(\.first)
        return letters.isEmpty ? "?" : String(letters).uppercased()
    }

    var firstName: String { name.split(separator: " ").first.map(String.init) ?? name }
}

/// GET /v1/auth/providers.
nonisolated struct AuthProviders: Codable, Sendable, Equatable {
    var providers: [String]
    var signInRequired: Bool?
}

nonisolated struct Welcome: Codable, Sendable {
    var user: Identity
    var relay: String
    var `protocol`: Int
    var knockTimeoutSeconds: Int
    var allowsAnonymous: Bool
    var authProviders: [String]?
}

nonisolated struct CodeRecord: Codable, Sendable, Equatable {
    var salt: Data
    var codeHash: Data
    var codeVersion: Int64
    var kdf: String
    var iterations: Int
}

nonisolated struct DoorOpen: Codable, Sendable {
    var doorId: String
    var doorKey: String
    var device: String
    var code: CodeRecord
    /// Relay-signed proof of ownership from an earlier door.opened.
    var claim: String?
}

nonisolated struct DoorOpened: Codable, Sendable {
    var doorId: String
    var claim: String?
}
nonisolated struct DoorCodeSet: Codable, Sendable { var code: CodeRecord }
nonisolated struct DoorSaltGet: Codable, Sendable { var doorId: String }
nonisolated struct DoorSalt: Codable, Sendable {
    var doorId: String
    var salt: Data
    var kdf: String
    var iterations: Int
}
nonisolated struct DoorRef: Codable, Sendable { var doorId: String }
nonisolated struct DoorForceNewCode: Codable, Sendable { var reason: String }

nonisolated enum Presence: String, Codable, Sendable {
    case online, busy, offline
    case codeChanged = "code_changed"
}

nonisolated struct RecentItem: Codable, Sendable {
    var doorId: String
    var codeHash: Data
}
nonisolated struct RecentCheck: Codable, Sendable { var items: [RecentItem] }
nonisolated struct RecentEntry: Codable, Sendable {
    var doorId: String
    var status: Presence
    var owner: Identity?
    var device: String?
    var codeVersion: Int64?
}
nonisolated struct RecentStatus: Codable, Sendable { var items: [RecentEntry] }

nonisolated struct KnockRequest: Codable, Sendable {
    var doorId: String
    var codeHash: Data
}
nonisolated struct KnockPending: Codable, Sendable {
    var knockId: String
    var expiresIn: Int
}
nonisolated struct KnockIncoming: Codable, Sendable, Identifiable, Equatable {
    var knockId: String
    var from: Identity
    var device: String
    var expiresIn: Int
    var id: String { knockId }
}
nonisolated struct KnockAnswer: Codable, Sendable {
    var knockId: String
    var allow: Bool
}
nonisolated struct KnockRef: Codable, Sendable {
    var knockId: String
    var reason: String?
}

nonisolated struct SessionStart: Codable, Sendable {
    var sessionId: String
    var role: String
    var peer: Identity
    var peerDevice: String
    var doorId: String
    var codeVersion: Int64
    var cols: Int?
    var rows: Int?
    var guestInput: Bool
}
nonisolated struct SessionEnd: Codable, Sendable {
    var sessionId: String?
    var reason: String?
}
nonisolated struct SessionResize: Codable, Sendable {
    var cols: Int
    var rows: Int
}
nonisolated struct SessionMode: Codable, Sendable { var guestInput: Bool }
