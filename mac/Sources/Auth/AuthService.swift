import AppKit
import AuthenticationServices
import CryptoKit
import Foundation
import os

/// Body of POST /v1/auth/token and /v1/auth/refresh; also the Keychain value.
struct StoredSession: Codable, Sendable {
    var accessToken: String
    var refreshToken: String
    var expiresAt: Int64
    var refreshExpiresAt: Int64
    var user: Identity

    var accessValid: Bool { Date().timeIntervalSince1970 < Double(expiresAt) - 300 }
    var accessUnexpired: Bool { Date().timeIntervalSince1970 < Double(expiresAt) }
    var refreshValid: Bool { Date().timeIntervalSince1970 < Double(refreshExpiresAt) - 60 }
}

enum AuthError: LocalizedError {
    case cancelled, failed(String), expired

    var errorDescription: String? {
        switch self {
        case .cancelled: "Sign-in was cancelled."
        case .failed(let m): m
        case .expired: "Your sign-in expired. Sign in again."
        }
    }
}

/// Optional sign-in with Google or GitHub, brokered by the relay.
/// The refresh token (90 days, sliding) lives in the Keychain, so people
/// stay signed in across launches and days. Each relay has its own sign-in
/// (account `session:<origin>`): one relay's tokens are never sent to another.
@MainActor
final class AuthService: NSObject {
    private let log = Logger(subsystem: "io.github.idestis.knock", category: "auth")
    /// The relay whose sign-in is loaded.
    private(set) var relay: RelayOrigin?
    private(set) var session: StoredSession?
    private var webSession: ASWebAuthenticationSession?

    /// Loads the sign-in for this relay, moving the one older builds kept (account
    /// `session`) to it first if it has none. Other relays' sign-ins stay where they are.
    func use(relay: RelayOrigin?) {
        guard relay != self.relay else { return }
        self.relay = relay
        session = nil
        lastRejected = nil
        guard let relay else { return }
        Self.migrateLegacy(to: relay)
        if let d = Keychain.read(relay.keychainAccount) {
            session = try? Wire.decoder.decode(StoredSession.self, from: d)
            if let s = session, !s.refreshValid { signOut() }
        }
    }

    private static func migrateLegacy(to relay: RelayOrigin) {
        let legacy = LegacyMigration.keychainAccount
        guard let d = Keychain.read(legacy),
              LegacyMigration.shouldMove(legacyExists: true, currentExists: Keychain.read(relay.keychainAccount) != nil) else { return }
        if Keychain.save(relay.keychainAccount, d) { Keychain.delete(legacy) }
    }

    var user: Identity? { session?.user }

    func signOut() {
        session = nil
        if let relay { Keychain.delete(relay.keychainAccount) }
    }

    /// A valid access token for `relay`, refreshing when needed. nil means anonymous,
    /// including when `relay` isn't the one whose sign-in is loaded.
    func accessToken(relay url: URL) async throws -> String? {
        guard let origin = RelayOrigin(url: url), origin == relay, let s = session else { return nil }
        if s.accessValid { return s.accessToken }
        guard s.refreshValid else {
            signOut()
            throw AuthError.expired
        }
        do {
            let fresh: StoredSession = try await post(url.appending(path: "v1/auth/refresh"), ["refresh_token": s.refreshToken])
            store(fresh, for: origin)
            return fresh.accessToken
        } catch AuthError.expired {
            if origin == relay { signOut() } else { Keychain.delete(origin.keychainAccount) }
            throw AuthError.expired
        } catch {
            // Relay unreachable: keep using a token that hasn't run out yet.
            if s.accessUnexpired { return s.accessToken }
            throw error
        }
    }

    private var lastRejected: Date?

