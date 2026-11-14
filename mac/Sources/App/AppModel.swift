import AppKit
import Observation
import os
import SwiftUI

/// A live shared shell, from either side.
@Observable
@MainActor
final class ActiveSession: Identifiable {
    let id: String
    let role: String
    let peer: Identity
    let peerDevice: String
    let doorId: String
    let started = Date()
    var guestInput: Bool
    var host: HostTerminal?
    var guest: GuestTerminal?
    var recorder: Recorder?
    var lastPeerKeyAt: Date?
    var ribbon: [String] = []
    var notice: String?

    var isHost: Bool { role == "host" }

    init(start: SessionStart) {
        id = start.sessionId
        role = start.role
        peer = start.peer
        peerDevice = start.peerDevice
        doorId = start.doorId
        guestInput = start.guestInput
    }

    /// Visible names for the keys the guest pressed, including ones the shell does not echo.
    /// Keys usually arrive one per message, so plain typing merges into the
    /// previous chip ("url parrot.live") while it keeps coming; special keys
    /// (⏎ ⌃C ⇥ ⌫ ↑) always get a chip of their own.
    func noteGuestKeys(_ d: Data) {
        let now = Date()
        let continuing = lastPeerKeyAt.map { now.timeIntervalSince($0) < 2 } ?? false
        lastPeerKeyAt = now
        for token in KeyRibbon.tokens(d) {
            if continuing, KeyRibbon.isText(token), let last = ribbon.last, KeyRibbon.isText(last), last.count < 32 {
                ribbon[ribbon.count - 1] = last + token
            } else {
                ribbon.append(token)
            }
        }
        if ribbon.count > 48 { ribbon.removeFirst(ribbon.count - 48) }
    }
}

enum KeyRibbon {
    static let special: Set<String> = ["⏎", "⇥", "⌫", "⎋", "↑", "↓", "→", "←"]

    /// Plain typed text, as opposed to a named key.
    static func isText(_ t: String) -> Bool {
        !special.contains(t) && !t.hasPrefix("⌃") && !t.hasPrefix("⎋")
    }

    static func tokens(_ d: Data) -> [String] {
        let s = String(decoding: d, as: UTF8.self)
        switch s {
        case "\u{1b}[A": return ["↑"]
        case "\u{1b}[B": return ["↓"]
        case "\u{1b}[C": return ["→"]
        case "\u{1b}[D": return ["←"]
        default: break
        }
        if s.hasPrefix("\u{1b}") && s.count > 1 { return ["⎋" + s.dropFirst().filter { $0.isLetter || $0.isNumber }] }
        var out: [String] = []
        for scalar in s.unicodeScalars {
            switch scalar.value {
            case 0x0D, 0x0A: out.append("⏎")
            case 0x09: out.append("⇥")
            case 0x7F, 0x08: out.append("⌫")
            case 0x1B: out.append("⎋")
            case 0x00..<0x20: out.append("⌃" + String(UnicodeScalar(scalar.value + 0x40)!))
            default:
                if let last = out.last, last.count < 24, !["⏎", "⇥", "⌫", "⎋"].contains(last), !last.hasPrefix("⌃") {
                    out[out.count - 1] = last + String(scalar)
                } else {
                    out.append(String(scalar))
                }
            }
        }
        return out
    }
}

/// An outgoing knock in progress.
struct Outgoing: Equatable {
    enum Phase: Equatable { case preparing, waiting(knockId: String), failed(String) }
    var doorId: String
    var name: String
    var codeHash: Data
    var phase: Phase
}

@Observable
@MainActor
final class AppModel {
    static let shared = AppModel()

    private let log = Logger(subsystem: "io.github.idestis.knock", category: "app")
    let relay = RelayClient()
    let auth = AuthService()
    let settings = KnockSettings()

    // Connection
    var connection: RelayClient.State = .idle
    var me: Identity?
    var providers: [String] = []
    /// The relay refuses anonymous use (GET /v1/auth/providers, or the welcome).
    var signInRequired = false
    var authError: String?
    var signingIn = false

    // Door
    var door: DoorFile?
    var isOpen = false
    var doorReady = false
    var rotatingCode = false
    var doorNotice: String?
    var copiedAt: Date?

    // Recent
    var recent: [RecentDoor] = []
    var presence: [String: Presence] = [:]

    // Knocks
    var incoming: [KnockIncoming] = []
    var outgoing: Outgoing?

    // Session
    var session: ActiveSession?

    var openWindow: OpenWindowAction?
    let notifier = Notifier()
    let notch = NotchPanelController()
    let sessionWindow = SessionWindowController()

