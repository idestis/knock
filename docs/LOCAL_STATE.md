# Local state shared by Knock.app and the knock CLI

Both clients read and write the same files so a user can switch between them.
Everything lives in `~/.knock/` (mode `0700`, files `0600`). Both clients honour
`KNOCK_HOME` to use another folder (handy for testing two identities on one Mac).

| Path | Contents |
|---|---|
| `~/.knock/config.toml` | Settings (section 12 of DESIGN.md). Only flat `key = value` lines under `[relay]`, `[sessions]`, `[guest]`, `[knock]`, `[shell]` tables. |
| `~/.knock/door.json` | This device's door: `{"door_id","door_key","code","code_version","salt","code_hash"}` (salt and hash base64). |
| `~/.knock/recent.json` | `{"items":[{"door_id","owner_name","owner_email","provider","verified","device","code_version","code_hash","last_session_at","mismatch_since"}]}` |
| `~/.knock/sessions/<ts>_<peer>_<id>/` | Recordings: `meta.json`, `session.cast` (asciicast v2), optional `keys.jsonl`. |

## Sign-in tokens

Stored in the login Keychain as a generic password:

- service `io.github.idestis.knock`, account `session`
- value: the JSON body returned by `POST /v1/auth/token` / `POST /v1/auth/refresh`
  (`access_token`, `refresh_token`, `expires_at`, `refresh_expires_at`, `user`).

Clients refresh the access token when it has less than 5 minutes left, and save
the rotated refresh token. The refresh token lasts 90 days and slides on every
refresh, so a user who opens Knock at least once a quarter never signs in again.
No entry means anonymous.

## Door and code formats

- Door id: 4 characters from `abcdefghjkmnpqrstuvwxyz23456789`, generated once per device (older clients made 8; any 4–32 is valid).
- Door key: 32 random bytes, base64url. Proves ownership of the door id to the relay.
- Code: 4 digits + 2 letters (no I/O), shown as `4821 KQ`.
- The code people share is door + code, shown and copied as `BVJY 1696 ME`.
  Parsers drop a leading `knock`, strip spaces, dashes and `·`, then take the last 6 characters
  as the code (upper-cased) and everything before as the door id (lower-cased).
- Hash: PBKDF2-HMAC-SHA256, 600 000 iterations, 32 bytes, 16-byte salt.
