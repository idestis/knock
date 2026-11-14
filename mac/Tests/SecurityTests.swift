import Foundation
import Testing

@Suite struct SecurityTests {
    private func scratch() throws -> URL {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("knock-fda-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir
    }

    @Test func readableProbeMeansGranted() throws {
        let dir = try scratch()
        defer { try? FileManager.default.removeItem(at: dir) }
        let f = dir.appendingPathComponent("TCC.db")
        try Data("x".utf8).write(to: f)
        #expect(FullDiskAccess(probes: [f.path]).isGranted)
    }

    @Test func unreadableProbeMeansNotGranted() throws {
        let dir = try scratch()
        let f = dir.appendingPathComponent("TCC.db")
        defer {
            try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: f.path)
            try? FileManager.default.removeItem(at: dir)
        }
        try Data("x".utf8).write(to: f)
        try FileManager.default.setAttributes([.posixPermissions: 0], ofItemAtPath: f.path)
        let readable = dir.appendingPathComponent("later")
        try Data("x".utf8).write(to: readable)
        // The first probe that exists decides, even if a later one is readable.
        #expect(!FullDiskAccess(probes: [f.path, readable.path]).isGranted)
    }

    @Test func missingProbesAreSkipped() throws {
        let dir = try scratch()
        defer { try? FileManager.default.removeItem(at: dir) }
        let ok = dir.appendingPathComponent("Bookmarks.plist")
        try Data("x".utf8).write(to: ok)
        let missing = dir.appendingPathComponent("nope").path
        #expect(FullDiskAccess(probes: [missing, ok.path]).isGranted)
        #expect(!FullDiskAccess(probes: [missing]).isGranted)
        #expect(!FullDiskAccess(probes: []).isGranted)
    }

    @Test func screenOnlyEverDeclines() {
        let anon = Identity(id: "", name: "Sam", provider: "", verified: false)
        let real = Identity(id: "github:1", name: "Sam", provider: "github", verified: true)
        #expect(KnockScreen.decide(from: anon, verifiedOnly: true) == .declineUnverified)
        #expect(KnockScreen.decide(from: anon, verifiedOnly: false) == .ask)
        #expect(KnockScreen.decide(from: real, verifiedOnly: true) == .ask)
        #expect(KnockScreen.decide(from: real, verifiedOnly: false) == .ask)
    }
}
