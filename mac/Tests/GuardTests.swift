import Foundation
import Testing

@Suite struct SessionGateTests {
    let guestId = Identity(id: "u_sam", name: "Sam", email: nil, login: nil, provider: "github", verified: true, avatar: nil)

    func start(role: String = "host", door: String = "bvjy", peer: String = "u_sam", device: String = "sam-mbp",
               version: Int64 = 3) -> SessionStart {
        var p = guestId
        p.id = peer
        return SessionStart(sessionId: "s1", role: role, peer: p, peerDevice: device, doorId: door, codeVersion: version,
                            cols: nil, rows: nil, guestInput: true)
    }

    let allowed = AllowedKnock(knockId: "k1", peerId: "u_sam", peerDevice: "sam-mbp", doorId: "bvjy", codeVersion: 3)

    @Test func hostStartsOnlyForTheKnockLetIn() {
        #expect(SessionGate.decide(start(), allowed: allowed, door: "bvjy", isOpen: true, knockingDoor: nil, busy: false) == .host)
    }

    @Test func unsolicitedHostStartIsRefused() {
        #expect(SessionGate.decide(start(), allowed: nil, door: "bvjy", isOpen: true, knockingDoor: nil, busy: false) == .refuse)
    }

    @Test func hostStartMustMatchEveryDetail() {
        let d = { (s: SessionStart) in
            SessionGate.decide(s, allowed: allowed, door: "bvjy", isOpen: true, knockingDoor: nil, busy: false)
        }
        #expect(d(start(peer: "u_other")) == .refuse)
        #expect(d(start(device: "other-mbp")) == .refuse)
        #expect(d(start(door: "zzzz")) == .refuse)
        #expect(d(start(version: 2)) == .refuse)
        #expect(d(start(role: "admin")) == .refuse)
    }

    @Test func hostStartRefusedWhenClosedOrBusy() {
        #expect(SessionGate.decide(start(), allowed: allowed, door: "bvjy", isOpen: false, knockingDoor: nil, busy: false) == .refuse)
        #expect(SessionGate.decide(start(), allowed: allowed, door: "bvjy", isOpen: true, knockingDoor: nil, busy: true) == .refuse)
        #expect(SessionGate.decide(start(), allowed: allowed, door: nil, isOpen: true, knockingDoor: nil, busy: false) == .refuse)
    }

    @Test func guestStartsOnlyForOurKnock() {
        let g = start(role: "guest", door: "kq7m")
        #expect(SessionGate.decide(g, allowed: nil, door: "bvjy", isOpen: true, knockingDoor: "kq7m", busy: false) == .guest)
        #expect(SessionGate.decide(g, allowed: nil, door: "bvjy", isOpen: true, knockingDoor: nil, busy: false) == .refuse)
        #expect(SessionGate.decide(g, allowed: nil, door: "bvjy", isOpen: true, knockingDoor: "zzzz", busy: false) == .refuse)
        #expect(SessionGate.decide(g, allowed: nil, door: "bvjy", isOpen: true, knockingDoor: "kq7m", busy: true) == .refuse)
    }
}

@Suite struct DoorSyncTests {
    func door(id: String = "bvjy", key: String = "key-0123456789abcdef", code: String = "4821KQ", version: Int64 = 1,
              hash: UInt8 = 1) -> DoorFile {
        DoorFile(doorId: id, doorKey: key, code: code, codeVersion: version, salt: Data(repeating: 7, count: 16),
                 codeHash: Data(repeating: hash, count: 32))
    }

    @Test func higherVersionOnDiskWins() {
        let mem = door(version: 1), disk = door(code: "1696ME", version: 2, hash: 2)
        #expect(DoorSync.reconcile(memory: mem, disk: disk) == disk)
        #expect(DoorSync.cutsOff(from: mem, to: disk))
    }

    @Test func sameVersionDifferentCodeDiskWins() {
        let mem = door(version: 2, hash: 1), disk = door(code: "1696ME", version: 2, hash: 2)
        #expect(DoorSync.reconcile(memory: mem, disk: disk) == disk)
    }

    @Test func olderDiskNeverRollsBack() {
        let mem = door(version: 3, hash: 3), disk = door(version: 2, hash: 2)
        #expect(DoorSync.reconcile(memory: mem, disk: disk) == mem)
    }

    @Test func identicalKeepsMemory() {
        let mem = door()
        #expect(DoorSync.reconcile(memory: mem, disk: mem) == mem)
        #expect(!DoorSync.cutsOff(from: mem, to: mem))
    }