    var signedIn: Bool { auth.user != nil }
    var connected: Bool { connection == .connected }

    private init() {}

    // MARK: Lifecycle

    func start() {
        recent = RecentFile.load()
        Recorder.prune(root: settings.sessionsDirectory, days: settings.retentionDays)
        isOpen = settings.onboarded && settings.openOnLaunch
        notifier.setUp(model: self)

        relay.onState = { [weak self] s in self?.connectionChanged(s) }
        relay.onWelcome = { [weak self] w in self?.welcomed(w) }
        relay.onEvent = { [weak self] e in self?.handle(e) }
        relay.onData = { [weak self] d in self?.sessionData(d) }
        relay.makeHello = { [weak self] in
            guard let self else { throw CancellationError() }
            return try await self.hello()
        }

        Task {
            if var d = DoorFile.load() {
                if d.doorId.count != Codes.doorIdLength {
                    d.doorId = Codes.newDoorId() // shorter door ids since 0.1: one code, "BVJY 1696 ME"
                    d.save()
                }
                door = d
            } else {
                let d = await Task.detached { DoorFile.create() }.value
                d.save()
                door = d
            }
            doorReady = true
            connectRelay()
        }
    }

    func connectRelay() {
        guard let url = settings.relayURL else {
            connection = .offline("Set a relay address in Settings.")
            return
        }
        relay.start(relay: url)
        Task {
            if let p = await auth.providers(relay: url) {
                providers = p.providers
                signInRequired = p.signInRequired ?? false
            }
        }
    }

    private func hello() async throws -> Hello {
        var token: String?
        if let url = settings.relayURL {
            do { token = try await auth.accessToken(relay: url) } catch {
                authError = error.localizedDescription
                token = nil
            }
        }
        return Hello(token: token, name: token == nil ? settings.displayName : nil, device: AppInfo.deviceName, client: "mac",
                     version: AppInfo.version, protocol: Wire.version)
    }

    private func connectionChanged(_ s: RelayClient.State) {
        connection = s
        if s != .connected, let session {
            finishSession(session, reason: "relay_lost")
        }
    }

    private func welcomed(_ w: Welcome) {
        me = w.user
        if w.user.verified { authError = nil }
        if let p = w.authProviders { providers = p }
        signInRequired = !w.allowsAnonymous
        if isOpen { openDoor() }
        checkRecent()
        #if DEBUG
        // KNOCK_DEMO=1: fixed presence for README screenshots (doors from KNOCK_HOME/recent.json).
        if ProcessInfo.processInfo.environment["KNOCK_DEMO"] != nil {
            DispatchQueue.main.asyncAfter(deadline: .now() + 1) { [weak self] in
                guard let self else { return }
                let states: [Presence] = [.online, .codeChanged, .busy, .offline]
                for (i, r) in self.recent.enumerated() { self.presence[r.doorId] = states[i % states.count] }
            }
        }
        #endif
    }

    func shutdown() {
        if let session { endSession(session, reason: "host_quit") }
        if isOpen { relay.send(MessageType.doorClose) }
        relay.stop()
    }

    // MARK: Account

    func signIn(_ provider: String) {
        guard let url = settings.relayURL else { return }
        signingIn = true
        authError = nil
        Task {
            defer { signingIn = false }
            do {
                _ = try await auth.signIn(provider: provider, relay: url)
                relay.reconnect()
            } catch AuthError.cancelled {
            } catch {
                authError = error.localizedDescription
            }
        }
    }

    /// The unverified name only reaches the relay in hello, so reconnect to send the new one.
    /// Never mid-session: that would end it.
    func displayNameChanged() {
        guard session == nil, me?.verified != true, me?.name != settings.displayName else { return }
        relay.reconnect()
    }

    func signOut() {
        auth.signOut()
        relay.reconnect()
    }

    // MARK: Door

    func setOpen(_ open: Bool) {
        isOpen = open
        if open { openDoor() } else { closeDoor() }
    }

    private func openDoor(retry: Bool = true) {
        guard connected, let door else { return }
        Task {
            do {
                _ = try await relay.request(MessageType.doorOpen,
                                            DoorOpen(doorId: door.doorId, doorKey: door.doorKey, device: AppInfo.deviceName, code: door.record))
                doorNotice = nil
            } catch let e as WireError where e.code == WireError.doorTaken && retry {
                // Someone else holds this id (rare). Start a fresh door.
                let d = await Task.detached { DoorFile.create() }.value
                d.save()
                self.door = d
                openDoor(retry: false)
            } catch {
                doorNotice = (error as? WireError)?.text ?? error.localizedDescription
            }
        }
    }

