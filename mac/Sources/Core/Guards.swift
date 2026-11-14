import Foundation

/// The knock the host just let in. Recorded before the "allow" goes out, so the
/// session.start that follows can be matched to a real "Let in" click (rule 1).
nonisolated struct AllowedKnock: Equatable, Sendable {
    var knockId: String
    var peerId: String
    var peerDevice: String
    var doorId: String
    var codeVersion: Int64
}

/// Whether a session.start from the relay may open a session on this Mac.
/// The relay is not trusted to decide that: a host shell only starts for the knock
/// the host let in, and a guest window only for a knock this app sent.
nonisolated enum SessionGate {
    enum Decision: Equatable, Sendable { case host, guest, refuse }

    /// - allowed: the knock the host let in and is waiting to start, if any.
    /// - door: this device's door id. isOpen: the host's "Open to knocks".
    /// - knockingDoor: the door this app knocked on and is waiting to hear from.
    /// - busy: a session (either side) is already running here; it is never silently replaced.
    static func decide(_ s: SessionStart, allowed: AllowedKnock?, door: String?, isOpen: Bool,
                       knockingDoor: String?, busy: Bool) -> Decision {
        switch s.role {
        case "host":
            guard !busy, isOpen, let a = allowed, let door, s.doorId == door, a.doorId == door,
                  s.peer.id == a.peerId, s.peerDevice == a.peerDevice, s.codeVersion == a.codeVersion else { return .refuse }
            return .host
        case "guest":
            guard !busy, let knockingDoor, s.doorId == knockingDoor else { return .refuse }
            return .guest
        default:
            return .refuse
        }
    }
}

/// door.json is shared with the CLI, which can rotate the code (`knock new-code`) or
/// replace the door while the app runs. These rules decide which record wins.
nonisolated enum DoorSync {
    /// The record to use, given the app's copy and what's on disk.
    /// Disk wins for another door, a higher code version, or the same version with a
    /// different code (two rotations collided); the app's copy wins otherwise.
    static func reconcile(memory: DoorFile?, disk: DoorFile?) -> DoorFile? {
        guard let disk, disk.isWellFormed else { return memory }
        guard let memory else { return disk }
        if disk.doorId != memory.doorId || disk.doorKey != memory.doorKey { return disk }
        let sameCode = disk.codeVersion == memory.codeVersion && disk.codeHash == memory.codeHash && disk.salt == memory.salt
        var winner = disk.codeVersion > memory.codeVersion || (disk.codeVersion == memory.codeVersion && !sameCode) ? disk : memory
        // Same door and key: keep the newest claim either side got from the relay (the
        // file holds whatever was saved last), and the clock of a code both sides share.
        winner.claim = disk.claim ?? memory.claim
        if sameCode { winner.codeCreatedAt = memory.codeCreatedAt ?? disk.codeCreatedAt }
        return winner
    }

    /// The version for a fresh code after the relay refused ours as stale and a reread of
    /// door.json didn't help. `relay` is the version the relay says it holds (nil from an
    /// older relay: no recovery). The result is always above both sides, so an older code
    /// is never adopted.
    static func recoveryVersion(relay: Int?, local: Int64) -> Int64? {
        guard let relay, relay > 0, relay < Int.max, local < Int64.max else { return nil }
        return max(Int64(relay), local) + 1
    }

    /// Whether moving from one record to another cuts off people holding the old code.
    static func cutsOff(from old: DoorFile, to new: DoorFile) -> Bool {
        old.doorId != new.doorId || old.doorKey != new.doorKey || old.codeVersion != new.codeVersion
            || old.codeHash != new.codeHash || old.salt != new.salt
    }
}

/// Limits on numbers the relay sends, so a bad relay can't ask for absurd sizes or timers.
nonisolated enum RelayBounds {
    static let grid = 1...1000
    static let knockExpiry = 1...300

    /// nil when either side is out of range.
    static func size(cols: Int, rows: Int) -> (cols: Int, rows: Int)? {
        grid.contains(cols) && grid.contains(rows) ? (cols, rows) : nil
    }

    static func expiry(_ seconds: Int) -> Int {
        min(max(seconds, knockExpiry.lowerBound), knockExpiry.upperBound)
    }
}

