# Sign-in on your relay

Knock.app and the `knock` CLI sign people in with Google or GitHub through your relay. You, the relay operator, create the OAuth apps and give the relay their client IDs and secrets. Clients never see those secrets: they ask the relay which providers it offers (`GET /v1/auth/providers`) and run a PKCE flow against it.

What sign-in gets you: the relay knows who every host and helper is, and a host sees the knocker's verified name, email (or GitHub username) and provider on every knock. Without it, people knock with a name they typed, marked *not verified*.

## 1. Pick the relay's public URL

Providers send people back to the relay, so it needs a URL their browser can reach:

```
KNOCK_PUBLIC_URL=https://knock.example.com
```

The callback URLs are this URL plus `/v1/auth/{provider}/callback`:

- `https://knock.example.com/v1/auth/google/callback`
- `https://knock.example.com/v1/auth/github/callback`

A relay that is only on your tailnet works too (`https://knock.tailnet-name.ts.net`); the browser doing the sign-in just has to reach it. Use `https` for anything but local testing.

## 2. Create a GitHub OAuth app

1. Go to **GitHub → Settings → Developer settings → OAuth Apps → New OAuth App**. For an organization, create it under **Organization settings → Developer settings → OAuth Apps** so it isn't tied to one person.
2. **Application name**: `Knock`. **Homepage URL**: your relay URL.
3. **Authorization callback URL**: `https://knock.example.com/v1/auth/github/callback`
4. Leave **Enable Device Flow** off.
5. Create it, then **Generate a new client secret**.

```
KNOCK_GITHUB_CLIENT_ID=Ov23li...
KNOCK_GITHUB_CLIENT_SECRET=...
```

To allow only members of one organization:

```
KNOCK_GITHUB_ORG=example-org
```

The relay then also asks for `read:org` and accepts only **active** members (a pending invitation isn't enough). If the organization restricts third-party OAuth apps, an org owner has to approve the app under **Organization settings → Third-party access**, or members are turned away with a message saying so.

## 3. Create a Google OAuth client

1. In the [Google Cloud console](https://console.cloud.google.com/), pick or create a project.
2. **APIs & Services → OAuth consent screen**: set it up with the app name `Knock`. Choose **Internal** if only your Workspace should sign in, **External** otherwise. Scopes: `openid`, `email`, `profile`.
3. **APIs & Services → Credentials → Create credentials → OAuth client ID**, type **Web application**.
4. **Authorized redirect URIs**: `https://knock.example.com/v1/auth/google/callback`
5. Create it and copy the client ID and secret.

```
KNOCK_GOOGLE_CLIENT_ID=1234567890-abc.apps.googleusercontent.com
KNOCK_GOOGLE_CLIENT_SECRET=GOCSPX-...
```

To allow only one Google Workspace domain:

```
KNOCK_GOOGLE_DOMAIN=example.com
```

The relay checks the `hd` claim Google returns for the account, so personal Gmail accounts and other domains are refused even if someone edits the sign-in URL. Accounts need a verified email either way.

## 4. Decide about anonymous use

| `KNOCK_ALLOW_ANONYMOUS` | No provider set | Google or GitHub set |
|---|---|---|
| unset or empty | anonymous only (local dev) | **sign-in required** |
| `true` | anonymous only | sign-in optional; anonymous knocks are marked *not verified* |
| `false` | refuses to start | sign-in required |

When sign-in is required, the relay refuses an anonymous hello with `anonymous_not_allowed`. The app and the CLI read `sign_in_required` from `GET /v1/auth/providers` (and `GET /v1/info`) and ask people to sign in first.

`task relay:run` with nothing set stays anonymous, as before.

## 5. Keep tokens stable

```
KNOCK_TOKEN_SECRET=$(openssl rand -base64 48)
```

The relay signs its own tokens with this secret: a one-hour access token and a 90-day refresh token that renews on every use. Keep it stable or everyone has to sign in again after a restart. Rotating it on purpose signs everyone out.

Changing `KNOCK_GOOGLE_DOMAIN` or `KNOCK_GITHUB_ORG`, or removing a provider, also stops existing sign-ins from refreshing, so everyone affected signs in again under the new rules within an hour.

## Check it

```sh
curl -s https://knock.example.com/v1/auth/providers
# {"providers":["google","github"],"sign_in_required":true}
knock login
knock whoami
```

## What the relay keeps and logs

- Provider access tokens are used once to read the profile and dropped. The relay keeps no user database: the identity lives in its signed tokens.
- Logs carry the provider and the user's stable ID (`github:123`, `google:456`), never tokens, codes, grants or provider responses.
- Signing out deletes the tokens from the Keychain. Tokens are stateless, so the relay can't revoke one early; an access token stops working within the hour and a refresh token at its expiry, unless you rotate `KNOCK_TOKEN_SECRET`.
- A sign-in only returns to `knock://auth/callback` (the app) or `http://127.0.0.1:PORT/callback` (the CLI). Each client address can start 30 sign-ins per 10 minutes; past that `/start` answers 429. The address is worked out as for knocks: the socket's address, or the first untrusted `X-Forwarded-For` hop when the request comes through a proxy listed in `KNOCK_TRUSTED_PROXIES`.
- Membership is checked at sign-in. Someone removed from the org or domain keeps their sign-in until it lapses (90 days without using Knock) or you rotate `KNOCK_TOKEN_SECRET`.
