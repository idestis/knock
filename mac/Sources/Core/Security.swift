import Foundation

/// Whether macOS has given Knock Full Disk Access.
///
/// The shared shell is a child of Knock, so macOS checks Knock's privacy permissions
/// when that shell touches Documents, Desktop, Downloads, iCloud Drive or Mail.
/// There's no API to ask, so this tries to open a file only Full Disk Access can read.
nonisolated struct FullDiskAccess: Sendable {
    /// Files that are always present and only readable with Full Disk Access.
    /// The first one that exists decides.
    var probes: [String]

    init(probes: [String] = FullDiskAccess.defaultProbes) { self.probes = probes }

    static var defaultProbes: [String] {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        return [
            home + "/Library/Application Support/com.apple.TCC/TCC.db",
            home + "/Library/Safari/Bookmarks.plist",
        ]
    }

    /// The System Settings pane where the user turns it on.
    static let settingsURL = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles")!

    var isGranted: Bool {
        for path in probes {
            let fd = open(path, O_RDONLY)
            if fd >= 0 { close(fd); return true }
            if errno == ENOENT || errno == ENOTDIR { continue }
            return false
        }
        return false
    }
}

/// What to do with a knock before the host is asked. It can only ever turn a knock
/// away: letting someone in always takes the host's click.
nonisolated enum KnockScreen: Equatable, Sendable {
    case ask
    case declineUnverified

    static func decide(from: Identity, verifiedOnly: Bool) -> KnockScreen {
        verifiedOnly && !from.verified ? .declineUnverified : .ask
    }
}
