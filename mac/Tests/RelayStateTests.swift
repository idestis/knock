import Foundation
import Testing

/// Must match the Go CLI exactly (docs/LOCAL_STATE.md, "Per-relay state").
@Suite struct RelayOriginTests {
    @Test(arguments: [
        ("https://Knock.Example.com/", "https://knock.example.com", "https_knock.example.com"),
        ("https://knock.example.com:443", "https://knock.example.com", "https_knock.example.com"),
        ("http://127.0.0.1:8787/path", "http://127.0.0.1:8787", "http_127.0.0.1_8787"),
        ("http://[::1]:8787", "http://[::1]:8787", "http_--1_8787"),
        ("http://localhost", "http://localhost", "http_localhost"),
    ])
    func normalises(input: String, origin: String, folder: String) throws {
        let r = try #require(RelayOrigin(input))
        #expect(r.origin == origin)
        #expect(r.folder == folder)
        #expect(r.keychainAccount == "session:\(origin)")
    }

    @Test func defaultPortsAndCase() {
        #expect(RelayOrigin("HTTP://LocalHost:80")?.origin == "http://localhost")
        #expect(RelayOrigin("http://knock.example.com:443")?.origin == "http://knock.example.com:443")
        #expect(RelayOrigin("https://knock.example.com:80")?.folder == "https_knock.example.com_80")
        #expect(RelayOrigin(" https://knock.example.com/v1?x=1 ")?.origin == "https://knock.example.com")
    }

    @Test func refusesWhatIsNotARelay() {
        #expect(RelayOrigin("") == nil)
        #expect(RelayOrigin("knock.example.com") == nil)
        #expect(RelayOrigin("ftp://knock.example.com") == nil)
        #expect(RelayOrigin("https://") == nil)
        #expect(RelayOrigin("http://../") == nil)
        #expect(RelayOrigin("http://a%2Fb") == nil)
    }

    @Test func storedRelayMatching() throws {
        let r = try #require(RelayOrigin("https://knock.example.com"))
        #expect(r.matches(stored: nil))
        #expect(r.matches(stored: "https://knock.example.com"))
        #expect(r.matches(stored: "https://Knock.example.com:443/"))
        #expect(!r.matches(stored: "https://evil.example.com"))
        #expect(!r.matches(stored: "http://knock.example.com"))
        #expect(!r.matches(stored: "garbage"))
    }
}

@Suite struct DoorFileRelayTests {
    let relay = RelayOrigin("https://knock.example.com")!

    func door(relay: String?) -> DoorFile {
        DoorFile(doorId: "bvjy", doorKey: "key-0123456789abcdef", code: "4821KQ", codeVersion: 1,
                 salt: Data(repeating: 7, count: 16), codeHash: Data(repeating: 1, count: 32), relay: relay,
                 codeCreatedAt: Date(timeIntervalSince1970: 1_800_000_000), claim: "c1")
    }

    @Test func otherRelaysFileIsIgnored() throws {
        let data = try LocalState.encoder.encode(door(relay: "https://evil.example.com"))
        #expect(DoorFile.decode(data, relay: relay) == nil)
    }

    @Test func ownRelayOrUnstampedFileLoads() throws {
        let own = door(relay: "https://knock.example.com")
        #expect(DoorFile.decode(try LocalState.encoder.encode(own), relay: relay) == own)
        let bare = door(relay: nil)
        #expect(DoorFile.decode(try LocalState.encoder.encode(bare), relay: relay) == bare)
    }

    @Test func wireShapeMatchesTheCLI() throws {
        let obj = try JSONSerialization.jsonObject(with: try LocalState.encoder.encode(door(relay: relay.origin))) as? [String: Any]
        #expect(obj?["relay"] as? String == "https://knock.example.com")
        #expect(obj?["code_created_at"] as? String == "2027-01-15T08:00:00Z")
        #expect(obj?["claim"] as? String == "c1")
        // Written by the CLI: whole seconds, no claim, no clock.
        let cli = """
        {"relay":"https://knock.example.com","door_id":"bvjy","door_key":"key-0123456789abcdef","code":"4821KQ","code_version":2,
         "salt":"BwcHBwcHBwcHBwcHBwcHBw==","code_hash":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=","code_created_at":null}
        """
        let d = try #require(DoorFile.decode(Data(cli.utf8), relay: relay))
        #expect(d.codeCreatedAt == nil && d.claim == nil && d.isWellFormed)
    }

    @Test func reconcileKeepsClaimAndClock() {
        var mem = door(relay: relay.origin)
        mem.claim = nil
        var disk = door(relay: relay.origin)
        disk.codeCreatedAt = nil
        let out = DoorSync.reconcile(memory: mem, disk: disk)
        #expect(out?.claim == "c1")
        #expect(out?.codeCreatedAt == mem.codeCreatedAt)
    }
}

@Suite struct LegacyMigrationTests {
    let relay = RelayOrigin("http://127.0.0.1:8787")!

    @Test func decision() {
        #expect(LegacyMigration.shouldMove(legacyExists: true, currentExists: false))
        #expect(!LegacyMigration.shouldMove(legacyExists: true, currentExists: true))
        #expect(!LegacyMigration.shouldMove(legacyExists: false, currentExists: false))
        #expect(!LegacyMigration.shouldMove(legacyExists: false, currentExists: true))
    }

