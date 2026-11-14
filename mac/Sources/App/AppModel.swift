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
    /// The relay says another key holds this door id (after a relay restart, say).
    /// Shared codes no longer reach us; the host decides whether to make a new door.
    var doorTaken = false
    /// door.json's modification date when we last read it (the CLI writes it too).
    private var doorMod: Date?

    /// The relay whose door, Recent list and sign-in are loaded. Each relay has its own:
    /// one relay's door key, hashes and tokens are never sent to another
    /// (docs/LOCAL_STATE.md, "Per-relay state").
    private(set) var relayOrigin: RelayOrigin?
    /// Bumped on every relay switch, so work started for the old relay never lands in the new one.
    private var relayGeneration = 0

    // Recent
    var recent: [RecentDoor] = []
    var presence: [String: Presence] = [:]

    // Knocks
    var incoming: [KnockIncoming] = []
    var outgoing: Outgoing?
    /// The knock the host just let in; only its session.start may open a shell.
    private var allowedKnock: AllowedKnock?

    // Session
    var session: ActiveSession?

    /// Whether macOS lets the shared shell into Documents, Desktop, iCloud and the rest.
    /// Rechecked whenever Knock comes back to the front, so it follows System Settings.
    var fullDiskAccess = FullDiskAccess().isGranted

    var openWindow: OpenWindowAction?
    let notifier = Notifier()
    let notch = NotchPanelController()
    let sessionWindow = SessionWindowController()

    var signedIn: Bool { auth.user != nil }
    var connected: Bool { connection == .connected }

    private init() {}

    // MARK: Lifecycle

    func start() {
        Recorder.prune(root: settings.sessionsDirectory, days: settings.retentionDays)
        isOpen = settings.onboarded && settings.openOnLaunch
        notifier.setUp(model: self)
        NotificationCenter.default.addObserver(forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { AppModel.shared.refreshFullDiskAccess() }
        }

        relay.onState = { [weak self] s in self?.connectionChanged(s) }
        relay.onWelcome = { [weak self] w in self?.welcomed(w) }
        relay.onEvent = { [weak self] e in self?.handle(e) }
        relay.onData = { [weak self] d in self?.sessionData(d) }
        relay.makeHello = { [weak self] in
            guard let self else { throw CancellationError() }
            return try await self.hello()
        }

        loadRelayState()
        watchDoorFile()
    }

    /// Loads the door, Recent list and sign-in that belong to the configured relay (moving
    /// an older build's single set there first), then connects.
    private func loadRelayState() {
        relayGeneration += 1
        let gen = relayGeneration
        let origin = settings.relayURL.flatMap { RelayOrigin(url: $0) }
        relayOrigin = origin
        auth.use(relay: origin)
        door = nil
        doorReady = false
        doorMod = nil
        doorTaken = false
        doorNotice = nil
        recent = []
        presence = [:]
        me = nil
        providers = []
        signInRequired = false
        authError = nil
        guard let origin else {
            connectRelay()
            return
        }
        LegacyMigration.migrateFiles(to: origin)
        recent = RecentFile.load(relay: origin)
        Task {
            var d: DoorFile
            if let disk = DoorFile.load(relay: origin) {
                d = disk
                if d.doorId.count != Codes.doorIdLength {
                    d.doorId = Codes.newDoorId() // shorter door ids since 0.1: one code, "BVJY 1696 ME"
                    d.claim = nil
                }
            } else {
                d = await Task.detached { DoorFile.create(relay: origin) }.value
            }
            guard gen == relayGeneration else { return }
            Self.prepare(&d, for: origin)
            if d != DoorFile.load(relay: origin) { saveDoor(d) }
            door = d
            doorMod = DoorFile.modificationDate(relay: origin)
            doorReady = true
            connectRelay()
        }
    }

    /// Stamps the relay's origin, and starts the code-age clock now for a door.json
    /// without `code_created_at`.
    private static func prepare(_ d: inout DoorFile, for origin: RelayOrigin) {
        d.relay = origin.origin
        if d.codeCreatedAt == nil { d.codeCreatedAt = Date().wholeSeconds }
    }

    /// Writes door.json for the current relay and remembers its modification date.
    private func saveDoor(_ d: DoorFile) {
        guard let relayOrigin else { return }
        d.save(relay: relayOrigin)
        doorMod = DoorFile.modificationDate(relay: relayOrigin)
    }

    private func saveRecent() {
        guard let relayOrigin else { return }
        RecentFile.save(recent, relay: relayOrigin)
    }

    /// Settings or onboarding changed the relay address. Another relay means another door,
    /// Recent list and sign-in: the door closes on the old relay first, and nothing from
    /// it is sent to the new one.
    func changeRelay(to address: String) {
        let next = RelayOrigin(address)
        guard next == nil || next != relayOrigin else {
            settings.relayURLString = address
            relay.stop()
            connectRelay()
            return
        }
        relayGeneration += 1
        doorReady = false // no code-age rotation for the relay we're leaving
        if let session { endSession(session, reason: session.isHost ? "door_closed" : nil) }
        cutOffEveryone(reason: "door_closed")
        if case .waiting = outgoing?.phase { relay.send(MessageType.knockCancel) }
        outgoing = nil
        let wasOpen = isOpen && connected
        Task {
            if wasOpen { _ = try? await relay.request(MessageType.doorClose, timeout: 3) }
            relay.stop()
            settings.relayURLString = address
            loadRelayState()
        }
    }

    /// Picks up `knock new-code` (or a new door) written by the CLI while the app runs,
    /// like the CLI's checkDoorFile. Cheap: a stat every 2 s, a read only after a write.
    private func watchDoorFile() {
        Task { [weak self] in
            while true {
                try? await Task.sleep(for: .seconds(2))
                guard let self else { return }
                self.checkDoorFile()
            }
        }
    }

    private func checkDoorFile() {
        guard doorReady, !rotatingCode, let relayOrigin else { return }
        let mod = DoorFile.modificationDate(relay: relayOrigin)
        if mod != doorMod {
            doorMod = mod
            syncDoorFromDisk(push: true)
        }
        checkCodeAge()
    }

    /// Code age (docs/LOCAL_STATE.md): while the door is open and idle, a code older than
    /// `door.code_max_age_days` is replaced like "New code". Never during a session or while
    /// a knock waits for an answer; finishSession calls this again once it's over.
    private func checkCodeAge() {
        guard doorReady, isOpen, !rotatingCode, let door, incoming.isEmpty, allowedKnock == nil else { return }
        let days = settings.codeMaxAgeDays
        if CodeAge.decide(createdAt: door.codeCreatedAt, maxAgeDays: days, now: Date(), sessionLive: session != nil) == .rotate {
            rotateForAge(days: days, reopen: false)
        }
    }

    private func rotateForAge(days: Int, reopen: Bool) {
        let text = CodeAge.notice(days: days)
        newCode(reason: text, reopen: reopen)
        if settings.useNotifications { notifier.postNotice(id: "code-age", title: "You have a new code", body: text) }
    }

    private static let staleNotice = "Your relay holds a newer code for this door. Update the relay, or make a new door."
    private static let recoveredNotice = "Your door had a newer code on the relay than this Mac knew, so Knock made a new one. Share it again."

    /// After code_stale survived a reread of door.json: when the relay says which version it
    /// holds, make a fresh code above it and try once more. Otherwise, or after that, say so.
    /// - reopen: the refusal came from door.open rather than door.code.set.
    private func recoverStaleCode(_ e: WireError, recovered: Bool, reopen: Bool) {
        guard !recovered, let local = door?.codeVersion,
              let version = DoorSync.recoveryVersion(relay: e.codeVersion, local: local) else {
            doorNotice = Self.staleNotice
            return
        }
        newCode(reason: Self.recoveredNotice, reopen: reopen, minVersion: version)
    }

    /// Reconciles the app's door with door.json. When the code or door changed, everyone
    /// with the old code is cut off here too (rule 4); with `push` and the door open, the
    /// relay is told so it ends the live session and pending knocks.
    private func syncDoorFromDisk(push: Bool) {
        guard let current = door, let relayOrigin else { return }
        let disk = DoorFile.load(relay: relayOrigin)
        guard var next = DoorSync.reconcile(memory: current, disk: disk) else { return }
        Self.prepare(&next, for: relayOrigin)
        if next == current {
            // Ours is newer, or the file is gone, torn or for another relay: write it back so the CLI can't roll the relay back.
            if disk != current { saveDoor(current) }
            return
        }
        if next != disk { saveDoor(next) }
        door = next
        guard DoorSync.cutsOff(from: current, to: next) else { return }
        if next.doorId != current.doorId { doorTaken = false }
        cutOffEveryone(reason: "code_changed")
        doorNotice = "Your code changed in another Knock, like knock new-code in a terminal. Anyone with the old code is cut off."
        guard push, isOpen, connected else { return }
        if next.doorId == current.doorId && next.doorKey == current.doorKey { pushCode() } else { openDoor() }
    }

    /// Tells the relay about the current code. code_stale means the relay holds a newer one
    /// than ours, so door.json is reread and the push tried once more; if that fails too, a
    /// fresh code above the relay's version is made and pushed once (`recovered`).
    private func pushCode(notice: String? = nil, retried: Bool = false, recovered: Bool = false) {
        guard let door, isOpen, connected else { return }
        let gen = relayGeneration
        Task {
            do {
                _ = try await relay.request(MessageType.doorCodeSet, DoorCodeSet(code: door.record))
            } catch let e as WireError where e.code == WireError.codeStale {
                guard gen == relayGeneration else { return }
                if retried {
                    recoverStaleCode(e, recovered: recovered, reopen: false)
                } else {
                    syncDoorFromDisk(push: false)
                    pushCode(notice: notice, retried: true, recovered: recovered)
                }
            } catch {
                guard gen == relayGeneration else { return }
                openDoor(notice: notice, recovered: recovered)
            }
        }
    }

    /// A new code or door: the live host session, pending knocks and their prompts all end.
    private func cutOffEveryone(reason: String) {
        allowedKnock = nil
        incoming.removeAll()
        notifier.removeAll()
        notch.dismiss()
        if let session, session.isHost { endSession(session, reason: reason) }
    }

    func refreshFullDiskAccess() {
        let granted = FullDiskAccess().isGranted
        if granted != fullDiskAccess { fullDiskAccess = granted }
    }

    func connectRelay() {
        guard let url = settings.relayURL else {
            connection = .offline("Set a relay address in Settings.")
            return
        }
        // Only ever connect with the state loaded for this relay.
        guard let origin = RelayOrigin(url: url) else {
            connection = .offline("That relay address doesn't look right. Check it in Settings.")
            return
        }
        guard origin == relayOrigin else {
            loadRelayState()
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
        if s != .connected { allowedKnock = nil }
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

    /// - notice: what to show once the door is open (nil clears an old one).
    /// - retried: the second try after code_stale.
    /// - recovered: the try with a fresh code made above the relay's version; no more after it.
    private func openDoor(notice: String? = nil, retried: Bool = false, recovered: Bool = false) {
        guard connected, !rotatingCode else { return }
        // door.json may hold a newer code from the CLI: never re-register a revoked one.
        syncDoorFromDisk(push: false)
        guard let door else { return }
        // Code age: an expired code is replaced before it's offered again.
        let days = settings.codeMaxAgeDays
        if CodeAge.decide(createdAt: door.codeCreatedAt, maxAgeDays: days, now: Date(), sessionLive: session != nil) == .rotate {
            rotateForAge(days: days, reopen: true)
            return
        }
        let gen = relayGeneration
        Task {
            do {
                let env = try await relay.request(MessageType.doorOpen,
                                                  DoorOpen(doorId: door.doorId, doorKey: door.doorKey, device: AppInfo.deviceName,
                                                           code: door.record, claim: door.claim))
                guard gen == relayGeneration else { return }
                if self.door?.doorId == door.doorId { doorTaken = false }
                doorNotice = notice
                if let claim = (try? env.decode(DoorOpened.self))?.claim, !claim.isEmpty {
                    keepClaim(claim, doorId: door.doorId, doorKey: door.doorKey)
                }
            } catch let e as WireError where e.code == WireError.codeStale {
                guard gen == relayGeneration else { return }
                // door.json may hold the newer code (the CLI rotated): openDoor rereads it.
                if retried {
                    recoverStaleCode(e, recovered: recovered, reopen: true)
                } else {
                    openDoor(notice: notice, retried: true, recovered: recovered)
                }
            } catch let e as WireError where e.code == WireError.doorTaken {
                guard gen == relayGeneration else { return }
                // Don't quietly swap in a new door: every code already shared would stop working
                // without a word. Say so, and let the host choose to make a new one.
                doorTaken = true
                isOpen = false
                cutOffEveryone(reason: "door_closed")
                doorNotice = "Someone else holds your door id on this relay, so codes you shared won't reach you. Make a new door to take knocks again."
            } catch {
                guard gen == relayGeneration else { return }
                doorNotice = (error as? WireError)?.text ?? error.localizedDescription
            }
        }
    }

    /// Stores the relay's claim for this door, so the key holder can take the door id back
    /// after a relay restart. Written into door.json's own record, so a code the CLI saved
    /// meanwhile isn't rolled back; the watcher picks that up as usual.
    private func keepClaim(_ claim: String, doorId: String, doorKey: String) {
        guard let relayOrigin, var d = door, d.doorId == doorId, d.doorKey == doorKey, d.claim != claim else { return }
        d.claim = claim
        door = d
        if var disk = DoorFile.load(relay: relayOrigin) {
            guard disk.doorId == doorId, disk.doorKey == doorKey else { return }
            disk.claim = claim
            disk.save(relay: relayOrigin)
        } else {
            d.save(relay: relayOrigin)
        }
    }

    /// The host's explicit answer to door_taken: a new door id, key and code. Old codes stop working.
    func makeNewDoor() {
        guard !rotatingCode, let origin = relayOrigin else { return }
        rotatingCode = true
        let gen = relayGeneration
        Task {
            let d = await Task.detached { DoorFile.create(relay: origin) }.value
            rotatingCode = false
            guard gen == relayGeneration else { return }
            saveDoor(d)
            door = d
            doorTaken = false
            doorNotice = nil
            cutOffEveryone(reason: "code_changed")
            isOpen = true
            openDoor()
        }
    }

    private func closeDoor() {
        cutOffEveryone(reason: "door_closed")
        guard connected else { return }
        relay.send(MessageType.doorClose)
    }

    /// - reopen: called from openDoor (code age), so open the door with the new code once
    ///   it's made instead of pushing it to a door the relay doesn't have yet.
    /// - minVersion: the lowest version the new code may have (code_stale recovery, see
    ///   recoverStaleCode). Set only there, so the open or push that follows is the last try.
    func newCode(reason: String? = nil, reopen: Bool = false, minVersion: Int64? = nil) {
        guard door != nil, !rotatingCode, let origin = relayOrigin else { return }
        // Rotate from the freshest record, so a CLI rotation is neither undone nor collided with.
        syncDoorFromDisk(push: false)
        guard let door else { return }
        rotatingCode = true
        cutOffEveryone(reason: "code_changed")
        let gen = relayGeneration
        Task {
            var d = await Task.detached { door.withNewCode() }.value
            rotatingCode = false
            guard gen == relayGeneration else { return }
            if let minVersion, d.codeVersion < minVersion { d.codeVersion = minVersion }
            // The CLI may have rotated while we hashed: stay ahead of it so versions never collide.
            if let disk = DoorFile.load(relay: origin), disk.doorId == d.doorId, disk.codeVersion >= d.codeVersion {
                d.codeVersion = disk.codeVersion + 1
                d.claim = d.claim ?? disk.claim
            }
            saveDoor(d)
            self.door = d
            doorNotice = reason
            cutOffEveryone(reason: "code_changed")
            let recovered = minVersion != nil
            if reopen {
                openDoor(notice: reason, retried: recovered, recovered: recovered)
            } else {
                pushCode(notice: reason, retried: recovered, recovered: recovered)
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
        saveRecent()
    }

    func removeRecent(_ r: RecentDoor) {
        recent.removeAll { $0.doorId == r.doorId }
        presence[r.doorId] = nil
        saveRecent()
    }

    private func remember(start: SessionStart, hash: Data) {
        let row = RecentDoor(doorId: start.doorId, ownerName: start.peer.name, ownerEmail: start.peer.email, provider: start.peer.provider,
                             verified: start.peer.verified, device: start.peerDevice, codeVersion: start.codeVersion, codeHash: hash,
                             lastSessionAt: Date(), mismatchSince: nil)
        recent.removeAll { $0.doorId == row.doorId }
        recent.insert(row, at: 0)
        saveRecent()
    }

    // MARK: Knocking (helper side)

    /// Knock with a typed share string ("k7f3x9q2 4821KQ").
    func knock(share: ShareString) {
        guard connected, session == nil else { return } // knocking mid-session would replace it
        outgoing = Outgoing(doorId: share.doorId, name: share.doorId, codeHash: Data(), phase: .preparing)
        Task {
            do {
                let salt = try await relay.request(MessageType.doorSaltGet, DoorSaltGet(doorId: share.doorId), as: DoorSalt.self)
                guard salt.kdf == Codes.kdf else { throw WireError(code: "unsupported", message: "This door uses a newer code format. Update Knock.") }
                let hash = await Task.detached { Codes.hash(code: share.code, salt: salt.salt, iterations: salt.iterations) }.value
                guard let hash else { throw WireError(code: "unsupported", message: "This door's code settings look wrong. Ask for a new code.") }
                await sendKnock(doorId: share.doorId, name: share.doorId, hash: hash)
            } catch {
                outgoing?.phase = .failed((error as? WireError)?.text ?? error.localizedDescription)
            }
        }
    }

    func knock(recent r: RecentDoor) {
        guard connected, session == nil else { return }
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
        saveRecent()
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
        // Record the "Let in" before it goes out: the relay sends session.start right after its ok.
        if allow, isOpen, session == nil, let door {
            allowedKnock = AllowedKnock(knockId: k.knockId, peerId: k.from.id, peerDevice: k.device,
                                        doorId: door.doorId, codeVersion: door.codeVersion)
        } else if allow || allowedKnock?.knockId == k.knockId {
            // Closed or already in a session: the relay will refuse, and no session.start may follow.
            allowedKnock = nil
        }
        Task {
            do { _ = try await relay.request(MessageType.knockAnswer, KnockAnswer(knockId: k.knockId, allow: allow)) } catch {
                if allowedKnock?.knockId == k.knockId { allowedKnock = nil }
                doorNotice = (error as? WireError)?.text ?? error.localizedDescription
            }
        }
    }

    /// "Only signed-in guests": answer no for the host, without a prompt or a sound.
    /// This path can only decline; letting someone in always takes the host's click.
    private func turnAway(_ k: KnockIncoming) {
        doorNotice = "Turned away \(k.from.name), who isn't signed in. You only take knocks from signed-in guests (Settings > Security)."
        Task {
            _ = try? await relay.request(MessageType.knockAnswer, KnockAnswer(knockId: k.knockId, allow: false))
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
            if KnockScreen.decide(from: k.from, verifiedOnly: settings.verifiedGuestsOnly) == .declineUnverified {
                turnAway(k)
                return
            }
            incoming.append(k)
            if incoming.count == 1 { presentKnock(k) }
            DispatchQueue.main.asyncAfter(deadline: .now() + .seconds(RelayBounds.expiry(k.expiresIn))) { [weak self] in
                guard let self else { return }
                if self.allowedKnock?.knockId == k.knockId, self.session == nil { self.allowedKnock = nil }
                guard self.incoming.contains(k) else { return }
                self.incoming.removeAll { $0 == k }
                self.notifier.remove(k.knockId)
                if self.incoming.isEmpty { self.notch.dismiss() }
            }
        case MessageType.knockCancelled:
            guard let r = try? e.decode(KnockRef.self) else { return }
            if allowedKnock?.knockId == r.knockId { allowedKnock = nil }
            incoming.removeAll { $0.knockId == r.knockId }
            notifier.remove(r.knockId)
            if incoming.isEmpty { notch.dismiss() } else { presentKnock(incoming[0]) }
        case MessageType.sessionStart:
            guard let s = try? e.decode(SessionStart.self) else { return }
            var knockingDoor: String?
            if let o = outgoing, case .waiting = o.phase { knockingDoor = o.doorId }
            let decision = SessionGate.decide(s, allowed: allowedKnock, door: door?.doorId, isOpen: isOpen,
                                              knockingDoor: knockingDoor, busy: session != nil)
            if decision == .host { allowedKnock = nil }
            guard decision != .refuse else {
                // Nobody here asked for this session: a host shell needs the host's "Let in" (rule 1).
                log.error("refused a session.start nobody asked for (role \(s.role, privacy: .public))")
                relay.send(MessageType.sessionEnd, SessionEnd(sessionId: s.sessionId, reason: s.role == "host" ? "host_ended" : "guest_left"))
                return
            }
            beginSession(s)
        case MessageType.sessionEnd:
            let r = try? e.decode(SessionEnd.self)
            if let session, r?.sessionId == nil || r?.sessionId == session.id {
                finishSession(session, reason: r?.reason ?? "ended")
            }
        case MessageType.sessionResize:
            if let r = try? e.decode(SessionResize.self), let g = session?.guest,
               let size = RelayBounds.size(cols: r.cols, rows: r.rows) {
                g.resize(cols: size.cols, rows: size.rows)
            }
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
            let why = (try? e.decode(SessionEnd.self))?.reason
            isOpen = false
            cutOffEveryone(reason: "door_closed")
            if why == "door_reclaimed" {
                doorNotice = "The owner of this door id took it back on the relay, so it's closed here. If that wasn't you, make a new door."
            } else {
                doorNotice = "Your door was closed from another Knock, like knock close in a terminal. Open it again any time."
            }
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
        // Tell the relay the old one is over before it's dropped here, so neither side keeps a half-open session.
        if let old = session { endSession(old, reason: "replaced") }
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
            notifier.removeAll()
            notch.dismiss()
        } else {
            let guest = GuestTerminal(toRelay: { [weak self] d in self?.relay.sendData(d) })
            guest.onBlockedInput = { [weak active] in active?.notice = "Hands off is on. You can watch, not type." }
            active.guest = guest
            let grid = RelayBounds.size(cols: s.cols ?? 80, rows: s.rows ?? 24) ?? (cols: 80, rows: 24)
            active.recorder = Recorder(mode: settings.recording(host: false), root: settings.sessionsDirectory, peer: s.peer,
                                       peerDevice: s.peerDevice, doorId: s.doorId, role: s.role, sessionId: s.sessionId,
                                       cols: grid.cols, rows: grid.rows)
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
        // A code that expired during the session is replaced now.
        checkCodeAge()
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
