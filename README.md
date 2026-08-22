<p align="center">
  <img src="internal/admin/static/logo.svg" alt="" width="104" height="104">
</p>

<h1 align="center">spillway</h1>

<p align="center">
  Pool your own AI subscription accounts behind one local proxy,<br>
  so a session doesn't stop at a 429.
</p>

Local, single-user daemon that proxies official AI CLIs (Claude Code, and any
Anthropic-shaped endpoint) and rotates requests across a pool of **your own**
subscription accounts, so a session doesn't stop at a 429.

It is a **proxy, never a client**: the vendor's own CLI stays in the loop, and
spillway forwards its requests byte-faithfully apart from three mutations
(auth header, `account_uuid`, and the model when mapping across providers).
That fidelity is what keeps usage inside your subscription rather than falling
through to metered API billing.

Single user, single machine. Not a team server.

## Use at your own risk

Unofficial. Not affiliated with, endorsed by, or supported by Anthropic,
Moonshot AI, or any provider it talks to.

Read this before running it:

- **It authenticates as the vendor's own CLI**, using that CLI's OAuth client
  id, because that is what keeps traffic inside your subscription instead of
  billing as metered API. Whether pooling several subscription accounts is
  permitted is between you and your provider's terms of service — check them.
  Possible consequences of getting that wrong include rate limiting, billing
  disputes and account suspension. Nobody here can indemnify you against that.
- **Your own accounts only.** Sharing one subscription across several people
  is what the terms of most providers are actually aimed at, and it is not
  what this is for — hence single user, single machine.