    func tempRoot() throws -> URL {
        let u = FileManager.default.temporaryDirectory.appendingPathComponent("knock-test-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
        return u
    }

    let legacyDoor = """
    {"door_id":"bvjy","door_key":"key-0123456789abcdef","code":"4821KQ","code_version":3,
     "salt":"BwcHBwcHBwcHBwcHBwcHBw==","code_hash":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="}
    """

    @Test func movesLegacyFilesOnceAndStampsTheRelay() throws {
        let root = try tempRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        try Data(legacyDoor.utf8).write(to: root.appendingPathComponent("door.json"))
        try Data(#"{"items":[]}"#.utf8).write(to: root.appendingPathComponent("recent.json"))

        LegacyMigration.migrateFiles(to: relay, root: root)
        LegacyMigration.migrateFiles(to: relay, root: root) // idempotent

        let dir = LocalState.relayDirectory(relay, root: root)
        #expect(dir.path.hasSuffix("relays/http_127.0.0.1_8787"))
        #expect(!FileManager.default.fileExists(atPath: root.appendingPathComponent("door.json").path))
        #expect(!FileManager.default.fileExists(atPath: root.appendingPathComponent("recent.json").path))
        #expect(FileManager.default.fileExists(atPath: dir.appendingPathComponent("recent.json").path))
        let d = try #require(DoorFile.load(relay: relay, root: root))
        #expect(d.relay == "http://127.0.0.1:8787" && d.codeVersion == 3)
        let mode = try FileManager.default.attributesOfItem(atPath: dir.path)[.posixPermissions] as? Int
        #expect(mode == 0o700)
    }

    @Test func neverReplacesTheRelaysOwnFile() throws {
        let root = try tempRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let dir = LocalState.relayDirectory(relay, root: root)
        LocalState.ensureDirectory(dir, root: root)
        let own = #"{"relay":"http://127.0.0.1:8787","door_id":"kq7m","door_key":"other-key-0123456789","code":"1696ME","code_version":1,"salt":"BwcHBwcHBwcHBwcHBwcHBw==","code_hash":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="}"#
        try Data(own.utf8).write(to: dir.appendingPathComponent("door.json"))
        try Data(legacyDoor.utf8).write(to: root.appendingPathComponent("door.json"))

        LegacyMigration.migrateFiles(to: relay, root: root)

        #expect(DoorFile.load(relay: relay, root: root)?.doorId == "kq7m")
        #expect(FileManager.default.fileExists(atPath: root.appendingPathComponent("door.json").path))
    }

    @Test func anotherRelayDoesNotSeeThisOnesDoor() throws {
        let root = try tempRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        try Data(legacyDoor.utf8).write(to: root.appendingPathComponent("door.json"))
        LegacyMigration.migrateFiles(to: relay, root: root)
        let other = RelayOrigin("https://knock.example.com")!
        #expect(DoorFile.load(relay: other, root: root) == nil)
        // A file copied into the wrong folder is still refused.
        let wrong = LocalState.relayDirectory(other, root: root)
        LocalState.ensureDirectory(wrong, root: root)
        try FileManager.default.copyItem(at: LocalState.relayDirectory(relay, root: root).appendingPathComponent("door.json"),
                                         to: wrong.appendingPathComponent("door.json"))
        #expect(DoorFile.load(relay: other, root: root) == nil)
    }
}

@Suite struct CodeAgeTests {
    let made = Date(timeIntervalSince1970: 1_800_000_000)
    func days(_ n: Double) -> Date { made.addingTimeInterval(n * 86_400) }

    @Test func rotatesOnceOld() {
        #expect(CodeAge.decide(createdAt: made, maxAgeDays: 7, now: days(6.9), sessionLive: false) == .keep)
        #expect(CodeAge.decide(createdAt: made, maxAgeDays: 7, now: days(7), sessionLive: false) == .rotate)
        #expect(CodeAge.decide(createdAt: made, maxAgeDays: 30, now: days(29), sessionLive: false) == .keep)
    }

    @Test func waitsForTheSessionToEnd() {
        #expect(CodeAge.decide(createdAt: made, maxAgeDays: 7, now: days(8), sessionLive: true) == .afterSession)
        #expect(CodeAge.decide(createdAt: made, maxAgeDays: 7, now: days(1), sessionLive: true) == .keep)
    }

    @Test func zeroMeansNever() {
        #expect(CodeAge.decide(createdAt: made, maxAgeDays: 0, now: days(3650), sessionLive: false) == .keep)
        #expect(CodeAge.decide(createdAt: made, maxAgeDays: -1, now: days(3650), sessionLive: false) == .keep)
    }

    @Test func missingClockKeepsTheCode() {
        #expect(CodeAge.decide(createdAt: nil, maxAgeDays: 7, now: days(100), sessionLive: false) == .keep)
    }

    @Test func copy() {
        #expect(CodeAge.notice(days: 7) == "Your code was a week old, so Knock made a new one. Share it again.")
        #expect(CodeAge.choices(including: 7).map { CodeAge.label(days: $0) } == ["A week", "2 weeks", "A month", "Never"])
        #expect(CodeAge.choices(including: 3) == [3, 7, 14, 30, 0])
        #expect(CodeAge.label(days: 3) == "3 days")
    }

    @Test func newCodeRestartsTheClock() {
        let d = DoorFile(doorId: "bvjy", doorKey: "key-0123456789abcdef", code: "4821KQ", codeVersion: 1,
                         salt: Data(repeating: 7, count: 16), codeHash: Data(repeating: 1, count: 32), codeCreatedAt: made, claim: "c1")
        let next = d.withNewCode(now: days(10).addingTimeInterval(0.6))
        #expect(next.codeCreatedAt == days(10))
        #expect(next.codeVersion == 2 && next.claim == "c1")
    }
}