    private func closeDoor() {
        incoming.removeAll()
        notch.dismiss()
        if let session, session.isHost { endSession(session, reason: "door_closed") }
        guard connected else { return }
        relay.send(MessageType.doorClose)
    }

    func newCode(reason: String? = nil) {
        guard let door, !rotatingCode else { return }
        rotatingCode = true
        Task {
            let d = await Task.detached { door.withNewCode() }.value
            d.save()
            self.door = d
            rotatingCode = false
            doorNotice = reason
            incoming.removeAll()
            notch.dismiss()
            if isOpen && connected {
                do { _ = try await relay.request(MessageType.doorCodeSet, DoorCodeSet(code: d.record)) } catch {
                    openDoor()
                }
            }
        }
    }

    func copyCode() {
        guard let door else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(door.share.text, forType: .string)
        let at = Date()
        copiedAt = at
        Task {
            try? await Task.sleep(for: .seconds(2.5))
            if copiedAt == at { copiedAt = nil }
        }
    }

    // MARK: Recent

    func checkRecent() {
        guard connected, !recent.isEmpty else { return }
        let items = recent.map { RecentItem(doorId: $0.doorId, codeHash: $0.codeHash) }
        Task {
            guard let st = try? await relay.request(MessageType.recentCheck, RecentCheck(items: items), as: RecentStatus.self) else { return }
            for e in st.items { apply(e) }
        }
    }

    private func apply(_ e: RecentEntry) {
        presence[e.doorId] = e.status
        guard let i = recent.firstIndex(where: { $0.doorId == e.doorId }) else { return }
        if e.status == .codeChanged {
            if recent[i].mismatchSince == nil { recent[i].mismatchSince = Date() }
        } else {
            recent[i].mismatchSince = nil
            if let o = e.owner { recent[i].ownerName = o.name; recent[i].ownerEmail = o.email; recent[i].verified = o.verified }
            if let d = e.device { recent[i].device = d }
        }
        RecentFile.save(recent)
    }

    func removeRecent(_ r: RecentDoor) {
        recent.removeAll { $0.doorId == r.doorId }
        presence[r.doorId] = nil
        RecentFile.save(recent)
    }

    private func remember(start: SessionStart, hash: Data) {
        let row = RecentDoor(doorId: start.doorId, ownerName: start.peer.name, ownerEmail: start.peer.email, provider: start.peer.provider,
                             verified: start.peer.verified, device: start.peerDevice, codeVersion: start.codeVersion, codeHash: hash,
                             lastSessionAt: Date(), mismatchSince: nil)
        recent.removeAll { $0.doorId == row.doorId }
        recent.insert(row, at: 0)
        RecentFile.save(recent)
    }

    // MARK: Knocking (helper side)

    /// Knock with a typed share string ("k7f3x9q2 4821KQ").
    func knock(share: ShareString) {
        guard connected else { return }
        outgoing = Outgoing(doorId: share.doorId, name: share.doorId, codeHash: Data(), phase: .preparing)
        Task {
            do {
                let salt = try await relay.request(MessageType.doorSaltGet, DoorSaltGet(doorId: share.doorId), as: DoorSalt.self)
                guard salt.kdf == Codes.kdf else { throw WireError(code: "unsupported", message: "This door uses a newer code format. Update Knock.") }
                let hash = await Task.detached { Codes.hash(code: share.code, salt: salt.salt, iterations: salt.iterations) }.value
                await sendKnock(doorId: share.doorId, name: share.doorId, hash: hash)
            } catch {
                outgoing?.phase = .failed((error as? WireError)?.text ?? error.localizedDescription)
            }
        }
    }

    func knock(recent r: RecentDoor) {
        guard connected else { return }
        outgoing = Outgoing(doorId: r.doorId, name: r.ownerName, codeHash: r.codeHash, phase: .preparing)
        Task { await sendKnock(doorId: r.doorId, name: r.ownerName, hash: r.codeHash) }
    }

    private func sendKnock(doorId: String, name: String, hash: Data) async {
        outgoing = Outgoing(doorId: doorId, name: name, codeHash: hash, phase: .preparing)
        do {
            let p = try await relay.request(MessageType.knockRequest, KnockRequest(doorId: doorId, codeHash: hash), as: KnockPending.self)
            if outgoing?.doorId == doorId { outgoing?.phase = .waiting(knockId: p.knockId) }
        } catch let e as WireError {
            if e.code == WireError.codeMismatch { presence[doorId] = .codeChanged; markMismatch(doorId) }
            if outgoing?.doorId == doorId { outgoing?.phase = .failed(e.text) }
        } catch {
            outgoing?.phase = .failed(error.localizedDescription)
        }
    }

