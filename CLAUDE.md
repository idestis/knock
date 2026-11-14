# Knock: agent instructions

Knock lets a host share one shell on their Mac with a helper through a self-hosted relay. Read `DESIGN.md` before feature work; it's the source of truth. `docs/LOCAL_STATE.md` is the contract between the app and the CLI.

## Rules the product never breaks

Check every change against these. If a change bends one, stop and ask.

1. Nothing happens without the host: a session starts only on an explicit "Let in" click or `y`. Never add a default button, auto-accept, or "remember this person" that skips the prompt, not even behind a debug flag.
2. App closed means unreachable: no launch agents, login items or daemons.
3. A code is only the right to knock.
4. A new code cuts everyone off: it ends the live session and pending knocks, and stored hashes stop working.
5. The host sees everything, including the guest's non-echoed keys, and can end the session at any time.
6. The relay never learns the code: clients send PBKDF2 hashes only. Never log codes, hashes, tokens or terminal bytes, anywhere.

## Layout

| Path | What | Notes |
|---|---|---|
| `protocol/` | Go wire types, codes, PBKDF2 | Change together with `mac/Sources/Relay/Protocol.swift` |
| `client/` | Go WebSocket client | Used by the CLI and `tools/smoke` |
| `relay/` | `knockd` | `internal/hub` holds all state under one mutex; `internal/auth` does tokens and OAuth |
| `cli/` | `knock` CLI | Charm v2 (`charm.land/...`), creack/pty. Raw passthrough during sessions, no TUI over the stream |
| `mac/` | Knock.app | SwiftUI, Swift 6, default MainActor isolation; XcodeGen `project.yml`; SwiftTerm; Sparkle |
| `tools/smoke` | Dev harness that knocks as a guest | The host still clicks |

## Commands

- `task test`: everything (gofmt check, vet, `go test -race`, Swift tests). Run it before you say you're done.
- `task go:test`, `task app:test`, `task app:build`, `task app:run`, `task relay:run`, `task cli:build`.
- xcodebuild needs `-skipPackagePluginValidation -skipMacroValidation` (SwiftTerm ships a build plugin); the Taskfile passes them.
- Run the app against a scratch state dir: `KNOCK_HOME=/tmp/k open -n mac/build/dd/Build/Products/Debug/Knock.app`.
- Debug builds only: `KNOCK_SNAPSHOT=/dir` renders key screens to PNG with ImageRenderer every 3 s. AppKit controls show as yellow boxes and glass as a plain fill.

## Conventions

- **Wire format.** JSON envelopes `{type, id, data, error}`; snake_case keys; `[]byte` is standard base64. In Swift, name properties without acronyms (`doorId`, not `doorID`) so the snake_case strategies round-trip.
- **Hash parity.** `protocol/code_test.go` and `mac/Tests/GoVector.swift` pin the same PBKDF2 vector. If one changes, both must.
- **Code format.** Shown and copied as `BVJY 1696 ME`: a 4-char door id plus a 6-char code. Parsers take the last 6 characters as the code and the rest as the door. 8-char door ids from older builds still parse.
- **Swift.** Pure types in `Core/` and `Relay/Protocol.swift` are marked `nonisolated` because hashing runs off the main actor. PBKDF2 is slow (~0.3 s); never call it on the main actor.
- **UI.** Follow the mocks in `design/mock/mocks/`. Glass goes on the navigation layer only (sidebar card, alerts, capsules); lists and body text sit on solid surfaces. Accent is `#0043F3`. Respect Reduce Motion.
- **Copy.** Short, second person, sentence case: "That code doesn't work. Ask for a new one."
- **Commits.** Conventional Commits; git-cliff builds the changelog and release notes. A `Release-note:` footer overrides the line.
- **Dependencies.** Ask before adding new ones. Pin exact versions.

## Releases

One version for app, CLI and relay in `mac/Config/Version.xcconfig`, with tags `vX.Y.Z`. `task release` (or `task release -- X.Y.Z`) then `task publish` uploads the DMG, CLI tarballs, `appcast.xml` and checksums to `github.com/idestis/knock` releases, then `task tap:update` rewrites the Homebrew tap (`idestis/homebrew-tap`, checked out at `../homebrew-tap`) from `dist/checksums.txt`. User-facing notes come from `scripts/cliff-release.toml` and `scripts/cliff-sparkle.toml`; `CHANGELOG.md` from `cliff.toml`. Sparkle reads `releases/latest/download/appcast.xml`, and the EdDSA private key stays in the maintainer's login keychain (account `knock`). There is no R2/S3 bucket.

## Known gaps (v0.1)

- The relay is single-replica and in memory; DynamoDB and pod-to-pod forwarding are designed but not built.
- No end-to-end encryption yet: the relay could read session bytes in memory.
- A session doesn't survive a relay restart ("Relay restarted, knock again").