/// Visible names for the keys a guest sends, so the host sees everything,
/// including keys the shell doesn't echo (rule 5).
nonisolated enum KeyRibbon {
    static let special: Set<String> = ["⏎", "⇥", "⌫", "⎋", "↑", "↓", "→", "←"]

    /// Plain typed text, as opposed to a named key.
    static func isText(_ t: String) -> Bool {
        !special.contains(t) && !t.hasPrefix("⌃") && !t.hasPrefix("⎋")
    }

    /// Splits one message from the guest into chips. Escape sequences (arrows, Alt+key,
    /// bracketed paste markers) are parsed on their own, so whatever follows them in the
    /// same frame, like the Enter at the end of a paste, still shows.
    static func tokens(_ d: Data) -> [String] {
        let scalars = Array(String(decoding: d, as: UTF8.self).unicodeScalars)
        var out: [String] = []
        var i = 0
        func appendText(_ s: String) {
            if let last = out.last, last.count < 24, isText(last) {
                out[out.count - 1] = last + s
            } else {
                out.append(s)
            }
        }
        while i < scalars.count {
            let v = scalars[i].value
            if v == 0x1B {
                let (token, next) = escape(scalars, from: i)
                if let token { out.append(token) }
                i = next
                continue
            }
            switch v {
            case 0x0D, 0x0A: out.append("⏎")
            case 0x09: out.append("⇥")
            case 0x7F, 0x08: out.append("⌫")
            case 0x00..<0x20: out.append("⌃" + String(UnicodeScalar(v + 0x40)!))
            case 0x80..<0xA0: out.append("⎋" + String(format: "%02X", v)) // C1 controls: never hidden as text
            default: appendText(String(scalars[i]))
            }
            i += 1
        }
        return out
    }

    /// Parses one escape sequence starting at `from` (an ESC). Returns its chip (nil for
    /// bracketed paste markers, which aren't keys) and the index after it.
    private static func escape(_ s: [Unicode.Scalar], from start: Int) -> (String?, Int) {
        let i = start + 1
        guard i < s.count else { return ("⎋", i) }
        let c = s[i]
        if c == "[" {
            // CSI: parameters 0x30-0x3F, intermediates 0x20-0x2F, final 0x40-0x7E.
            var j = i + 1
            var body = ""
            while j < s.count, (0x20...0x3F).contains(s[j].value) {
                body.unicodeScalars.append(s[j])
                j += 1
            }
            guard j < s.count, (0x40...0x7E).contains(s[j].value) else {
                return ("⎋[" + printable(body), j) // cut short: show what came
            }
            let last = s[j]
            let next = j + 1
            if body.isEmpty || body == "1" {
                switch last {
                case "A": return ("↑", next)
                case "B": return ("↓", next)
                case "C": return ("→", next)
                case "D": return ("←", next)
                default: break
                }
            }
            if last == "~", body == "200" || body == "201" { return (nil, next) } // paste start / end
            return ("⎋[" + printable(body) + String(last), next)
        }
        if c == "O", i + 1 < s.count {
            // SS3: arrows in application mode, F1-F4.
            let f = s[i + 1]
            switch f {
            case "A": return ("↑", i + 2)
            case "B": return ("↓", i + 2)
            case "C": return ("→", i + 2)
            case "D": return ("←", i + 2)
            default: return ("⎋O" + printable(String(f)), i + 2)
            }
        }
        if c.value == 0x1B { return ("⎋", i) } // ESC ESC: the first is a plain Escape
        // Alt+key, including Alt+Enter and Alt+Backspace.
        switch c.value {
        case 0x0D, 0x0A: return ("⎋⏎", i + 1)
        case 0x7F, 0x08: return ("⎋⌫", i + 1)
        case 0x00..<0x20: return ("⎋⌃" + String(UnicodeScalar(c.value + 0x40)!), i + 1)
        default: return ("⎋" + printable(String(c)), i + 1)
        }
    }

    private static func printable(_ s: String) -> String {
        String(String.UnicodeScalarView(s.unicodeScalars.map { $0.value < 0x20 || $0.value == 0x7F ? "·" : $0 }))
    }
}