    private func markMismatch(_ doorId: String) {
        guard let i = recent.firstIndex(where: { $0.doorId == doorId }) else { return }
        if recent[i].mismatchSince == nil { recent[i].mismatchSince = Date() }
        RecentFile.save(recent)
    }

    func cancelKnock() {
        if case .waiting = outgoing?.phase { relay.send(MessageType.knockCancel) }
        outgoing = nil
    }

    // MARK: Answering (host side)

    func answer(_ k: KnockIncoming, allow: Bool) {
        incoming.removeAll { $0.knockId == k.knockId }
        notifier.remove(k.knockId)
        if incoming.isEmpty { notch.dismiss() } else { presentKnock(incoming[0]) }
        Task {
            do { _ = try await relay.request(MessageType.knockAnswer, KnockAnswer(knockId: k.knockId, allow: allow)) } catch {
                doorNotice = (error as? WireError)?.text ?? error.localizedDescription
            }
        }
    }

    private func presentKnock(_ k: KnockIncoming) {
        let mainIsFront = NSApp.isActive && (NSApp.keyWindow?.identifier?.rawValue.contains("main") ?? false)
        if mainIsFront {
            notch.dismiss()
        } else if settings.useNotch {
            notch.show(knock: k, model: self)
        }
        if settings.useNotifications && !mainIsFront { notifier.post(k) }
        NSSound(named: "Glass")?.play()
    }

    // MARK: Events

    private func handle(_ e: Envelope) {
        switch e.type {
        case MessageType.knockIncoming:
            guard let k = try? e.decode(KnockIncoming.self), isOpen else { return }
            incoming.append(k)
            if incoming.count == 1 { presentKnock(k) }
            DispatchQueue.main.asyncAfter(deadline: .now() + .seconds(k.expiresIn)) { [weak self] in
                guard let self, self.incoming.contains(k) else { return }
                self.incoming.removeAll { $0 == k }
                self.notifier.remove(k.knockId)
                if self.incoming.isEmpty { self.notch.dismiss() }
            }
        case MessageType.knockCancelled:
            guard let r = try? e.decode(KnockRef.self) else { return }
            incoming.removeAll { $0.knockId == r.knockId }
            notifier.remove(r.knockId)
            if incoming.isEmpty { notch.dismiss() } else { presentKnock(incoming[0]) }
        case MessageType.sessionStart:
            guard let s = try? e.decode(SessionStart.self) else { return }
            beginSession(s)
        case MessageType.sessionEnd:
            let r = try? e.decode(SessionEnd.self)
            if let session, r?.sessionId == nil || r?.sessionId == session.id {
                finishSession(session, reason: r?.reason ?? "ended")
            }
        case MessageType.sessionResize:
            if let r = try? e.decode(SessionResize.self), let g = session?.guest { g.resize(cols: r.cols, rows: r.rows) }
        case MessageType.sessionMode:
            if let m = try? e.decode(SessionMode.self), let session, !session.isHost {
                session.guestInput = m.guestInput
                session.guest?.inputAllowed = m.guestInput
                session.notice = m.guestInput ? "You can type again." : "\(session.peer.firstName) switched on Hands off. You can watch, not type."
            }
        case MessageType.presenceUpdate:
            if let r = try? e.decode(RecentEntry.self) { apply(r) }
        case MessageType.doorCodeChanged:
            if let r = try? e.decode(DoorRef.self) {
                presence[r.doorId] = .codeChanged
                markMismatch(r.doorId)
            }
        case MessageType.doorForceNewCode:
            let r = try? e.decode(DoorForceNewCode.self)
            newCode(reason: (r?.reason ?? "Too many wrong codes.") + " We made you a new code.")
        case MessageType.doorClose:
            isOpen = false
            doorNotice = "Your door was closed from another Knock, like knock close in a terminal. Open it again any time."
        case MessageType.error:
            guard let err = e.error else { return }
            if err.code == WireError.unauthorised {
                // The relay refused a token we thought was good (clock skew,
                // rotated secret, changed sign-in rules). Refresh once; if the
                // relay refuses that too, sign out.
                if auth.markAccessRejected() {
                    relay.reconnect()
                } else {
                    auth.signOut()
                    authError = signInRequired ? "Your sign-in expired. Sign in again to use this relay."
                                               : "Your sign-in expired. You're anonymous until you sign in again."
                    relay.reconnect()
                }
            } else if err.code == WireError.anonymousNotAllowed {
                signInRequired = true
                authError = "This relay needs you to sign in."
            } else if outgoing != nil {
                if err.code == WireError.codeMismatch, let id = outgoing?.doorId { presence[id] = .codeChanged; markMismatch(id) }
                outgoing?.phase = .failed(err.text)
            }
        default:
            break
        }
    }

