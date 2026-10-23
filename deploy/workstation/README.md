# Workstation deploy

Personal deployment of this fork on the Fedora PC "workstation": one Docker
container, reachable from other devices only through the tailnet. Secrets are
not in this directory; see [Secrets](#secrets).

## Layout on the host (`~/cliproxy`)

```
~/cliproxy/
├── compose.yaml           # copy of ./compose.yaml
├── .secrets               # management key in plaintext (chmod 600)
├── data/
│   ├── config.yaml        # from ./config.example.yaml, placeholders filled in
│   └── static/management.html   # panel, downloaded by the server
└── auths/                 # OAuth credentials, session-affinity.sab, usage-ledger.ulg
```

## Fresh setup

1. Copy `compose.yaml` to `~/cliproxy/` and `config.example.yaml` to
   `~/cliproxy/data/config.yaml`.
2. Fill in the placeholders in `config.yaml`:
   - `management.secret-key`: the management key. Plaintext is fine; the
     server replaces it with a bcrypt hash on first start. Keep the plaintext
     in `~/cliproxy/.secrets` (`chmod 600`).
   - `access.api-keys`: the client API key. Store it in the GNOME keyring:
     `secret-tool store --label="CLIProxyAPI client key" service cliproxy-client-key`.
3. `cd ~/cliproxy && docker compose up -d`
4. Log in the subscription accounts through the panel
   (`http://127.0.0.1:8317/management.html`). The OAuth callbacks need ports
   1455 (Codex) and 54545 (Claude), which compose binds on loopback.
5. Expose it on the tailnet (tailnet only, persists across reboots):
   `sudo tailscale serve --bg http://127.0.0.1:8317`
   → `https://workstation.tailbbcf6c.ts.net`
6. Boot autostart: `sudo systemctl enable docker.service tailscaled.service`.
   The container has `restart: unless-stopped`.

## Update

- Backend: `cd ~/cliproxy && docker compose pull && docker compose up -d`.
  The image `ghcr.io/lautarosegura/cli-proxy-api:latest` is built on `v*` tags
  by `.github/workflows/fork-ghcr-image.yml`.
- Panel: the server pulls the latest release of
  `lautarosegura/Cli-Proxy-API-Management-Center` at start and every 3 hours.
  To apply a release right away without restarting (live sessions survive):

  ```bash
  gh release download <tag> -R lautarosegura/Cli-Proxy-API-Management-Center -p management.html -D /tmp
  docker cp /tmp/management.html cli-proxy-api:/CLIProxyAPI/data/static/management.html
  ```

## Token usage history

With `observability.usage.usage-statistics-enabled: true` the server keeps
hourly token totals in `auths/usage-ledger.ulg`, which the panel's quota page
charts. To backfill the days before the ledger existed from this machine's
Codex (`~/.codex/sessions`) and Claude Code (`~/.claude/projects`) logs:

```bash
go run ./cmd/import_session_usage -dry-run   # totals only
(set -a; . ~/cliproxy/.secrets; go run ./cmd/import_session_usage)
```

Each run replaces the previous import. Hours at or after the ledger's first
recorded request are dropped, so traffic is never counted twice; usage that
bypassed the proxy after that point (`cxo`, `claudeo`) is not included.

## Secrets

| Secret | Where it lives |
| --- | --- |
| Management key (plaintext) | `~/cliproxy/.secrets`; `config.yaml` holds only its bcrypt hash |
| Client API key | GNOME keyring, `secret-tool lookup service cliproxy-client-key` |
| Account credentials | `~/cliproxy/auths/` |

Back up `auths/` and `.secrets` separately; they are not in git.

## Clients

The proxy is the default; the native login stays available as a bypass.

- **Claude Code** keeps its claude.ai OAuth login (so claude.ai connectors keep
  working) and sends the proxy key as a custom header. `~/.zshenv`:

  ```bash
  export CLIPROXY_API_KEY="$(secret-tool lookup service cliproxy-client-key 2>/dev/null)"
  export ANTHROPIC_BASE_URL="http://127.0.0.1:8317"
  export ANTHROPIC_CUSTOM_HEADERS="X-Api-Key: $CLIPROXY_API_KEY"
  ```

- **Codex**, `~/.codex/config.toml`:

  ```toml
  model_provider = "cliproxy"

  [model_providers.cliproxy]
  name = "CLIProxyAPI"
  base_url = "http://127.0.0.1:8317/v1"
  wire_api = "responses"
  supports_websockets = true

  [model_providers.cliproxy.auth]
  command = "secret-tool"
  args = ["lookup", "service", "cliproxy-client-key"]
  ```

  Bypass profile `~/.codex/oauth.config.toml`: `model_provider = "openai"`.

- **Grok Build** (CLI and T3 Code), `~/.grok/config.toml`:

  ```toml
  [auth]
  auth_provider_command = "secret-tool lookup service cliproxy-client-key"
  auth_provider_label = "CLIProxyAPI"
  auth_token_ttl = 86400

  [endpoints]
  models_base_url = "http://127.0.0.1:8317/v1"

  # The proxy lists every provider's models; keep Grok's native default.
  [models]
  default = "grok-4.7"
  ```

  A `grok.com` login session in `~/.grok/auth.json` wins over the auth provider,
  so that file must hold only the token the provider writes there, never a
  session. The native login lives in a separate home, `~/.grok-native`, with the
  original config.

- **Bypass commands**, `~/.zshrc`:

  ```bash
  alias cxo="codex --yolo -p oauth"
  claudeo() { env -u ANTHROPIC_BASE_URL -u ANTHROPIC_CUSTOM_HEADERS claude "$@"; }
  alias clo="claudeo --dangerously-skip-permissions"
  groko() { GROK_HOME="$HOME/.grok-native" grok "$@"; }
  ```
