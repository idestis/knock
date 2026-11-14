import Foundation
import Testing

@Suite struct IdentityTests {
    /// A knock.incoming as the Go relay sends it for a GitHub user.
    @Test func decodesRelayIdentity() throws {
        let json = #"{"knock_id":"k1","from":{"id":"github:7","name":"Maya Patel","login":"maya","provider":"github","verified":true},"device":"maya-mbp","expires_in":60}"#
        let k = try Wire.decoder.decode(KnockIncoming.self, from: Data(json.utf8))
        #expect(k.from.login == "maya")
        #expect(k.from.email == nil)
        #expect(k.from.verifiedDetail == "@maya")
        #expect(k.from.verifiedLine == "@maya · GitHub")
    }

    @Test func verifiedLine() {
        let google = Identity(id: "google:1", name: "Sam", email: "sam@acme.dev", provider: "google", verified: true)
        #expect(google.verifiedLine == "sam@acme.dev · Google")
        let bare = Identity(id: "github:2", name: "Ann", email: "", provider: "github", verified: true)
        #expect(bare.verifiedDetail == nil)
        #expect(bare.verifiedLine == "GitHub")
        // An anonymous knocker never gets a verified line, whatever they sent.
        let anon = Identity(id: "anon:1", name: "Sam", email: "ceo@acme.dev", login: "ceo", provider: "anonymous", verified: false)
        #expect(anon.verifiedDetail == nil)
        #expect(anon.verifiedLine == nil)
    }

    @Test func authProviders() throws {
        let p = try Wire.decoder.decode(AuthProviders.self, from: Data(#"{"providers":["github"],"sign_in_required":true}"#.utf8))
        #expect(p == AuthProviders(providers: ["github"], signInRequired: true))
        // Older relays don't send sign_in_required.
        let old = try Wire.decoder.decode(AuthProviders.self, from: Data(#"{"providers":[]}"#.utf8))
        #expect(old.signInRequired == nil)
    }
}
