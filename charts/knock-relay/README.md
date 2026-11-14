# knock-relay

Helm chart for **knockd**, the self-hosted relay for [Knock](https://github.com/idestis/knock). The relay brokers knocks and forwards session bytes between a host's Mac and a helper. It never runs a shell, and it never learns codes: clients send PBKDF2 hashes only.

The chart version always equals the Knock release (app, CLI, relay image and chart share one version).

## Install

```sh
helm install knock-relay oci://ghcr.io/idestis/charts/knock-relay --version 0.1.0 -f values.yaml
```

Then point Knock.app at the relay in **Settings → General → Relay**, or set `relay.url` in `~/.knock/config.toml`. The install notes print the URL and, with sign-in set up, the callback URLs to register.

## What 0.1.0 can do

- **One replica, state in memory.** Knocks, doors and live sessions live in the pod. A restart or upgrade ends every session ("Relay restarted, knock again"). The chart uses the `Recreate` strategy and fails the render if `replicaCount` isn't 1.
- **`store.type` is `memory` only.** A shared store for several replicas is planned; the `store` block is where it will plug in. Any other value fails the install for now.
- **Draining.** On SIGTERM knockd stops taking knocks and waits up to `drainGrace` for live sessions to end. `terminationGracePeriodSeconds` must be longer, and the chart checks that.

## Examples

### Anonymous dev relay

No sign-in: people knock with a name they type, shown to hosts as *not verified*. Reach it with a port-forward.

```yaml
# values.yaml
relayName: knock-dev
```

```sh
helm install knock-relay oci://ghcr.io/idestis/charts/knock-relay --version 0.1.0 -f values.yaml
kubectl port-forward svc/knock-relay 8787:80
curl -s http://127.0.0.1:8787/v1/info
```

### GitHub org sign-in behind ingress-nginx with TLS

Create the OAuth app first (see [docs/RELAY_SIGN_IN.md](https://github.com/idestis/knock/blob/main/docs/RELAY_SIGN_IN.md)) with the callback URL `https://knock.example.com/v1/auth/github/callback`, then keep its credentials and the token secret in Secrets you manage:

```sh
kubectl create namespace knock
kubectl -n knock create secret generic knock-relay-token \
  --from-literal=token-secret="$(openssl rand -base64 48)"
kubectl -n knock create secret generic knock-relay-github \
  --from-literal=github-client-id=Ov23li... \
  --from-literal=github-client-secret=...
```

```yaml
# values.yaml
publicURL: https://knock.example.com
allowAnonymous: false          # sign-in required (the default once a provider is set)
tokenSecret:
  existingSecret: knock-relay-token
auth:
  github:
    existingSecret: knock-relay-github
    org: example-org           # active members only
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt
    # Sessions are long-lived WebSockets; the default 60 s drops idle ones.
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
  hosts:
    - host: knock.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: knock-relay-tls
      hosts: [knock.example.com]
```

```sh
helm install knock-relay oci://ghcr.io/idestis/charts/knock-relay --version 0.1.0 -n knock -f values.yaml
curl -s https://knock.example.com/v1/auth/providers
# {"providers":["github"],"sign_in_required":true}
```

### On your tailnet only

With the [Tailscale Kubernetes operator](https://tailscale.com/kb/1236/kubernetes-operator), skip the ingress and annotate the Service (as `deploy/knock-relay.yaml` does). This serves plain HTTP on the tailnet, which is fine without sign-in; for an `https://` URL that providers accept, use the operator's `tailscale` ingress class instead.

```yaml
service:
  annotations:
    tailscale.com/expose: "true"
    tailscale.com/hostname: knock
```

## Secrets

- **`KNOCK_TOKEN_SECRET`** signs the relay's own access and refresh tokens. With neither `tokenSecret.existingSecret` nor `tokenSecret.value`, the chart generates 64 random characters on install and reads them back from the live Secret on every upgrade, so sign-ins survive. Tools that render without cluster access (`helm template`, Argo CD) can't read it back and would rotate it on every sync, signing everyone out: use `existingSecret` there.
- **Provider credentials** come from `auth.<provider>.clientId`/`clientSecret` (stored in the chart's Secret) or from `auth.<provider>.existingSecret` with the keys `clientIdKey`/`clientSecretKey`.
- Secrets reach knockd only as `secretKeyRef` env vars, never through a ConfigMap. knockd doesn't log them.
- Changing a Secret's contents doesn't restart the pod. Restart it yourself (`kubectl rollout restart deploy/knock-relay`), knowing it ends live sessions.

## Values

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/idestis/knock-relay` | Relay image |
| `image.tag` | `""` (appVersion) | Image tag; release tags publish `X.Y.Z` |
| `image.digest` | `""` | `sha256:...`; wins over the tag |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `nameOverride`, `fullnameOverride` | `""` | |
| `store.type` | `memory` | Only `memory` in 0.1.0 |
| `replicaCount` | `1` | Must be 1 with the memory store |
| `publicURL` | `""` | `KNOCK_PUBLIC_URL`. Required with a provider. Empty: `https://<first ingress host>`, or knockd's local default |
| `relayName` | `knock-relay` | `KNOCK_RELAY_NAME` |
| `allowAnonymous` | `null` | `KNOCK_ALLOW_ANONYMOUS`. `null`: anonymous only without providers; `true`: anonymous next to sign-in; `false`: sign-in required (needs a provider) |
| `knockTimeoutSeconds` | `60` | `KNOCK_KNOCK_TIMEOUT_SECONDS`: how long a knock waits for the host |
| `drainGrace` | `50s` | `KNOCK_DRAIN_GRACE`: how long a stopping relay waits for live sessions |
| `terminationGracePeriodSeconds` | `60` | Must be longer than `drainGrace` |
| `tokenSecret.existingSecret` | `""` | Secret holding `KNOCK_TOKEN_SECRET` |
| `tokenSecret.existingSecretKey` | `token-secret` | Key in that Secret |
| `tokenSecret.value` | `""` | Inline value (32+ bytes). Empty with no `existingSecret`: generated and kept across upgrades |
| `auth.google.clientId`, `clientSecret` | `""` | Google OAuth client |
| `auth.google.existingSecret` | `""` | Secret with the Google client ID and secret |
| `auth.google.clientIdKey`, `clientSecretKey` | `google-client-id`, `google-client-secret` | Keys in that Secret |
| `auth.google.domain` | `""` | `KNOCK_GOOGLE_DOMAIN`: one Workspace domain only |
| `auth.github.clientId`, `clientSecret` | `""` | GitHub OAuth app |
| `auth.github.existingSecret` | `""` | Secret with the GitHub client ID and secret |
| `auth.github.clientIdKey`, `clientSecretKey` | `github-client-id`, `github-client-secret` | Keys in that Secret |
| `auth.github.org` | `""` | `KNOCK_GITHUB_ORG`: active members of one org only |
| `extraEnv` | `[]` | Extra container env entries |
| `extraEnvFrom` | `[]` | Extra `envFrom` sources |
| `serviceAccount.create` | `true` | |
| `serviceAccount.name` | `""` | |
| `serviceAccount.annotations` | `{}` | |
| `serviceAccount.automountServiceAccountToken` | `false` | knockd never calls the Kubernetes API |
| `podAnnotations`, `podLabels` | `{}` | |
| `podSecurityContext` | non-root 65532, `RuntimeDefault` seccomp | |
| `securityContext` | no privilege escalation, read-only root, drop `ALL` | |
| `resources` | requests 50m / 64Mi, limit 256Mi | |
| `livenessProbe` | `GET /healthz` | |
| `readinessProbe` | `GET /readyz` | |
| `service.type` | `ClusterIP` | |
| `service.port` | `80` | Targets knockd's port 8787 |
| `service.annotations` | `{}` | e.g. Tailscale operator |
| `ingress.enabled` | `false` | |
| `ingress.className` | `""` | |
| `ingress.annotations` | `{}` | Raise WebSocket timeouts (see the example) |
| `ingress.hosts` | `knock.example.com`, `/` | |
| `ingress.tls` | `[]` | |
| `serviceMonitor.enabled` | `false` | Prometheus Operator scrape of `/metrics` |
| `serviceMonitor.interval`, `scrapeTimeout` | `30s`, `10s` | |
| `serviceMonitor.labels` | `{}` | Labels your Prometheus selects on |
| `serviceMonitor.namespace` | `""` (release namespace) | |
| `podDisruptionBudget.enabled` | `false` | With one replica, `minAvailable: 1` blocks node drains from cutting live sessions |
| `podDisruptionBudget.minAvailable` | `1` | Or set `maxUnavailable` instead |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | empty | |
| `priorityClassName` | `""` | |

`/metrics` shares the relay's public port, so an ingress exposes it too. Block it at your proxy if that matters to you.

## Develop

```sh
task chart:lint      # helm lint + helm template with every file in ci/, and checks that bad settings fail
task chart:package   # dist/charts/knock-relay-<version>.tgz
```

Pushing a `vX.Y.Z` tag publishes the chart to `oci://ghcr.io/idestis/charts/knock-relay` (`.github/workflows/chart.yml`). `task release` bumps `Chart.yaml` together with the app version.