- **It can spend real money, but only if you tell it to.** `allowOverage`
  lets a spent account keep serving on pay-as-you-go extra usage. It is off by
  default and every unknown state fails closed, but if you enable it, the
  charges are yours. See [Extra usage](#extra-usage).
- **It handles OAuth tokens.** They live in your OS keychain and never touch
  disk, and the code is here to read. Audit it rather than take that on trust.
- **No warranty.** The MIT licence disclaims all of it — see
  [LICENCE](LICENSE). This software is provided as is.

## Prerequisites

**To build**

| | |
|---|---|
| **Go 1.25.13+** | `go.mod` sets that floor. Go 1.21+ also works — it reads the floor and downloads the right toolchain itself, unless you have set `GOTOOLCHAIN=local`. 1.25.13 specifically because earlier 1.25 patches carry `crypto/tls` and `net/http` CVEs, and this binary terminates TLS and holds OAuth tokens. |
| **git** | to clone. |

Nothing else: no cgo, no npm, no build step for the UI. The SQLite driver is
pure Go and the dashboard is embedded in the binary.

**To run**

| | |
|---|---|
| **A system keychain** | Secrets never touch disk. macOS Keychain and Windows Credential Manager are built in. **Linux needs a running D-Bus Secret Service** — gnome-keyring, kwallet, KeePassXC. There is no file-based fallback, so on a headless box without one, `spillway login` fails at the point it tries to store the token. |
| **The vendor CLI** | `claude` on `PATH`, for `spillway run` and for `import`. Not needed if you point `ANTHROPIC_BASE_URL` at the proxy yourself. |

**Optional**

- **`xdg-open`** (Linux) — `login` opens your browser with it. Without it the
  URL is printed instead; the flow still completes.
- **`notify-send`** (Linux) for desktop notifications. macOS uses `osascript`
  and Windows uses PowerShell, both built in. Notifications are silently off
  where the platform has no notifier.
- **Node 20+** only to run the dashboard's JS test; that test skips when it is
  absent.

## Install

macOS:

```sh
brew install --cask coderage-labs/tap/spillway
```

Windows:

```powershell
scoop bucket add coderage-labs https://github.com/coderage-labs/scoop-bucket
scoop install spillway
```

Linux, or any tagged build:

```sh
gh release download --repo coderage-labs/spillway --pattern '*linux_amd64*'
tar xzf spillway_*_linux_amd64.tar.gz && ./spillway version
```

Asset names carry the version — `spillway_v0.6.0_linux_amd64.tar.gz` — so
`releases/latest/download/<name>` cannot be written without knowing it.
Without `gh`, resolve it first:

```sh
url=$(curl -s https://api.github.com/repos/coderage-labs/spillway/releases/latest \
      | grep -oE 'https://[^"]*linux_amd64\.tar\.gz' | head -1)
curl -sL "$url" | tar xz && ./spillway version
```

From source:

```sh
go install github.com/coderage-labs/spillway/cmd/spillway@latest
```

A build made that way reports `spillway dev` — the release workflow injects
the tag, commit and date at link time, and `go install` does not.

`go install` writes to `$(go env GOPATH)/bin` and does not touch your `PATH`.
If `spillway` is not found afterwards, that directory is missing from it:

```sh
echo 'export PATH="$PATH:$(go env GOPATH)/bin"' >> ~/.zshrc && exec zsh
```

(`spillway statusline install` and `service install` record the binary's
absolute path, so those keep working either way.)

Releases are cut by release-please from commit messages — see
[RELEASING.md](RELEASING.md). Binaries are unsigned: the cask strips the
quarantine attribute on install, but a tarball fetched through a **browser**
carries it and Gatekeeper will refuse to run the binary until you clear it
with `xattr -d com.apple.quarantine spillway`. `curl`, `gh` and `tar` do not
set the attribute, so a download by any of those needs nothing.

### Windows

Supported, with two differences and one caveat.

| | macOS | Linux | Windows |
|---|---|---|---|
| Config, CA, request log | `~/Library/Application Support/spillway/` | `~/.config/spillway/` | `%AppData%\spillway\` |
| Background daemon | launchd agent | systemd **user** unit | Task Scheduler task |
| Credential storage | Keychain | Secret Service, else a 0600 file | Credential Manager |
| Daemon log | `~/Library/Logs/spillway.log` | `journalctl --user -u spillway` | `%LocalAppData%\spillway\spillway.log` |

All three come from `os.UserConfigDir()`.

The Windows daemon is a per-user **Scheduled Task**, not a Windows Service.
That is deliberate: account tokens live in Credential Manager, which is
per-user and unreadable from `SYSTEM`, so a service would install cleanly and
then fail to find a single account.

Two protections work differently there, and neither is a downgrade you should
ignore:

- **File modes.** Everything holding credential material — config, CA private
  key, admin token, request log — is written 0600 on Unix. `Chmod` on Windows
  only toggles a read-only bit, so that mode is not enforcement. Protection
  comes instead from the NTFS ACL inherited from `%AppData%`, which grants the
  user, `SYSTEM` and Administrators and nobody else. Comparable in practice to
  0600 under a world-readable home directory — but inherited rather than
  asserted, so a machine with unusual profile ACLs is on its own.
- **Unix-socket admin listener.** Refused on Windows rather than emulated.
  AF_UNIX exists there and Go can listen on it, but the reason to use a socket
  is that it can be 0600 — and that is exactly the part that does not work. A
  socket that binds while providing none of the protection it is chosen for is
  worse than no socket. Use `127.0.0.1:7657` with `admin.token` set.

**Caveat: no ordinary Windows machine has run this.** What is exercised, on
`windows-latest`, is more than it used to be — the task XML is registered with
a real Task Scheduler, and an integration test installs the service, waits for
the daemon to answer, reinstalls over it and requires the process to be
replaced, then uninstalls and requires it to stop. That found four bugs that
compiled and unit-tested cleanly, including task XML the scheduler rejected
outright, so `service install` had never once worked.

Still exercised by nobody: the logon trigger actually firing at a logon (a CI
runner never logs in), toast notifications, and the browser hand-off during
login. Treat first use as a bug hunt and please file what breaks.

## Quickstart

```sh
spillway login claude <name>   # browser login; adds an account
spillway server                # foreground; see `service` for launchd
spillway run                   # spawns claude, routed through the pool
```

Then open the dashboard at <http://127.0.0.1:7657/>.

`spillway login claude <name>` opens a browser and takes the code back over a
loopback callback on `localhost:54545` — nothing to copy. If that port cannot
be bound (a second login already running, or a headless box), it falls back to
printing the URL and reading `code#state` from stdin, which is the only mode
that works over SSH.

## Commands

| Command | What it does |
|---|---|
| `spillway install [--force]` | Service + status line + plugin, in one go; `uninstall` reverses it |
| `spillway server` | Run the daemon (proxy + admin listener) |
| `spillway run [-- <claude args>]` | Spawn `claude` wired to the proxy; refuses if the daemon is down |
| `spillway status [--json]` | Compact pool summary in the terminal; `--json` for state, accounts and recent requests |
| `spillway accounts [remove <name>]` | List or remove accounts |
| `spillway accounts overage <name> on\|off\|default` | Allow or forbid pay-as-you-go past quota for one account — see [Extra usage](#extra-usage) |
| `spillway accounts priority <name> <n>` | Order selection; lower is preferred |
| `spillway switch <account>\|--auto [--force]` | Point the pool at one account until told otherwise |
| `spillway login claude <name>` | Add a Claude account (OAuth PKCE) |
| `spillway login kimi <name>` | Add a Kimi account (OAuth device flow) |
| `spillway statusline` | Print the Claude Code status line |
| `spillway statusline install\|uninstall\|status` | Wire it into `~/.claude/settings.json` |
| `spillway service install\|uninstall\|status` | Run the daemon in the background — launchd on macOS, a Scheduled Task on Windows, a systemd user unit on Linux |
| `spillway version` | Build identity: tag, commit, date, Go version |

### Attaching clients

```sh
spillway run                                       # MITM mode — the supported path
ANTHROPIC_BASE_URL=http://127.0.0.1:7654 claude    # base URL only, this invocation
```

`run` sets the proxy environment and then execs `claude`. That ordering is
load-bearing: Node reads `NODE_EXTRA_CA_CERTS` once, at startup, when it
builds its root store, so a CA supplied after the process has booted is never
trusted — which is why rolling your own wrapper means exporting it *before*
launching, not from inside anything the client runs.

**Remote Control needs MITM mode.** `claude --remote-control` refuses to start
when `ANTHROPIC_BASE_URL` points anywhere but `api.anthropic.com`, so the
base-URL route cannot carry it. Use `run`.

### Status line

```sh
spillway statusline install
```

```
⛁ work · haiku-4-5  ████████ 94% 5h  ███████░ 88% 7d
```

Serving account, the model **actually** going upstream, and a headroom bar per
quota window. Install refuses to replace a status line another tool owns unless
you pass `--force`, and uninstall leaves a foreign one alone.

**It prints nothing in a session that is not going through spillway.** The
line is installed once and then runs for every Claude Code session on the
machine, and showing the pool to one that is not attached is worse than
silence — the numbers are real, but they describe traffic that session is not
part of. Attachment is read from `HTTPS_PROXY` (or `ANTHROPIC_BASE_URL`)
pointing at the configured listener. Pass `--always` in the command if you
want it regardless.

### Claude Code plugin

```sh
spillway install          # or, on its own:
claude plugin marketplace add coderage-labs/spillway
claude plugin install spillway@spillway
```

Adds `/spillway:status`, which reports the pool from inside a session: headroom,
what is serving, whether anything is parked waiting for a reset, and whether
any request is being billed.

`/spillway:switch <account>` pins the pool from the same place — it resolves a
label or a unique prefix to the account name, and reports what would happen
rather than forcing past a refusal that costs money.

Both exist for Remote Control. The status line and the desktop notification
live on the machine, so a session driven from a phone has no way to see or
change any of this; a slash command is the one channel that travels.

## MITM mode

The proxy listener also answers CONNECT: hosts of configured upstreams are
terminated with a locally minted leaf and fed into the same pool pipeline;
every other host is a blind TCP relay, so only configured vendor hosts are ever
decrypted. The CA private key lives in the OS keychain (survives restarts,
never touches disk); the CA cert is written to `~/.config/spillway-ca.pem`
(0600) and trusted only by spawned CLIs via `NODE_EXTRA_CA_CERTS` — never the
system store. Upstream TLS is always fully verified.

Identity-bound paths — `/v1/oauth/token`, `/v1/code/*`, `/v1/environments/*`,
`/v1/sessions/*`, `/api/oauth/files/*`, `/api/oauth/file_upload`, and WebSocket
upgrades (`/v1/session_ingress/ws*`) — relay with the client's own credential
verbatim: no injection, no pool, no rewrite. That is what keeps Remote Control
and the CLI's own token refresh working through the proxy.

## Config

`~/.config/spillway.yaml` (override with `SPILLWAY_CONFIG`), created with
defaults on first run, mode 0600. **Tokens are not kept here** — they live in the OS
keychain, and any inline tokens from an older config are migrated out at
startup, so this file settles to metadata only.

```yaml
proxy:
  port: 7654
  host: 127.0.0.1
  allowRemote: false        # required to bind anywhere but loopback — see below
upstream: https://api.anthropic.com
egress:
  mode: direct              # direct | http-connect | environment
  proxy: ""                 # required for http-connect
admin:
  addr: 127.0.0.1:7657      # a path here serves a 0600 unix socket instead
pool:
  exhaustedMode: notify     # fail | hold | notify
  holdMax: 4h               # "0" never holds
  switchThreshold: 0.98     # used-fraction at which an account is skipped
  probeOnStart: true        # one cheap request per account with no quota data
  probeInterval: 30m        # re-probe stale standby accounts; "0" = startup only
  canaryInterval: 2h        # check idle accounts for dead credentials; "0" = off
  maxBufferBytes: 8388608   # largest body still eligible for cross-account retry
  crossProvider: false      # see the cross-provider caveat below
log:
  level: info
accounts:
  - name: you@example.com
    label: work             # what the dashboard and status line show
    type: claude-oauth
    source: keychain        # the claude CLI's own login (see below)
    priority: 0             # lower is preferred; ties break on load
  - name: kimi
    type: kimi-oauth
    upstream: https://api.kimi.com/coding
    modelMap:
      claude-haiku-4-5-20251001: kimi-for-coding   # exact match wins
      claude-*: k3                                  # glob; longest pattern wins
```

### Binding the proxy off loopback

The proxy port has no authentication of its own and stamps a pooled account's
bearer token onto every request it forwards. Off loopback that makes it an
open credential-injecting relay: anyone who can reach it spends your quota —
your money, under `allowOverage` — and can read or rewrite every prompt and
response going through it.

So a non-loopback `proxy.host` is refused outright and the daemon does not
start. Setting `proxy.allowRemote: true` is the opt-in, and the daemon warns
at every startup while it is on. If you need the pool reachable from another
machine, prefer an SSH tunnel or a reverse proxy that terminates TLS and
authenticates callers over exposing this port directly.

(The admin listener answers the same question its own way: off loopback its
token becomes mandatory, and a missing one fails closed.)

## Credentials

**One credential, one refresher.** Refresh tokens rotate on use, so two
processes refreshing the same account will invalidate each other.

- The account the `claude` CLI is logged into must be `source: keychain`.
  spillway reads it and never refreshes it — the CLI owns it. Note this
  follows the CLI's *current* login: if you `/login` as someone else, that
  entry silently becomes the other account.
- Every other account is spillway's alone: tokens in the keychain under
  spillway's own service, refreshed by the daemon and by nothing else.
- A background sweep refreshes any token within 5 minutes of expiry,
  regardless of traffic — an idle account has nothing else to trigger one.

### Where they are kept

The OS keychain: Keychain Services, Credential Manager, or Secret Service.

**Linux without a desktop keyring is the exception.** Secret Service is a
D-Bus service that a desktop provides and a server, a container or an SSH
session does not, and spillway used to exit rather than start without it. It
now falls back to `spillway-secrets.json` beside the config, 0600 in a 0700
directory — the same way the `claude` CLI stores the same class of token on
Linux. It says so, loudly, every time it does it.

It is a fallback, not a choice: it happens only where no keychain service is
running at all. A locked macOS keychain or a dismissed Windows prompt is you
declining, and spillway will fail rather than answer that by writing your
tokens to a file.

## Model mapping

Providers that speak different model ids get the request's `model` rewritten
(design doc §4 mutation #3). **An id with no mapping is a hard error** —
forwarding a Claude id to another provider is how you get a 200 back from
something you did not ask for.

Kimi ships defaults, so a freshly logged-in account works with no config:

| Asked | Served | Why |
|---|---|---|
| `claude-opus-*` | `k3` | 1,048,576-token context — the only Kimi model that is not a downgrade from what a Claude session may already carry |
| `claude-sonnet-*` | `k3` | as above |
| `claude-haiku-*` | `kimi-for-coding-highspeed` | the background worker: small contexts, latency matters |

Measured from Kimi's `/v1/models`, not assumed. There is deliberately **no
catch-all**: an id from another vendor still stops rather than quietly
becoming `k3`.

Override per account, per key — your entries win individually, the rest keep
their defaults:

```yaml
accounts:
  - name: you@example.com
    type: kimi-oauth
    modelMap:
      claude-haiku-*: k3-256k     # the other three defaults still apply
```

Exact ids beat globs, and among globs the longest pattern wins.

## Pool behaviour

Selection prefers the lowest `priority` among accounts that can serve the
request, then the least loaded — so a reserve account stays unused while a
preferred one has headroom, but never blocks one when it is spent. Sessions
stick to one account (prompt-cache affinity) and rotate only on quota
exhaustion, or when an account crosses `pool.switchThreshold` in any window
(predictive rotation — skipped while another eligible account exists, used
anyway when it is the last one). Claude quota comes from
`anthropic-ratelimit-*` response headers; Kimi's from `/v1/usages` polling.

**Rank accounts of different providers.** At equal priority the tie-break is
in-flight count, and headroom below the threshold does not enter into it — so
whichever account happens to be idle at that instant wins the session, and
`crossProvider: false` then keeps that session on that provider for the rest
of its life. One stray request is enough to run a whole conversation on the
wrong model. `spillway accounts priority <name> <n>` makes the choice
deliberate:

```sh
spillway accounts priority you@work.example 0    # preferred
spillway accounts priority you@side.example 1    # next
spillway accounts priority kimi 2                # last resort
```

**Pinning overrides all of it.** `spillway switch <account>` directs selection
at one account until `spillway switch --auto`, or until the daemon restarts —
it is a live instruction, not a setting, which is the difference between it and
`priority`. Useful for keeping a piece of work on one subscription, steering off
an account you are about to need elsewhere, or watching rotation without having
to spend a quota to see it.

A pin survives the rotate-away threshold — naming an account is a statement
that you want it — but not exhaustion: holding every request while healthy
accounts sit idle would be a way to take yourself offline by accident. It is
refused, needing `--force`, if the account would serve from paid extra usage,
or if it would move a live session to another provider (§6.18: the client
configured its capabilities from the first model it saw). Switching costs the
prompt cache, which is per account.

The pin is pool-wide, not per session, because sticky selection already is: the
session key hashes `metadata.user_id`, which every Claude Code session on the
machine shares.

A quota-429 marks the account exhausted until its reset and re-sends the
buffered request on the next account, invisibly to the client. A transient
rate-limit-429 retries the same account with backoff (max 3), never rotates.
Only `POST /v1/messages` bodies up to 8MB are buffered for failover; everything
else streams straight through, and failover only happens **before the first
response byte** — mid-stream aborts are the client's own retry behaviour.

When every account is spent, `pool.exhaustedMode` decides: `fail` (pass the 429
through), `hold` (park until the soonest reset, up to `pool.holdMax`), or
`notify` (hold plus a loud log), the default. `spillway run` raises the child's
`API_TIMEOUT_MS` past `holdMax` so the client waits out a hold, never lowering
an existing value.

An idle account reports no quota until something is routed to it, so a standby
tank would sit blank. `probeOnStart` sends one minimal request per account with
no reading, and `probeInterval` re-probes readings that have gone stale. This
is spillway originating traffic rather than proxying it, so it is deliberately
narrow: never for an account that already reported recently, never fatal on
failure, and switchable off.

### Settings

The dashboard can edit an allowlisted subset of the config —
`exhaustedMode`, `holdMax`, `switchThreshold`, `probeOnStart`,
`probeInterval`, `crossProvider`, and per-account `label`, `priority` and
`disabled`.
Changes validate before they are written and apply to the running pool with no
restart. Credentials are not editable and are not exposed: token material must
not be reachable from a browser, loopback or not.

`disabled` parks an account — kept, with its credential, but out of rotation.
That is deliberately distinct from the disable that means a credential died,
which un-parking never reverses.

### Cross-provider caveat

Claude Code decides its client-side capabilities from the model name it
believes it is using, **before** any request. `modelMap` rewrites the model
afterwards, so a session that spills from Claude into Kimi mid-flight has
advertised one model's capabilities while another serves it. **Rotation
therefore does not cross provider families by default** — set
`pool.crossProvider: true` to allow it. Same-provider rotation is unaffected.

Beyond that, measured incompatibilities are declared per provider and the
request is checked before routing, so a provider that cannot serve it is never
tried. The one measured case: Kimi reasons by default and rejects a forced
`tool_choice` while it does, so a flow that forces a tool fails there out of
the box. When no account can serve a request the response is a 400 naming the
feature, not a 429 — a capability mismatch is not a rate limit.

The dashboard and status line show the model actually served, and flag it when
it differs from the one requested.

## Extra usage

The only thing here that costs money, and it is off by default.

When a subscription's quota is gone, some providers will keep serving at
pay-as-you-go rates. Spillway can use that as a **last-resort tier**, reached
only after every free account is genuinely spent — including the free 429s,
which cost nothing to attempt.

```yaml
pool:
  allowOverage: true          # pool-wide default
accounts:
  - name: you@example.com
    allowOverage: false       # per-account override; unset means follow the pool
```

Selection is three tiers, and the third is the only one ever charged:

| Tier | Who | Cost |
|---|---|---|
| 1 | under `switchThreshold` | covered |
| 2 | over threshold, will not bill — the provider may still say yes | covered |
| 3 | quota gone, extra usage available **and** permitted | **billed** |

Every default fails closed. An unknown state, an unrecognised header value,
and a provider refusal all mean no. A pool-wide `true` still requires the
provider to have confirmed extra usage is available; naming a single account
explicitly is treated as an assertion and works without that confirmation,
because spillway will not spend a request probing a spent account to find out.

The dashboard shows each account's state read-only — `ON (billable)`,
`available, not enabled`, `unavailable`, or `unknown` — but will not let you
change it. Everything else in that panel changes how long a request waits;
this decides whether it is charged, so it takes a command:

```sh
spillway accounts overage you@example.com on        # this account may be billed
spillway accounts overage you@example.com off       # never, even if the pool allows it
spillway accounts overage you@example.com default   # follow pool.allowOverage
spillway accounts                                   # see the current state
```

`unknown` means no response has come back from that account yet this run.
The state is read from provider headers and held in memory only, so it resets
on every restart and is normally filled in by the startup probe within
seconds. It persists only if `probeOnStart` is off, the probe failed, or the
account has never been used.

Nothing bills silently. A charged request gets a `WARN`, its own `overage`
kind in the request log, a desktop notification, a red badge on the tank, and
`£ N on extra usage` in the status line. The dashboard also shows how much of
the allowance is gone — that is a second, slower cliff, and it refills on a
billing period rather than at the quota window.

## Behind a corporate proxy

Set `egress.mode: http-connect` and `egress.proxy: http://user:pass@host:3128`.
Both paths honour it — ordinary requests and the CONNECT tunnels MITM mode
opens — because proxying only the first would leave every tunnelled host
going direct. `environment` honours `HTTPS_PROXY`/`NO_PROXY` instead, and is
opt-in rather than the default: `spillway run` points `HTTPS_PROXY` at
spillway itself, so a daemon started from such a shell would proxy through
itself.

## Admin API + web UI

A loopback-only admin listener on `127.0.0.1:7657` — a separate trust class
from the proxy port.

Set `admin.addr` to a filesystem path to serve over a **0600 unix socket**
instead: file permissions become the access control and nothing on the
network can reach it at all.

**No token on loopback.** A token there would only stop processes running as
another user (they can reach the port but not read a 0600 file) while putting a
secret in every URL and breaking your open tab on each restart. The guards that
actually stop browser attacks remain: `Host` validation (defeats DNS
rebinding), no CORS headers (a malicious page can fire requests but not read
the response), `X-Frame-Options: DENY`, and 405 on any non-GET. Binding
`admin.addr` anywhere but loopback makes a token mandatory, generated if you do
not supply one, and fails closed if it is required but missing.

- `/` — dashboard: liquid tanks per quota window, a pin control per account,
  headroom-over-time chart with
  burn-rate projection, activity histogram, exact-figures table, spill events,
  request log
- `GET /api/accounts` — state, quota windows, in-flight, last model served
- `GET /api/requests?limit=N` — recent requests
- `GET /api/quota-history?hours=N` — headroom curves per account/window
- `GET /api/activity?hours=N` — bucketed request counts
- `GET /api/events` — SSE stream of rotation/quota events
- `POST /api/pin` `{"account":"…","force":false}` / `DELETE /api/pin` — pin
  selection to one account, or release it. `409` is a refusal `force` can
  override (would bill, or crosses provider); `400` is not. `GET /api/state`
  reports the current pin.

The request log is SQLite at `~/.config/spillway-requests.db` (0600) and stores
**metadata only** — never headers or bodies. A schema test asserts the exact
column set, so widening that surface has to be deliberate.

## Development

```sh
go build ./... && go vet ./... && go test ./...
```

The dashboard's JavaScript is exercised against a fake DOM by
`internal/admin/testdata/ui_dom_test.js`, run from Go when `node` is available
and skipped when it is not — the repo stays `go build`-only.

## Licence

MIT. See [LICENSE](LICENSE).