    @Test func otherDoorOnDiskWins() {
        let mem = door(), disk = door(id: "kq7m", key: "other-key-0123456789")
        #expect(DoorSync.reconcile(memory: mem, disk: disk) == disk)
    }

    @Test func missingOrBrokenDiskKeepsMemory() {
        let mem = door()
        #expect(DoorSync.reconcile(memory: mem, disk: nil) == mem)
        var bad = door(version: 9)
        bad.codeHash = Data([1, 2])
        #expect(DoorSync.reconcile(memory: mem, disk: bad) == mem)
        #expect(DoorSync.reconcile(memory: nil, disk: mem) == mem)
    }
}

@Suite struct CodeBoundsTests {
    let salt = Data((0..<16).map { UInt8($0) })

    @Test func refusesIterationsOutsideRange() {
        #expect(Codes.hash(code: "4821KQ", salt: salt, iterations: 1000) == nil)
        #expect(Codes.hash(code: "4821KQ", salt: salt, iterations: 0) == nil)
        #expect(Codes.hash(code: "4821KQ", salt: salt, iterations: -1) == nil)
        #expect(Codes.hash(code: "4821KQ", salt: salt, iterations: 10_000_001) == nil)
        #expect(Codes.hash(code: "4821KQ", salt: salt, iterations: Int(UInt32.max) + 1) == nil) // used to trap
        #expect(Codes.hash(code: "4821KQ", salt: Data(), iterations: 100_000) == nil)
    }

    @Test func acceptsLowerBound() {
        #expect(Codes.hash(code: "4821KQ", salt: salt, iterations: 100_000)?.count == 32)
    }
}

@Suite struct RelayBoundsTests {
    @Test func gridAndExpiry() {
        #expect(RelayBounds.size(cols: 80, rows: 24)! == (80, 24))
        #expect(RelayBounds.size(cols: 0, rows: 24) == nil)
        #expect(RelayBounds.size(cols: 80, rows: 100_000) == nil)
        #expect(RelayBounds.expiry(60) == 60)
        #expect(RelayBounds.expiry(-5) == 1)
        #expect(RelayBounds.expiry(Int.max) == 300)
    }
}

@Suite struct KeyRibbonTests {
    func t(_ s: String) -> [String] { KeyRibbon.tokens(Data(s.utf8)) }

    @Test func plainTypingAndKeys() {
        #expect(t("ls") == ["ls"])
        #expect(t("\r") == ["⏎"])
        #expect(t("\u{03}") == ["⌃C"])
        #expect(t("\u{1b}") == ["⎋"])
        #expect(t("\u{1b}[A") == ["↑"])
        #expect(t("\u{1b}OB") == ["↓"])
    }

    @Test func bracketedPasteKeepsTextAndEnter() {
        #expect(t("\u{1b}[200~rm -rf ~/x; echo hi\u{1b}[201~\r") == ["rm -rf ~/x; echo hi", "⏎"])
    }

    @Test func escapeThenMoreKeysInOneFrame() {
        #expect(t("\u{1b}[Awhoami\r") == ["↑", "whoami", "⏎"])
        #expect(t("\u{1b}b\r") == ["⎋b", "⏎"])
        #expect(t("\u{1b}[3~") == ["⎋[3~"])
        #expect(t("\u{1b}\r") == ["⎋⏎"])
    }

    @Test func punctuationIsShown() {
        #expect(t("\u{1b}[200~a|b>c\u{1b}[201~") == ["a|b>c"])
    }
}

@Suite struct StaleCodeRecoveryTests {
    @Test func goesAboveTheRelaysVersion() {
        #expect(DoorSync.recoveryVersion(relay: 5, local: 2) == 6)
    }

    @Test func neverGoesBelowTheLocalVersion() {
        #expect(DoorSync.recoveryVersion(relay: 1, local: 3) == 4)
    }

    @Test func olderRelayGetsNoRecovery() {
        #expect(DoorSync.recoveryVersion(relay: nil, local: 2) == nil)
    }

    @Test func nonsenseVersionsGetNoRecovery() {
        #expect(DoorSync.recoveryVersion(relay: 0, local: 2) == nil)
        #expect(DoorSync.recoveryVersion(relay: -4, local: 2) == nil)
        #expect(DoorSync.recoveryVersion(relay: Int.max, local: 2) == nil)
    }
}
