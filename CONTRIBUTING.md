# Contributing to Knock

Thanks for helping. A few things make reviews quick:

1. **Read [DESIGN.md](DESIGN.md) section 2 first.** Knock has rules it never breaks: nothing happens without the host, quitting closes the door, the relay never learns the code. Changes that bend them won't be merged.
2. **Run `task test`** before you open a pull request. It runs gofmt, `go vet`, `go test -race` and the Swift tests.
3. **Keep the protocol in step.** If you touch `protocol/`, update `mac/Sources/Relay/Protocol.swift` in the same pull request.
4. **Use Conventional Commits** (`feat(cli): …`, `fix(relay): …`). The changelog and release notes are generated from them.
5. **Ask before adding dependencies.** Open an issue first.

## Security

Please don't open public issues for vulnerabilities. Use [GitHub's private vulnerability reporting](https://github.com/idestis/knock/security/advisories/new) instead.
