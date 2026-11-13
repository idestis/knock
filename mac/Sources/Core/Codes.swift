import CommonCrypto
import Foundation
import Security

/// Codes, salts and hashes. Must match protocol/code.go byte for byte.
nonisolated enum Codes {
    static let kdf = "pbkdf2-sha256"
    static let iterations = 600_000
    static let hashLength = 32
    static let saltLength = 16
    static let doorIdLength = 4

    static let digits = Array("0123456789")
    static let letters = Array("ABCDEFGHJKLMNPQRSTUVWXYZ") // no I, O
    static let doorAlphabet = Array("abcdefghjkmnpqrstuvwxyz23456789")

    /// "4821KQ"
    static func newCode() -> String {
        String((0..<4).map { _ in digits.randomElement()! } + (0..<2).map { _ in letters.randomElement()! })
    }

    static func newDoorId() -> String {
        String((0..<doorIdLength).map { _ in doorAlphabet.randomElement()! })
    }

    static func randomBytes(_ n: Int) -> Data {
        var b = Data(count: n)
        _ = b.withUnsafeMutableBytes { SecRandomCopyBytes(kSecRandomDefault, n, $0.baseAddress!) }
        return b
    }

    static func newSalt() -> Data { randomBytes(saltLength) }

    static func newDoorKey() -> String { base64URL(randomBytes(32)) }

    /// Upper case, no spaces or dashes.
    static func normalise(_ code: String) -> String {
        code.filter { !$0.isWhitespace && $0 != "-" }.uppercased()
    }

    /// "4821 KQ"
    static func display(_ code: String) -> String {
        let c = normalise(code)
        guard c.count == 6 else { return c }
        return String(c.prefix(4)) + " " + String(c.suffix(2))
    }

    static func isValidCode(_ code: String) -> Bool {
        let c = Array(normalise(code))
        return c.count == 6 && c[0..<4].allSatisfy(\.isNumber) && c[4..<6].allSatisfy { letters.contains($0) }
    }

    /// PBKDF2-HMAC-SHA256. Slow on purpose (~0.3 s); never call on the main actor.
    static func hash(code: String, salt: Data, iterations: Int = iterations) -> Data {
        let password = Array(normalise(code).utf8).map { Int8(bitPattern: $0) }
        var out = [UInt8](repeating: 0, count: hashLength)
        salt.withUnsafeBytes { saltPtr in
            _ = CCKeyDerivationPBKDF(CCPBKDFAlgorithm(kCCPBKDF2), password, password.count,
                                     saltPtr.bindMemory(to: UInt8.self).baseAddress, salt.count,
                                     CCPseudoRandomAlgorithm(kCCPRFHmacAlgSHA256), UInt32(iterations),
                                     &out, hashLength)
        }
        return Data(out)
    }

    static func base64URL(_ d: Data) -> String {
        d.base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }
}

/// The whole code people share: door + code, shown and copied as "BVJY 1696 ME".
nonisolated struct ShareString: Equatable {
    var doorId: String
    var code: String

    /// "BVJY 1696 ME"
    var text: String { "\(doorId.uppercased()) \(Codes.display(code))" }

    /// Accepts "BVJY 1696 ME", "bvjy-1696me", "knock bvjy 1696ME", "bvjy1696ME" and older
    /// 8-character doors ("k7f3x9q2 4821KQ"). The last six characters are the code.
    static func parse(_ input: String) -> ShareString? {
        var s = input.trimmingCharacters(in: .whitespacesAndNewlines)
        if s.lowercased().hasPrefix("knock ") { s = String(s.dropFirst(6)) }
        let flat = Array(s.filter { !$0.isWhitespace && $0 != "-" && $0 != "·" })
        guard flat.count >= Codes.doorIdLength + 6, flat.count <= 32 + 6 else { return nil }
        let door = String(flat.dropLast(6)).lowercased()
        let code = String(flat.suffix(6)).uppercased()
        guard door.allSatisfy({ Codes.doorAlphabet.contains($0) }), Codes.isValidCode(code) else { return nil }
        return ShareString(doorId: door, code: code)
    }
}
