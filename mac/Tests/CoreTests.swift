import Foundation
import Testing

@Suite struct CodeTests {
    @Test func codeShape() {
        for _ in 0..<200 {
            let c = Codes.newCode()
            #expect(Codes.isValidCode(c))
            #expect(!c.contains("I") && !c.contains("O"))
        }
    }

    @Test func normaliseAndDisplay() {
        #expect(Codes.normalise(" 4821 kq ") == "4821KQ")
        #expect(Codes.normalise("4821-KQ") == "4821KQ")
        #expect(Codes.display("4821kq") == "4821 KQ")
    }

    /// Same vector as Go: protocol.HashCode("4821KQ", salt 0..15, 1000).
    @Test func hashMatchesGo() {
        let salt = Data((0..<16).map { UInt8($0) })
        let h = Codes.hash(code: "4821 kq", salt: salt, iterations: 1000)
        #expect(h.count == 32)
        #expect(h == Codes.hash(code: "4821KQ", salt: salt, iterations: 1000))
        #expect(h.map { String(format: "%02x", $0) }.joined() == GoVector.hash1000)
    }

    @Test func shareParsing() {
        let want = ShareString(doorId: "bvjy", code: "4821KQ")
        #expect(ShareString.parse("BVJY 4821 KQ") == want)
        #expect(ShareString.parse("bvjy-4821kq") == want)
        #expect(ShareString.parse("knock bvjy 4821KQ") == want)
        #expect(ShareString.parse("bvjy4821KQ") == want)
        #expect(ShareString.parse("k7f3x9q2 4821KQ") == ShareString(doorId: "k7f3x9q2", code: "4821KQ"))
        #expect(ShareString.parse("bvjy 4821IO") == nil)
        #expect(ShareString.parse("4821KQ") == nil)
        #expect(ShareString.parse("hello") == nil)
        #expect(want.text == "BVJY 4821 KQ")
    }

    @Test func configParse() {
        let c = ConfigFile.parse("""
        # comment
        [relay]
        url = "https://knock.example.ts.net"  # trailing
        [sessions]
        save = "output"
        retention_days = 14
        """)
        #expect(c["relay.url"] == "https://knock.example.ts.net")
        #expect(c["sessions.save"] == "output")
        #expect(c["sessions.retention_days"] == "14")
    }

    @Test func envelopeRoundTrip() throws {
        let s = try Envelope.encode(type: "knock.request", id: "7", data: KnockRequest(doorId: "abcd2345", codeHash: Data([1, 2, 3])))
        let env = try Envelope.parse(Data(s.utf8))
        #expect(env.type == "knock.request")
        #expect(env.id == "7")
        #expect(s.contains("\"door_id\":\"abcd2345\""))
        #expect(s.contains("\"code_hash\":\"AQID\""))
        let back = try env.decode(KnockRequest.self)
        #expect(back.codeHash == Data([1, 2, 3]))
    }
}