    // MARK: Sessions

    private func beginSession(_ s: SessionStart) {
        if let old = session { finishSession(old, reason: "replaced") }
        let active = ActiveSession(start: s)
        if s.role == "host" {
            let host = HostTerminal(
                toRelay: { [weak self] d in self?.relay.sendData(d) },
                onResize: { [weak self] c, r in self?.relay.send(MessageType.sessionResize, SessionResize(cols: c, rows: r)) },
                onExit: { [weak self, weak active] in
                    guard let self, let active else { return }
                    self.endSession(active, reason: "shell_exited")
                })
            active.host = host
            active.recorder = Recorder(mode: settings.recording(host: true), root: settings.sessionsDirectory, peer: s.peer,
                                       peerDevice: s.peerDevice, doorId: s.doorId, role: s.role, sessionId: s.sessionId,
                                       cols: host.cols, rows: host.rows)
            host.recorder = active.recorder
            host.start(shellCommand: settings.shellCommand)
            if !settings.guestInputDefault { setGuestInput(false, session: active) }
            incoming.removeAll()
            notch.dismiss()
        } else {
            let guest = GuestTerminal(toRelay: { [weak self] d in self?.relay.sendData(d) })
            guest.onBlockedInput = { [weak active] in active?.notice = "Hands off is on. You can watch, not type." }
            active.guest = guest
            active.recorder = Recorder(mode: settings.recording(host: false), root: settings.sessionsDirectory, peer: s.peer,
                                       peerDevice: s.peerDevice, doorId: s.doorId, role: s.role, sessionId: s.sessionId,
                                       cols: s.cols ?? 80, rows: s.rows ?? 24)
            guest.recorder = active.recorder
            if let o = outgoing, o.doorId == s.doorId { remember(start: s, hash: o.codeHash) }
            outgoing = nil
        }
        session = active
        sessionWindow.show(session: active, model: self)
        if s.role == "host" {
            // Tell the guest our grid once the view has laid out.
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) { [weak self] in
                guard let self, let h = active.host else { return }
                self.relay.send(MessageType.sessionResize, SessionResize(cols: h.cols, rows: h.rows))
            }
        }
        log.info("session started as \(s.role, privacy: .public)")
    }

    private func sessionData(_ d: Data) {
        guard let session else { return }
        if let host = session.host {
            guard session.guestInput else { return }
            session.noteGuestKeys(d)
            host.guestInput(d)
        } else {
            session.guest?.output(d)
        }
    }

    func setGuestInput(_ allowed: Bool, session: ActiveSession) {
        guard session.isHost else { return }
        session.guestInput = allowed
        relay.send(MessageType.sessionMode, SessionMode(guestInput: allowed))
    }

    /// A host can say why (shell_exited, door_closed, host_quit) so the guest isn't told "ended" for everything.
    func endSession(_ s: ActiveSession, reason: String? = nil) {
        let why = s.isHost ? (reason ?? "host_ended") : "guest_left"
        relay.send(MessageType.sessionEnd, SessionEnd(sessionId: s.id, reason: why))
        finishSession(s, reason: why)
    }

    private func finishSession(_ s: ActiveSession, reason: String) {
        guard session === s else { return }
        s.host?.terminate()
        s.recorder?.finish(reason: reason)
        session = nil
        sessionWindow.close(reason: SessionEndText.describe(reason, peer: s.peer, isHost: s.isHost))
        if !s.isHost { checkRecent() }
    }
}

enum SessionEndText {
    static func describe(_ reason: String, peer: Identity, isHost: Bool) -> String {
        switch reason {
        case "host_ended": isHost ? "You ended the session." : "\(peer.firstName) ended the session."
        case "guest_left": isHost ? "\(peer.firstName) left." : "You left \(peer.firstName)'s shell."
        case "shell_exited": isHost ? "The shell exited, so the session ended." : "\(peer.firstName)'s shell exited, so the session ended."
        case "host_quit": isHost ? "You quit Knock, so the session ended." : "\(peer.firstName) quit Knock, so the session ended."
        case "host_left": "\(peer.firstName)'s connection dropped, so the session ended."
        case "code_changed": isHost ? "Your code changed, so the session ended." : "\(peer.firstName) changed their code, so the session ended."
        case "door_closed": isHost ? "You closed your door, so the session ended." : "\(peer.firstName) closed their door."
        case "relay_lost": "Relay restarted, knock again."
        default: "The session ended."
        }
    }
}