    /// The relay refused our access token at hello. Returns true when a
    /// refresh is worth trying (the next hello will do it); false when we
    /// just tried that, so the caller should sign out instead of looping.
    func markAccessRejected() -> Bool {
        guard var s = session, s.refreshValid else { return false }
        if let last = lastRejected, Date().timeIntervalSince(last) < 60 { return false }
        lastRejected = Date()
        s.expiresAt = 0
        session = s  // memory only: the Keychain copy is replaced by the refresh
        return true
    }

    func providers(relay: URL) async -> AuthProviders? {
        guard let (d, resp) = try? await URLSession.shared.data(from: relay.appending(path: "v1/auth/providers")),
              (resp as? HTTPURLResponse)?.statusCode == 200 else { return nil }
        return try? Wire.decoder.decode(AuthProviders.self, from: d)
    }

    func signIn(provider: String, relay: URL) async throws -> Identity {
        guard let origin = RelayOrigin(url: relay) else { throw AuthError.failed("Set a relay address in Settings.") }
        let verifier = Codes.base64URL(Codes.randomBytes(32))
        let challenge = Codes.base64URL(Data(SHA256.hash(data: Data(verifier.utf8))))
        let state = Codes.base64URL(Codes.randomBytes(16))
        var c = URLComponents(url: relay.appending(path: "v1/auth/\(provider)/start"), resolvingAgainstBaseURL: false)!
        c.queryItems = [.init(name: "redirect_uri", value: "knock://auth/callback"), .init(name: "state", value: state),
                        .init(name: "code_challenge", value: challenge), .init(name: "code_challenge_method", value: "S256")]

        let callback: URL = try await withCheckedThrowingContinuation { cont in
            let ws = ASWebAuthenticationSession(url: c.url!, callback: .customScheme("knock")) { url, error in
                if let url { cont.resume(returning: url) } else {
                    let cancelled = (error as? ASWebAuthenticationSessionError)?.code == .canceledLogin
                    cont.resume(throwing: cancelled ? AuthError.cancelled : AuthError.failed(error?.localizedDescription ?? "Sign-in failed."))
                }
            }
            ws.presentationContextProvider = self
            ws.prefersEphemeralWebBrowserSession = false
            webSession = ws
            ws.start()
        }
        webSession = nil
        let items = URLComponents(url: callback, resolvingAgainstBaseURL: false)?.queryItems ?? []
        func q(_ n: String) -> String? { items.first { $0.name == n }?.value }
        guard q("state") == state else { throw AuthError.failed("Sign-in response did not match. Try again.") }
        if let e = q("error") { throw AuthError.failed(q("error_description") ?? e) }
        guard let code = q("code") else { throw AuthError.failed("No sign-in code came back.") }

        let s: StoredSession
        do {
            s = try await post(relay.appending(path: "v1/auth/token"), ["code": code, "code_verifier": verifier])
        } catch AuthError.expired {
            throw AuthError.failed("The relay didn't accept the sign-in. Try again.")
        }
        store(s, for: origin)
        log.info("signed in with \(provider, privacy: .public)")
        return s.user
    }

    /// Saves under the relay that issued it; memory only follows if that's still the current relay.
    private func store(_ s: StoredSession, for origin: RelayOrigin) {
        if origin == relay { session = s }
        if let d = try? Wire.encoder.encode(s) { Keychain.save(origin.keychainAccount, d) }
    }

    private func post<T: Decodable>(_ url: URL, _ body: [String: String]) async throws -> T {
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONSerialization.data(withJSONObject: body)
        let (d, resp) = try await URLSession.shared.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        if code == 401 || code == 400 { throw AuthError.expired }
        guard code == 200 else { throw AuthError.failed("The relay answered \(code).") }
        return try Wire.decoder.decode(T.self, from: d)
    }
}

extension AuthService: ASWebAuthenticationPresentationContextProviding {
    nonisolated func presentationAnchor(for session: ASWebAuthenticationSession) -> ASPresentationAnchor {
        MainActor.assumeIsolated { NSApp.keyWindow ?? NSApp.windows.first { $0.isVisible } ?? NSWindow() }
    }
}
