# Local state shared by Knock.app and the knock CLI

Both clients read and write the same files so a user can switch between them.
Everything lives in `~/.knock/` (mode `0700`, files `0600`). Both clients honour
`KNOCK_HOME` to use another folder (handy for testing two identities on one Mac).

| Path | Contents |
|---|---|
| `~/.knock/config.toml` | Settings (section 12 of DESIGN.md). Only flat `key = value` lines under `[relay]`, `[sessions]`, `[guest]`, `[knock]`, `[door]`, `[shell]` tables. |
| `~/.knock/relays/<relay>/door.json` | This device's door on that relay: `{"relay","door_id","door_key","code","code_version","code_created_at","salt","code_hash","claim"}` (salt and hash base64, `code_created_at` RFC 3339 UTC, `claim` optional). |
| `~/.knock/relays/<relay>/recent.json` | Doors joined through that relay: `{"items":[{"door_id","owner_name","owner_email","provider","verified","device","code_version","code_hash","last_session_at","mismatch_since"}]}` |
| `~/.knock/sessions/<ts>_<peer>_<id>/` | Recordings: `meta.json`, `session.cast` (asciicast v2), optional `keys.jsonl`. |

## Per-relay state

A door key, a code hash and a sign-in are bearer secrets for the relay that
accepted them. Clients keep them per relay and never send one relay's secrets
to another, so pointing Knock at a different relay starts with a fresh door, an
empty Recent list and no sign-in there.

- The relay's origin is the configured URL reduced to `scheme://host[:port]`:
  lower case, default port (80 for http, 443 for https) dropped, no path or
  trailing slash. Example: `https://knock.example.com`, `http://127.0.0.1:8787`.
- `<relay>`, the folder name, is the origin with `://` replaced by `_`, `:`
  replaced by `_`, and IPv6 brackets removed with the address's `:` replaced by
  `-`. Examples: `https_knock.example.com`, `http_127.0.0.1_8787`, `http_--1_8787`.
- `door.json` stores the origin in `relay`. A client that finds a different
  origin there ignores the file.
- Migration from older builds: if `~/.knock/door.json` or `~/.knock/recent.json`
  exists and the current relay has no file of that kind yet, the client moves it
  (rename) into the current relay's folder and adds `relay`. The legacy Keychain
  account `session` moves to the current relay's account the same way. Whichever
  client runs first does the move; the other then finds nothing to migrate.
  If the current relay already has its own file or sign-in, the legacy one is
  deleted instead, so it can never move into another relay's folder later.

## Stale codes

The relay refuses a `door.open` or `door.code.set` whose code is older than the one
it holds (`code_stale`), so a stale client can't bring back a code someone was cut
off from. The error carries `code_version`, the version the relay holds. A client
first re-reads `door.json` and retries once. If the relay is still ahead, the door
key holder makes a fresh code with `code_version` one above the relay's, saves it,
retries, and tells the host to share the new code. It never adopts an older code.

## Code age

`door.code_max_age_days` in `config.toml` (default `7`, `0` means never) limits
how long one code lives. A client checks it when it opens the door and while the
door is open and idle. If `code_created_at` is older than the limit, it rotates
the code exactly like "New code" and tells the host to share the new one. It never
rotates during a live session: it waits until the session ends. Rule 2 still
holds: nothing runs while both apps are closed, so an expired code is rotated the
next time the door opens. A `door.json` without `code_created_at` gets the
current time on load, which starts the clock.

## Sign-in tokens

Stored in the login Keychain as a generic password:

- service `io.github.idestis.knock`, account `session:<origin>` (for example
  `session:https://knock.example.com`); older builds used the single account `session`
- value: the JSON body returned by `POST /v1/auth/token` / `POST /v1/auth/refresh`
  (`access_token`, `refresh_token`, `expires_at`, `refresh_expires_at`, `user`).
  `user` is the relay's identity: `id`, `name`, `email`, `login` (GitHub), `provider`, `verified`, `avatar`.

Clients refresh the access token when it has less than 5 minutes left, and save
the rotated refresh token. The refresh token lasts 90 days and slides on every
refresh, so a user who opens Knock at least once a quarter never signs in again.
No entry means anonymous.

If the relay refuses the access token at `hello` (`unauthorised`), clients refresh
once even if the token looks valid, and delete the entry when the refresh is
refused (HTTP 401). A refresh that fails because the relay is unreachable keeps
the entry.

## Door and code formats

- Door id: 4 characters from `abcdefghjkmnpqrstuvwxyz23456789`, generated once per device (older clients made 8; any 4–32 is valid).
- Door key: 32 random bytes, base64url. Proves ownership of the door id to the relay.
- Claim: an opaque string the relay returns in `door.opened`. Clients store it in
  `door.json` and send it with every `door.open`, so the key holder can take the
  door id back after a relay restart even if someone else opened it first.
  Every `door.opened` brings a fresh claim; save it over the old one. A claim
  is good for 90 days after the relay issued it.
- Code: 4 digits + 2 letters (no I/O), shown as `4821 KQ`.
- The code people share is door + code, shown and copied as `BVJY 1696 ME`.
  Parsers drop a leading `knock`, strip spaces, dashes and `·`, then take the last 6 characters
  as the code (upper-cased) and everything before as the door id (lower-cased).
- Hash: PBKDF2-HMAC-SHA256, 600 000 iterations, 32 bytes, 16-byte salt.
