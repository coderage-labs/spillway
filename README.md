<p align="center">
  <img src="internal/admin/static/logo.svg" alt="" width="104" height="104">
</p>

<h1 align="center">spillway</h1>

<p align="center">
  Pool your own AI subscription accounts behind one local proxy, so a session doesn't stop at a 429.
</p>

<p align="center">
  <img src="docs/images/dashboard-healthy.png" alt="spillway admin dashboard: four pooled accounts across Claude and Kimi, one pinned, one permitted paid extra usage, and a headroom-over-time chart with a burn-rate projection" width="720">
</p>

Local, single-user daemon that proxies official AI CLIs (Claude Code, and any
Anthropic-shaped endpoint) and rotates requests across a pool of **your own**
subscription accounts, so a session doesn't stop at a 429.

It is a **proxy, never a client**: the vendor's own CLI stays in the loop, and
spillway forwards its requests byte-faithfully apart from four request
mutations (auth header, `account_uuid`, and the model when mapping across
providers — both where Claude Code puts it: the top-level executor model, and
an advisor's model nested inside `tools[]`). That fidelity is what keeps
usage inside your subscription rather than falling through to metered API
billing. Responses are untouched by default; one opt-in setting,
`pool.hideOverageFromClient`, removes Anthropic's credit markers from pooled
responses — see "Hiding credit signals" below for why a pool wants that and
what it trades away.

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
| `spillway accounts [remove <account>]` | List accounts, or remove one — `<account>` must be its exact name or exact label; a partial match is refused rather than guessed, since the wrong match deletes the wrong credential. Removal reaches a running daemon immediately — the account stops being selectable before this command returns, not at the next restart |
| `spillway accounts overage <account> on\|off\|default` | Allow or forbid pay-as-you-go past quota for one account — resolved by name, label or a unique prefix/substring — see [Extra usage](#extra-usage). Applies to a running daemon immediately, in both directions |
| `spillway accounts priority <account> <n>` | Order selection for one account — resolved by name, label or a unique prefix/substring; lower is preferred. Applies to a running daemon immediately |
| `spillway notify set <channel>` | Prompt for a provider and its credential, and which events it should fire on; writes the credential to the secret store and the metadata to config — see [Notifications](#notifications) |
| `spillway notify test <channel>` | Send a real notification through one channel, synchronously — the honest way to check it's wired up |
| `spillway notify list` | Configured channels, their events, and whether a credential is present |
| `spillway notify remove <channel>` | Delete a channel's config entry and its stored credential |
| `spillway switch [<account>\|--auto] [--force]` | Point the pool at one account — resolved by name, label or a unique prefix/substring — until told otherwise; with no argument, reports what's pinned and what you could switch to |
| `spillway login claude <account>` | Add a Claude account (OAuth PKCE), or re-authenticate an existing one — `<account>` resolves against existing accounts by name, label or a unique prefix/substring first, and only becomes a new account's name if nothing matches. Reaches a running daemon immediately: a new account is selectable before the command returns, and re-authenticating an existing one hot-swaps its credential in the running pool rather than leaving the daemon holding the stale one until restarted |
| `spillway login kimi <account>` | Add a Kimi account (OAuth device flow), or re-authenticate an existing one — same resolution and live-apply behaviour as `login claude` |
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

`/spillway:switch <account>` pins the pool from the same place — the CLI
itself resolves a label or a unique prefix to the account name, and reports
what would happen rather than forcing past a refusal that costs money.
`/spillway:switch` with no argument reports what is pinned, or that
selection is automatic, and what you could switch to instead.

Both exist for Remote Control. The status line and the desktop notification
live on the machine, so a session driven from a phone has no way to see or
change any of this; a slash command is the one channel that travels.

## MITM mode

The proxy listener also answers CONNECT: hosts of configured upstreams are
terminated with a leaf minted for that exact host, and fed into the same pool
pipeline; every other host is a blind TCP relay, so only configured vendor
hosts are ever decrypted. Upstream TLS is always fully verified.

**There is no CA private key at rest, anywhere — not in the OS keychain, not
on disk (issue #69).** Earlier designs kept the key around so leaves could be
minted on demand as new hosts appeared: first only in the keychain, then
(briefly) on disk next to the cert. Both still had a key that had to be read
from somewhere before anything could be signed, and a keychain read that
failed ambiguously during a routine `brew upgrade` was indistinguishable from
"no CA yet" — which either silently destroyed a working CA (the original
bug) or, once that was fixed to fail loudly instead, left a daemon that would
not start (still a live session stranded, just louder about it).

On-demand signing turns out not to be needed: the full set of hosts spillway
will ever terminate CONNECT for — the global upstream, every registered
provider's default upstream (`api.anthropic.com`, `api.kimi.com` — issue
#87), and every configured account's upstream override — is known before the
first request. Pre-minting a leaf for every *provider*, not only the hosts of
accounts configured that day, is what lets `spillway login` on a brand-new
provider (say, the first Kimi account in a Claude-only pool) reach a running
daemon immediately: its leaf is already there, so adding the account never
needs a chain regeneration. `login`, `accounts remove`, and `accounts
priority`/`overage` all now apply to the running pool immediately; see
[Commands](#commands) and [Settings](#settings). The one case that still needs
a restart is a *custom* `upstream:` host that is no provider's default —
`spillway login`/`accounts add` will say so plainly rather than silently
regenerating the chain (which would strand every other running proxied CLI
— exactly the failure mode the rest of this section exists to avoid). So at
startup spillway
generates the CA, mints a leaf for every one of those hosts up front, writes
the CA cert (`spillway-ca.pem`) and the leaf certs+keys
(`spillway-ca-leaves.json`, 0600 in the existing 0700 directory — only the
leaf keys inside it are secret, but one file is simpler than two) to disk,
and lets the CA private key fall out of scope. There is nothing left to fail
to read on the next start, and no ambiguous keychain or disk error to
mishandle, because the thing that used to be read no longer exists.

A restart with an unchanged host set — the ordinary case, including the
`brew upgrade` scenario that caused the original outage, and now also the
ordinary case of adding an account for a provider spillway already knows
about (issue #87) — reuses the stored CA cert and every leaf byte-for-byte:
nothing a client already trusts changes. Only a host set that actually grew
— a custom `upstream:` override naming a host no provider already covers —
forces a full regeneration, which strands running clients exactly like any
CA replacement — accepted, because it only follows a deliberate change that
already needs a restart, never a plain upgrade or an ordinary account add.
An install from before #69
migrates the same way: it has an old pem but no leaf manifest, which is
handled as ordinary regeneration rather than trying to read the now-orphaned
keychain key forward — reading it would have to succeed at the moment of the
very upgrade that installs this fix, i.e. exactly the situation the original
incident occurred in, so it would keep the dependency this issue removes,
just one release later. That old keychain entry is left in place, never read
again.

Identity-bound paths — `/v1/oauth/token`, `/v1/code/*`, `/v1/environments/*`,
`/v1/sessions/*`, `/api/oauth/files/*`, `/api/oauth/file_upload`, and WebSocket
upgrades (`/v1/session_ingress/ws*`) — relay with the client's own credential
verbatim: no injection, no pool, no rewrite. That is what keeps Remote Control
and the CLI's own token refresh working through the proxy.

**Confirmed non-quota paths get the same treatment (issue #91).**
`/api/event_logging/v2/batch`, `/api/claude_code/settings`, and
`/api/claude_code/policy_limits` are telemetry/settings/limits lookups that
consume no quota and need no pooled account — real traffic showed them
being selected against a real account (misattributing them in the request
log) and, once the pool went dry, held for up to 53 minutes waiting on
quota they never needed, turning a two-account exhaustion into a 51-request
queue all due to fire at once on the next reset. They now relay with the
client's own credential exactly like an identity-bound path: no injection,
no pool, no hold. Only `POST /v1/messages` is confirmed to need a pooled
account at all — every other path (an unclassified one seen in the same
traffic but not confirmed either way, e.g. `/mcp-registry/v0/servers` or
`/latest/api/token`) still gets pool selection and a pooled credential, on
the theory that wrongly bypassing a path that does need one is worse than a
pointless wait, but can never hold on exhaustion: it fails fast with the
same 429 a hold would eventually reach, just without the wait.

**If the CA is regenerated, restart every proxied CLI.** Because
`NODE_EXTRA_CA_CERTS` is read once at process start, a client launched before
the CA changed can never be made to trust the new one — reconnecting doesn't
help, and the failures look like plain network flakiness (`tls: bad record
MAC`, `EOF`, connection resets) rather than a trust-anchor change. spillway
logs a WARN naming the reason and the new CA's fingerprint whenever this
happens, and says explicitly when a restart is required.

**A running session is warned directly, not just logged at (issue #66).**
The trigger left after #69/#70 is narrow — only a deliberate config change
that adds a new upstream host forces a regeneration; a plain restart with an
unchanged host set reuses the stored chain untouched. But that one case
still strands whatever else is running, and knowing you just ran
`spillway login` is not the same as realising three other sessions are now
silently broken. So:

- One desktop notification fires the moment a genuine regeneration happens
  (never on the ordinary restart-reuses-the-chain path), saying other
  sessions may need restarting.
- The daemon then watches for the actual symptom, not just the fact of the
  regeneration: the same MITM handshake failure (host + reason, with the
  client's ephemeral port stripped out) recurring within about two minutes.
  A single handshake failure is ordinary churn — a client walking away
  mid-handshake, see the CONNECT-handling comment above — so it never counts
  alone, and a failure has to actually recur to look like a client stuck
  retrying against an anchor that will never verify again. Failures before
  the regeneration never count either.
- While that symptom is live, `/api/state` reports `staleCA: true` and
  `spillway statusline` shows `⚠ stale CA — restart this session` — scoped
  to a proxied session exactly like every other statusline signal, and
  never fabricated into a proxied API response (§4).
- The warning decays on its own about 15 minutes after the last recurring
  failure. Once every affected session has actually restarted, the failures
  stop, and a warning that stayed on forever after a single regeneration
  would just be a different false positive.

**A stored leaf chain that can't be read as a store entry (corrupt file, disk
error) is never treated as "no CA yet"**: spillway leaves the existing pem
and manifest untouched and fails loudly instead (`server` degrades to
base-URL mode, logging the failure; `run` refuses to launch the CLI).
Silently minting a replacement on an ambiguous read error is exactly what
caused the original outage.

### Non-Node subprocesses (issue #64)

`run` only ever taught **Node** to trust spillway's CA, via
`NODE_EXTRA_CA_CERTS`. But every subprocess of the spawned CLI inherits
`HTTPS_PROXY`/`HTTP_PROXY` too — MCP servers included — and a non-Node one
(a Python MCP server under `uv`, most concretely) that happens to call a
MITM'd host (`api.anthropic.com`) has no way to trust that CA: Python's
`ssl` reads `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`, curl reads
`CURL_CA_BUNDLE`, and neither knows anything about
`NODE_EXTRA_CA_CERTS`. Measured live: one such client retried the same
failing request roughly 7 times a minute, indefinitely, producing 11,665
log lines from a single MCP server over one afternoon.

`run` now also writes a combined CA bundle — this machine's system root
certificates concatenated with spillway's own CA cert — to
`spillway-ca-bundle.pem` beside `spillway-ca.pem`, and points
`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and `CURL_CA_BUNDLE` at it.
`NODE_EXTRA_CA_CERTS` is untouched — Node keeps trusting the plain CA cert,
not the bundle. Unlike the CA cert and leaf manifest, this file holds only
public certificates, so it is world-readable (0644), not 0600.

**The bundle is only ever written when the system roots can be confidently
obtained.** `SSL_CERT_FILE` and friends *replace* a client's trust store
rather than extend it — a bundle missing the system roots would turn
today's narrow failure (only MITM'd hosts break) into every ordinary site
failing to verify for anything that reads these variables. Go gives no
portable way to read the system roots back out as PEM
(`x509.SystemCertPool()`'s `CertPool` cannot be enumerated), so this is
platform-specific: macOS shells out to `security find-certificate`, the
same mechanism other tools use to reach the Keychain's root store; Linux
reads the first present well-known system bundle file (`/etc/ssl/certs/ca-
certificates.crt` and similar, matching each distro's convention). Windows
has no equivalent well-known bundle file and its own certificate store
mostly bypasses these variables anyway, so it is deliberately left
unimplemented. Whenever the roots can't be found — including this
unimplemented-platform case — **nothing is written and none of the three
variables are set**, silently, logged once at WARN: leaving those
subprocesses exactly as they were before this fix (some things fail loudly
against a MITM'd host) is strictly better than a bundle that breaks
everything else.

**This changes what an MCP server's own calls to a MITM'd host actually
do.** Today they fail outright. Once this bundle exists, they succeed —
which means they get rotated across your account pool and **consume your
subscription quota**, silently, for inference the CLI never asked for. This
is not gated or policed: spillway intercepts the hosts it is configured to
intercept and pools whatever reaches them, same as always. To make such a
request visible after the fact, the request log and the `request` log line
now also carry the client's own `User-Agent` header verbatim — the CLI's
own is distinctive (`claude-cli/x.y.z (external, cli)`); most other HTTP
clients (`python-requests`, `urllib`, curl's own) are not, so this is a
hint to look at when something seems off, never something to gate or route
on.

The "mitm connection failed" warning (logged when a terminated TLS
connection errors out — a client that doesn't trust the leaf, a client that
walked away mid-handshake) is now rate-limited per host and failure detail:
the first occurrence always logs, repeats within a one-minute window are
folded into the next line's `suppressed_repeats` count instead of one line
each. This exists independently of the bundle above — it is what let the
11,665-line loop bury an unrelated Remote Control failure in noise for
hours, and a client stuck in some other permanent failure mode would do the
same regardless of what the bundle does or doesn't fix.

The failure detail this dedupes on has the client's ephemeral source port
stripped out first — Go's http.Server prepends `http: TLS handshake error
from <client-addr>: ` to every one of these, and a fresh CONNECT is a fresh
TCP connection with a fresh port every single time. Without stripping it,
two occurrences of the exact same underlying failure against the same host
never actually compare equal, which would have silently limited the dedup
above to a coincidence that never happens in practice — and separately, it
is the one piece issue #66's stale-CA detector needs to recognise the same
failure recurring.

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
  stickyAcrossFamily: false # see "Per-family quota" below
  hideOverageFromClient: false # see "Hiding credit signals" below
log:
  level: info
watchConfig: true         # reload this file when anything else edits it — see below
notify:
  channels:                # optional; empty/absent = local desktop notifications only
    - name: phone
      provider: ntfy        # os | webhook | ntfy | pushover
      events: [exhausted, held, account-disabled]
    - name: desktop
      provider: os
      events: [overage-cap]
accounts:
  - name: you@example.com
    label: work             # what the dashboard and status line show
    type: claude-oauth       # written by `spillway login claude you@example.com`
    priority: 0             # lower is preferred; ties break on load
  - name: kimi
    type: kimi-oauth
    upstream: https://api.kimi.com/coding
    modelMap:
      claude-haiku-4-5-20251001: kimi-for-coding   # exact match wins
      claude-*: k3                                  # glob; longest pattern wins
```

### Editing the config while spillway is running

**A running daemon watches this file.** Change it with a text editor, a
script, or a dotfile synced from another machine, and spillway picks the
change up on its own — the same way its own CLI commands and the dashboard
already did. A restart is a last resort, not the normal way to change
something.

What is applied is checked first: the file is only read once it has stopped
changing (editors write in bursts, and spillway's own writes replace the
file wholesale), and it must parse **and** validate before anything happens.
A broken or half-written config leaves the running configuration exactly as
it was and logs why. Nothing is re-applied when the file is rewritten with
the same content — including spillway's own rewrites, and including a
reformat that changes only whitespace, comments or key order.

Every reload logs one line saying what it applied and what it could not:

| Change | Applies live? |
|---|---|
| `pool.switchThreshold`, `crossProvider`, `allowOverage`, `stickyAcrossFamily`, `hideOverageFromClient` | **yes** |
| An account's `label`, `priority`, `disabled`, `allowOverage` | **yes** |
| Removing an account | **yes** — out of rotation immediately, before it can be selected again |
| Adding an account (its credential already in the keychain) | **yes**, unless it names an `upstream` host spillway has no MITM leaf for — see below |
| `notify.channels` | **yes** — a new channel starts firing, a removed one stops |
| `log.level` | **yes** |
| `upstream`, `proxy.*`, `admin.*`, `egress.*` | no — listeners and the proxy handler are built at startup |
| `pool.exhaustedMode`, `holdMax`, `maxBufferBytes`, `probeOnStart`, `probeInterval`, `canaryInterval` | no — snapshotted at startup |
| An existing account's `type`, `upstream`, `source` or `modelMap` | no |

**The one refusal.** An account whose `upstream` names a host spillway has
no certificate for cannot be added to a running daemon: covering a new host
means regenerating the whole MITM chain, which strands every CLI already
running against the old one. Rather than do that silently with nobody at a
terminal, the reload **refuses that account**, keeps serving the accounts it
already has, and logs what it refused and that a restart is needed. Every
provider's normal upstream is covered from startup, so this only affects a
genuinely custom host. (`spillway login`, where a person is reading the
reply, still adds such an account and tells you the caveat.)

Set `watchConfig: false` to turn watching off; the daemon then picks the
file up only at startup, or when a `spillway` command or the dashboard tells
it to. Turning it off takes effect on the running daemon; turning it back on
needs a restart.

Credentials are not part of any of this. A reload reads names, providers,
events and flags from the yaml and every secret from the keychain, writes no
secret to the file, and logs no destination, topic or token.

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
processes refreshing the same account will invalidate each other. Every
pooled account should be added with `spillway login claude <name>` (or
`spillway login kimi <name>`) — that gives it its own OAuth grant, held in
the keychain under spillway's own service, refreshed by the daemon and by
nothing else.

- A background sweep refreshes any token within 5 minutes of expiry,
  regardless of traffic — an idle account has nothing else to trigger one.
- `spillway login` on an account name that already exists re-authenticates
  it in place: it writes a fresh grant and clears anything that would make
  spillway ignore it, without touching the label, priority, `allowOverage` or
  model map you already set. A running daemon picks up the new grant
  immediately (issue #87) — it no longer keeps serving the old, possibly
  disabled, credential in memory until restarted.

### `source: keychain` is deprecated — do not use it for pooling

An account configured `source: keychain` **borrows** whatever credential the
`claude` CLI itself is currently logged into, instead of holding its own
grant. It is not a lightweight variant of the mode above — it is a different
and structurally broken one:

- spillway reads that keychain item but **never refreshes it**; the `claude`
  CLI owns it and refreshes it on its own schedule.
- It cannot be made to refresh safely, either. Refresh tokens rotate on use,
  so if spillway refreshed it too, the two processes would invalidate each
  other's login — breaking `claude` or being broken by it, unpredictably.
- The result: the account works until the CLI's own token expires, then
  fails permanently. Nothing in spillway can revive it — not a `claude`
  re-login, not time passing — until the daemon is restarted. spillway now
  warns about this loudly at every startup, naming the account and the fix.

**The fix is `spillway login claude <name>` on that same account name.** As
of this fix it also clears `source: keychain` from the config automatically,
so nothing needs a manual yaml edit. If you are on an older config with
`source: keychain` still set by hand, delete that line, or just re-run
`spillway login` — it clears the line for you.

This mode is not being removed outright, only deprecated: existing configs
keep working (with the startup warning) until you re-authenticate. Reading
the `claude` CLI's own login is still useful for *discovering* an account
worth adding — spillway's bootstrap fallback (running with zero accounts
configured) reads it as a convenience for quick, single-account use — but
that is a one-account passthrough, not a way to add an account to the pool.

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

The same map, and the same hard error, also apply to an **advisor's model
nested inside `tools[]`** (design doc §4 mutation #4). Claude Code puts the
executor's model at the top level and an advisor's model as a `model` field
directly on the tool object — `{"tools": [{"type": "advisor_...", "model":
"..."}]}` — and both would otherwise reach the provider unmapped. Nothing
else nested in the body is touched: a `model` string inside a message the
user typed, or a same-named field buried in a tool's own schema, is left
exactly as sent.

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
exhaustion, or when an account crosses `pool.switchThreshold` in the window
that governs the request (predictive rotation — skipped while another
eligible account exists, used anyway when it is the last one). Claude quota
comes from `anthropic-ratelimit-*` response headers; Kimi's from
`/v1/usages` polling.

### Per-family quota (fable)

Anthropic reports separate quota buckets per model family: `5h` and `7d` are
account-wide, and `7d-fable` is an extra weekly bucket that only fable models
draw on. An account whose fable bucket is spent is still fully usable for
Sonnet and Opus, so it is deprioritised **only for a fable request** — the
same "preference, not a ban" rule `switchThreshold` already applies, now
scoped to the family the request actually needs rather than scanning every
window regardless of what was asked. An unrecognised model resolves to the
general (`5h`/`7d`) windows, never to fable — spillway does not guess a
narrower family for a model it cannot identify. Kimi has no such buckets, so
none of this changes its behaviour.

This interacts with stickiness: a session pinned to an account whose fable
bucket is spent will, by default, move to another account with headroom for
a fable request — trading the prompt cache for an account that will not
refuse the request. Set `pool.stickyAcrossFamily: true` to keep the session
on its pinned account instead, eating the possible refusal to keep the cache
warm. Same-family requests, and requests against an account with headroom,
are unaffected either way.

The dashboard and `/api/accounts` reflect the same scoping: `overThreshold`
means the general windows are spent (what actually affects Sonnet/Opus/Haiku
traffic), and a separate `fableSpent` flag names the fable bucket
specifically, so an account can show as healthy and fable-spent at once
without the two meanings colliding.

**A spent reading expires at its own reset.** A window is only evidence until
the moment the provider said it would refill; after that spillway has a
number and no way to know whether it is still true. For the account-wide
windows this hardly matters — the next request on the account re-measures
them — but a spent `7d-fable` reading has no such path out: it only arrives
on a fable response, and being spent is exactly what stops fable being routed
there (issue #135). Until the reset it deprioritises as described above.
Once the reset passes the reading stops counting — for selection, for
`overThreshold` and `fableSpent`, and for the "would this bill" check that
keeps an overage-capable account out of the free tier — and the account is
tried again on the ordinary rules; whatever headers come back replace the
stale figure. The same applies to the row a fable-only 429 forges into the
account's windows, which otherwise outlived the rejection it recorded. A
reading with no reset at all stays spent wherever re-measuring it would be
charged: that is the rule the idle probe uses (never spend uninvited), and
the opposite of what startup seeding does with such a row — the two agree on
failing toward the side that costs nothing; the cost just sits on opposite
sides. Where the probe is free the question does not arise, because the probe
simply asks again.

The provider's reset header can also lag the refill it announces. Measured
live: an account's `7d` fell from 0.89 to 0.0 while its reported reset stayed
put, thirty-one hours ahead — and its `7d-fable`, carrying the very same
reset, sat at 1.0 for those thirty-one hours because nothing routed fable to
it to find out otherwise. So a turnover observed on one window (a reading
that has fallen to near zero from a materially higher one, which within a
cycle can only mean the cycle ended) ends the cycle of every stored window
that shared its reported reset and was not itself re-measured by the same
response. No family knowledge is involved — the provider's own statement
that two windows refill together is the only link — and no reset is guessed:
the retired window's reset becomes the moment the turnover was seen.

Expired windows still appear in `/api/accounts` (flagged `expired: true`),
on the dashboard (a dash and "expired" in place of a level and a refill
countdown) and in `spillway status`; the status line and the headroom
history leave them out.

**A window's age is when the provider measured it, not when spillway last
looked (issue #138).** The 30s sampler records every window into
`quota_samples` for the headroom chart, but a window nobody has re-measured
has nothing new to report each tick — only `Sample.FetchedAt`, carried
through from the window's own `FetchedAt`, says when it was actually last
measured; `ts` is just when that row was written, and exists for pruning.
Startup seeding installs `FetchedAt`, not `ts`, as the window's age, so a
reading that is genuinely days old still looks days old after a restart
instead of as fresh as the last sampler tick — which is what let a
retirement above outlive the daemon that made it: with the sampler once
skipping already-expired windows to avoid extending a stale line, a
retirement's corrected (past) reset had no tick left to reach disk before
the next restart, and the pre-retirement reading — still inside its
original, un-expired reset — came back and re-locked the account out. The
sampler now records every window regardless, so a retirement's correction
is on disk within one tick and survives the next restart same as it would
without one.

**A confirmed 429 is stronger than "over threshold".** The above is all
proactive — a *preference*, built from utilization headers, that still
serves the request when nothing better exists. An actual quota-429 from
upstream is upgraded from a maybe to a certainty, and is scoped the same
way: a fable-only rejection excludes the account from fable selection
entirely (never served for fable again until that window's own reset,
even if it's the only account left — the request falls through to the
usual hold-then-429 path instead) while leaving it fully eligible for
Sonnet/Opus/Haiku, which a fable rejection never touches. A rejection of
`5h` or `7d` still exhausts the whole account as before, and now waits out
only the window(s) that actually rejected the request rather than every
window the account has — a fable-only 429 used to be misread as a
same-account throttle and retried three times, and a 5h-only 429 used to
borrow 7d's far longer reset and sideline the account for up to a week when
it should have cleared in a couple of hours.

**A combined rejection benches to its soonest window, not its longest.**
Anthropic can reject more than one window in the same response (e.g. `5h`
*and* `7d` together). The account-wide deadline is the soonest reset among
whichever windows actually fired, not the longest: measured live, a
combined `5h`+`7d` rejection took the far-off weekly reset and benched an
account for three days, even though its 5h window — the binding
constraint — cleared an hour later and was healthy. If a longer-lived
rejection is still genuinely in force once the shorter one clears, the
worst case is one more 429 costing a rotation, not days of a missing
account. Separately, `exhaustedUntil` is capped at 9 days regardless of
what a reset value claims, so a corrupted or wildly-wrong reset (a bad
epoch parse, a stale org-level cap) can't sentence an account indefinitely
either.

**Exhausted accounts are re-probed, not just waited out.** Nothing else
ever routes real traffic to an exhausted account, so a bench that was
wrong — transient, spurious, or read off a stale figure — used to sit
until its deadline arrived or the daemon restarted (a restart clears
in-memory exhaustion unconditionally, which is how one such case was
actually discovered: the account came back serving immediately, its
weekly window reading 17% remaining, days before its recorded deadline).
The same `probeOnStart`/`probeInterval` machinery that fills idle tanks
now also re-verifies an exhausted account while it stays exhausted: a
healthy re-probe clears the bench immediately, and a re-probe that is
rejected again extends it to the fresh deadline while growing — never
resetting — its own probe backoff, so a genuinely spent account gets
checked less often over time instead of every tick. This never bills
uninvited: the same guard that stops an ordinary idle probe from paying
to re-learn a spent window (see below) applies identically here.

### Hiding credit signals (Claude Code's silent model swap)

Claude Code carries a usage-credit gate for its top model family: when a
response tells it that fable is being served on paid extra usage — the
`overage-in-use` header on a 200, a 429 whose representative-claim names the
fable weekly bucket, or a 429 body carrying `credits_required` — it latches,
silently swaps the session down to the next model (Opus on a Max plan), and
stays there until the CLI restarts or `/model` is run. The latch never lifts
when a quota window resets.

Behind a pool that behaviour inverts its purpose. Those signals describe
**one account**, but the client reads them as the world: measured live, the
overage tier served one fable request from the single billable account at a
moment the whole pool was legitimately fable-dry, the client latched, and
when another account's fable window reset hours later the session kept
asking for Opus indefinitely — rotation never saw another fable request to
act on, so an account with a full fable tank sat idle while the session ran
degraded.

`pool.hideOverageFromClient: true` removes exactly those latch inputs from
pooled Claude responses (and only those: utilization and status headers pass
through, passthrough/identity traffic is never touched, and non-Claude
providers are never rewritten). Spillway's own defence against silent
spending is unaffected — the warning log, the desktop notification, and the
request-log `overage` entry are all written before the strip.

It is off by default because it is a consent decision, not a tuning knob:
with it on, the CLI's own "spend money?" dialog never appears, and
`pool.allowOverage` (off by default, fail-closed, per-account overridable)
becomes the only authority on whether spillway ever serves a billed
request. Turn it on when you run more than one account and would rather
spillway decide when to spend; leave it off if you want the vendor CLI's
own prompt to stay in the loop. An already-latched session is not
recovered — the latch lives in the client process, so one restart of that
session is still needed after enabling this.

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

Like overage, this reaches a running daemon immediately — no restart —
falling back cleanly to "takes effect at the next start" when no daemon is
reachable.

**Pinning overrides all of it.** `spillway switch <account>` directs selection
at one account until `spillway switch --auto`, or until the daemon restarts —
it is a live instruction, not a setting, which is the difference between it and
`priority`. Useful for keeping a piece of work on one subscription, steering off
an account you are about to need elsewhere, or watching rotation without having
to spend a quota to see it.

`<account>` need not be the exact name: it matches, in order, an exact name,
an exact label, then a unique case-insensitive prefix or substring of either.
Ambiguous input lists the candidates and pins nothing — it never guesses.
`spillway switch` with no argument does not pin anything either; it reports
what is currently pinned (or that selection is automatic) and which accounts
you could switch to, marking any that would serve from paid extra usage or
that are parked, disabled or already spent.

A pin survives the rotate-away threshold — naming an account is a statement
that you want it — but not exhaustion: holding every request while healthy
accounts sit idle would be a way to take yourself offline by accident. It is
refused, needing `--force`, if the account would serve from paid extra usage,
or if it would move a live session to another provider (§6.18: the client
configured its capabilities from the first model it saw). Switching costs the
prompt cache, which is per account.

"Would serve from paid extra usage" is asked twice, because a pin skips
selection and the two answers differ. First: would **spillway** choose to
bill here — quota gone and `allowOverage` on. Second: would the **provider**
bill here anyway — quota gone and the provider's own extra usage enabled, in
which case the pinned request is served on it and charged whatever
`allowOverage` says, since nothing about that setting reaches the provider.
Only the second one was missing before, so a pin at a spent account with
`allowOverage` off was accepted silently and the next request billed. Neither
check uses `switchThreshold`: an account merely past it still has free quota
and a pin exists to override it. Both are refused with the same message and
the same `--force`, and an account whose extra usage the provider has
**disabled** pins freely — the request there is refused, not billed. So does
one whose extra usage state is still `unknown`; see [Extra
usage](#extra-usage) for why.

The pin is pool-wide: it is consulted before the sticky map, so it applies to
every session at once. Sticky selection itself is **per session** — the session
key hashes `metadata.user_id`, and Claude Code sends a JSON blob there
carrying `device_id`, `account_uuid` and a per-session `session_id`, so two
windows on one machine get different keys and can sit on different accounts.

(An earlier version of this paragraph said every session on the machine shared
one key. Measured on 2026-08-23: they do not.)

A quota-429 marks the account exhausted until its reset and re-sends the
buffered request on the next account, invisibly to the client. A transient
rate-limit-429 retries the same account with backoff (max 3), never rotates.
An upstream 5xx (529 Overloaded included — Anthropic's explicit "over
capacity" signal, and 500/502/503/504 alike, since none of them say anything
about this account's credentials or quota) gets exactly **one** rotation to
another account: not the account's fault, so it isn't marked exhausted, but
also not chained through the whole pool the way a quota-429 is — if the
upstream is down for everyone, walking every account just serialises that
many failures before the client sees the same 5xx it would have gotten
immediately. If there's no other account to try (or the body wasn't
buffered), the real upstream 5xx reaches the client — never a synthesized
error that would hide the outage. Only `POST /v1/messages` bodies up to 8MB
are buffered for failover; everything else streams straight through, and
failover only happens **before the first response byte** — mid-stream aborts
are the client's own retry behaviour.

A client that goes away mid-request — Escape in Claude Code, an aborted
subagent, a closed terminal — is **dropped, not rotated**. The cancellation
reaches spillway looking like any other pre-first-byte connection failure,
but no account can serve a request whose client is gone, so rotating would
fail identically on every remaining account and blame each one on the way
past. Nothing is written back (there is nobody to write to), no rotation
event is published and no account is named: the request log records it as
`(cancelled)`, and the only trace is a DEBUG line.

When every account is spent, `pool.exhaustedMode` decides: `fail` (pass the 429
through), `hold` (park until the soonest reset, up to `pool.holdMax`), or
`notify` (hold plus a loud log), the default. `spillway run` raises the child's
`API_TIMEOUT_MS` past `holdMax` so the client waits out a hold, never lowering
an existing value. `hold`/`notify` only ever apply to `POST /v1/messages`
(issue #91) — every other pooled path fails fast on exhaustion regardless of
mode; see the confirmed non-quota paths described under [MITM mode](#mitm-mode)
above.

`hold` and `notify` fail fast, rather than parking for the full `holdMax`,
once the soonest known reset would land past the hold deadline anyway — a
spent weekly window with days left on it gets the same 429 a full-length
hold would have reached, just without the wait (issue #55; a 5-hour window
almost always resets inside a 4-hour `holdMax`, so this mainly matters for
7-day and `fable`'s 7-day windows). When nothing exhausted has a known reset
to wait for at all (every blocking account is disabled rather than merely
spent), spillway still holds for the full `holdMax` rather than guessing —
an unknown reset is not the same as a known-far-off one, and treating it
that way would turn a possibly transient gap into an instant, unretryable
error.

A held request also wakes on new pool **capacity**, not only on the clock
(issue #105). Waiting purely for the soonest reset means a pool that gains a
usable account mid-hold is invisible to a request already parked — reported
live: a working account was added while every other account was exhausted,
and the queued requests kept sleeping instead of using it. Adding an account
(live-add, issue #87, or a re-authenticated credential), un-parking one, and
an exhausted account's bench clearing early via re-probe (issue #90 — a
rejected re-probe now un-benches the account before its recorded reset, and
a held request can see that too) all wake every currently-held request
immediately. A wake means "go re-check", not "you're served": selection
runs again, and if it still fails the request goes back to waiting against
its *original* hold deadline — the wake never grants a fresh budget. To
avoid recreating the exact herd issue #91 was filed about (51 requests held
at once), requests woken by the same event stagger their re-selection by a
small, increasing delay (30ms per waiter, capped at 2s) rather than all
re-selecting in the same instant; ordinary quota-header updates never fire
this signal, only an actual transition into potentially-usable capacity.

An idle account reports no quota until something is routed to it, so a standby
tank would sit blank. `probeOnStart` sends one minimal request per account with
no reading, and `probeInterval` re-probes readings that have gone stale — the
same schedule also re-verifies an exhausted account for as long as it stays
exhausted (see "Exhausted accounts are re-probed" above), rather than a
separate mechanism. This is spillway originating traffic rather than proxying
it, so it is deliberately narrow: never for an account that already reported
recently, never fatal on failure, never where the request would be *charged*,
and switchable off.

That last one is a money rule, not a quota rule, and the difference matters
(issue #152). A probe is charged only when the account's own quota is spent
**and** extra usage is permitted for it — by `allowOverage` or its own
override — in which case the provider answers 200 and bills. With extra usage
off, which is the default, the same probe is refused with a 429: free, and
carrying current quota headers. So a spent account that cannot bill is probed
like any other, and the worst case is a refusal that corrects the reading.
Until this was fixed the guard skipped every spent account, which meant the
one reading nothing else can correct — no traffic is routed to a spent
account, and a reading whose reset is still ahead does not expire — was also
the one reading never re-measured. On 2026-09-04 Anthropic reset every user's
quota outside the reset times its own headers had given; three accounts went
on showing spent, one of them for a further five days, and restarting the
daemon did not help because `probeOnStart` asked the same question.

Where a probe *would* be charged the caution stays, but the reported reset is
no longer treated as the only way a window can refill (it demonstrably is
not — see the reset lag above). Such an account is left on its stored reading
for at most a bounded interval — 48 × `probeInterval`, clamped to between 6
and 24 hours — after which one probe is bought: a single small request is a
far smaller cost than days of wrongly-deprioritised capacity. A window whose
measurement time is unknown never triggers that, since an unknown age is not
evidence of an old one and spending against it would be a charge per restart.

A restart is also an escape hatch again. `probeOnStart` re-measures a seeded
reading that says "spent" rather than trusting it, so a user who knows their
quota came back early can restart the daemon and have spillway agree within
seconds — free, unless extra usage is on, in which case the bounded interval
above still governs. Accounts with headroom are still left alone at startup:
their readings are believable and re-measuring them would be traffic nobody
asked for.

Quota windows live in memory, so a restart used to mean every tank went blank
until the next probe or request — and, for an account that was spent with
extra usage permitted, meant the next probe could itself be a charge. The
daemon now seeds each account's last recorded reading from the request log on
startup, provided it is still within its own reset window, so a restart shows
last-known state immediately and only a genuinely never-seen account is
probed unconditionally. The same rule now applies while running — a reading
past its reset stops counting against the account (see "A spent reading
expires at its own reset" above) — and the history sampler stops recording
such a reading, so a restart never finds a stale spent row worth seeding in
the first place.

### Settings

The dashboard can edit an allowlisted subset of the config —
`exhaustedMode`, `holdMax`, `switchThreshold`, `probeOnStart`,
`probeInterval`, `crossProvider`, `stickyAcrossFamily`,
`hideOverageFromClient`, and per-account `label`, `priority` and `disabled`.
`hideOverageFromClient` is editable here, unlike `allowOverage`, because it
cannot cause spend by itself: with `allowOverage` off spillway refuses
rather than bills, stripped markers or not.
Changes validate before they are written and apply to the running pool with no
restart. Credentials are not editable and are not exposed: token material must
not be reachable from a browser, loopback or not.

`disabled` parks an account — kept, with its credential, but out of rotation.
That is deliberately distinct from the disable that means a credential died,
which un-parking never reverses.

`spillway accounts priority`/`overage`/`remove` write the same config file
from the CLI rather than the dashboard's form, but reach a running daemon
through the same mechanism — there is one path into the live pool, not a
dashboard path and a separate CLI path that could disagree about whether a
change took effect. `remove` in particular takes the account out of
selection immediately; a request already in flight on it is left to finish
rather than aborted, the same way `disabled` leaves in-flight work alone.
`login` (issue #87) is the mirror on the way in: a brand-new account is
selectable before the command returns, and re-authenticating an existing one
hot-swaps its credential on the live `*Account` in place — reviving it if it
had gone disabled — rather than the daemon keeping the stale one in memory
until restarted. If no daemon is reachable, all four still succeed — the
config (and secret store) is what a future `spillway server` reads at
startup — and say plainly that nothing applied live rather than claiming it
did.

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

Each of those reaches a running daemon immediately — turning `off` off stops
billing before the command returns, it does not wait for a restart. If no
daemon is reachable the command still succeeds (the config is what a future
`spillway server` reads at startup) and says so plainly instead of implying
it took effect live.

Two things `allowOverage` does **not** do, because both are the provider's
decision and not spillway's. It does not turn the provider's extra usage off:
if your account has it enabled, a request that reaches the provider with no
quota left is served on it and charged. And it does not stop `spillway
switch` being refused — the pin guard asks the provider-side question too, so
naming a spent account whose provider will bill it needs `--force` even with
`allowOverage` off. What `allowOverage` governs is tier 3 of ordinary
selection: whether spillway will ever *route* there on its own.

`unknown` is the one state on that path that does not refuse a pin. It cannot
be resolved without spending a request on the account, so refusing would
leave `--force` as the only way forward and teach it as a reflex — and
`unknown` is the ordinary state of an account that has not been used yet, so
refusing it would block the common case rather than the dangerous one. A
pinned request on a spent account with no reading can be billed once; the
`overage-in-use` header then makes the state known, with the usual `WARN`,
notification and status-line marker, and the refusal applies from then on.

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

## Notifications

Spillway always raises a local desktop notification for the events below —
`osascript` on macOS, `notify-send` on Linux, a toast on Windows. That is
today's exact behaviour and nothing below changes it: **`notify.channels` is
entirely opt-in.**

The reason to add one: a held request has nothing else that can reach you.
HTTP gives one response per request, so a request spillway is currently
holding cannot itself carry a message — the only way to hear about it before
you next look at the screen is an out-of-band channel.

```yaml
notify:
  channels:
    - name: phone
      provider: ntfy
      events: [exhausted, held, account-disabled]
    - name: desktop
      provider: os
      events: [overage-cap]
```

Each channel is a `name`, a `provider`, and the `events` it wants — that's
all that's in config. Everything else about a channel is a credential and
lives in the secret store, never in the yaml:

| Provider | Credential | Notes |
|---|---|---|
| `os` | none | The local desktop notification, as a channel like any other |
| `webhook` | a URL, optionally a bearer token | Generic `POST` — covers Slack, Discord, or anything that takes JSON |
| `ntfy` | a **topic** (the credential itself on ntfy.sh — see below), optionally a self-hosted base URL and a bearer token | [ntfy.sh](https://ntfy.sh) needs no account: install the app, subscribe to your topic |
| `pushover` | an app token and a user key | Sends at emergency priority, repeated until acknowledged — the one thing that reliably wakes someone |

Manage them with:

```sh
spillway notify set phone       # prompts for provider + credential + events
spillway notify test phone      # sends a real notification, synchronously
spillway notify list            # channels, events, whether a credential is present
spillway notify remove phone    # deletes the config entry and its credential
```

Each of these applies to a running daemon immediately — a channel you add
starts firing without a restart, and one you remove stops. (It used not to:
channels were read once at startup, so a newly configured channel stayed
silent until the service was bounced. See "Editing the config while spillway
is running".)

### ntfy topics and webhook URLs are credentials, not configuration

**On ntfy.sh's free tier there is no access control at all.** Anyone who
knows your topic name can not only read your notifications but *publish to
it* — a fake spillway alert on your phone. The topic **is** the whole
security model, so:

- generate it with something like `openssl rand -hex 16`, never a memorable
  name;
- for a reserved topic (paid tier) or a self-hosted server, `spillway notify
  set` also takes an access token (`tk_...`), sent as a bearer
  `Authorization` header — the same mechanism the `webhook` provider uses
  for its own token.

A Slack or Discord webhook URL is exactly as sensitive: possession of the
URL is authorisation to post as that integration. Both are why these values
never appear in `spillway.yaml` — a config file with only `name`/`provider`/
`events` in it is safe to paste into a bug report, which is exactly when
people paste configs.

Because the transport is only as private as that guessed string, the default
messages carry state and timing, never identity — *"All accounts exhausted,
soonest reset 07:00 (in 6h12m)"*, never which account or its email address.
The one exception is `account-disabled`, which names the account's own
label (never an email) because the message is useless otherwise — you can't
re-login an account you can't identify.

### Events

| Event | Fires when |
|---|---|
| `exhausted` | Every account is spent and a request was refused |
| `held` | The first request has been parked waiting for a reset — the leading indicator, before a queue builds |
| `overage-cap` | An account already billing for extra usage has hit its own limit there too |
| `account-disabled` | An account's credential died and it dropped out of rotation |

An unknown event name in `notify.channels` fails config load immediately,
naming the valid set — a typo here would otherwise silently disable an
alert, which is the worst failure mode for a feature whose whole job is
telling you something is wrong.

A channel with no credential yet (or one a broken keychain read couldn't
retrieve) is disabled with a startup warning naming the channel and the
`spillway notify set` command to fix it — the daemon still starts. A channel
that is merely unreachable at send time never delays or fails the request
that triggered it, and never stops another channel from getting the same
notification; a pile-up still produces one notification per channel, not one
per request.

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
- `GET /api/accounts` — state, quota windows (each flagged `expired: true`
  once its reset has passed with no fresh reading since), in-flight, last
  model served
- `GET /api/requests?limit=N` — recent requests
- `GET /api/quota-history?hours=N` — headroom curves per account/window
- `GET /api/activity?hours=N` — bucketed request counts
- `GET /api/prefix-drift` — prompt-cache prefix instability: how often each
  part of the request prefix changed between consecutive requests in a
  session, and the cache-token volume that went with it (see "Prefix
  instability" below)
- `GET /api/events` — SSE stream of rotation/quota events
- `POST /api/pin` `{"account":"…","force":false}` / `DELETE /api/pin` — pin
  selection to one account, or release it. `409` is a refusal `force` can
  override (would bill, or crosses provider); `400` is not. `GET /api/state`
  reports the current pin.
- `GET /api/state`'s `staleCA` is issue #66's stale-CA warning (see "MITM
  mode" above) — true while a genuine CA regeneration looks like it has
  stranded at least one client; the statusline is what actually shows it.

<p align="center">
  <img src="docs/images/dashboard-hold.png" alt="spillway admin dashboard showing a request held: every Claude account is dry, including paid extra usage, while a Kimi account sits healthy because cross-provider rotation is off" width="720">
</p>

The banner above is the state `internal/admin/demo -hold` exists to produce:
every Claude account spent, including side's paid extra usage, with a
request parked rather than failed. Kimi stays healthy in the corner —
cross-provider rotation is off in this config, so its headroom does not
rescue a Claude request.

The request log is SQLite at `~/.config/spillway-requests.db` (0600) and stores
**metadata only** — never headers or bodies, and never any prompt or
completion content. A schema test asserts the exact column set, so widening
that surface has to be deliberate.

One deliberate exception (issue #110): four integers from the response's
`usage` block — `input_tokens`, `output_tokens`, `cache_creation_input_tokens`
and `cache_read_input_tokens` — are read and stored, because spillway's
rotation logic had never measured what a rotation actually costs in prompt
cache. Nothing else in that body is ever parsed — no content block, no tool
input, no stop reason, no text of any kind — and a request's session is
recorded only as a hash, never the raw value (which can be a client IP). The
parser observes response bytes as they stream past without buffering them: a
parse failure, a truncated stream, or an encoded (gzip/br) body it doesn't
decode all just record zero, and never affect the response the client
receives. The dashboard's "exact figures" table shows the resulting cache hit
rate and cache-create/cache-read volume per account, beside burn/h and dry
in.

### Prefix instability

Claude Code's prompt cache keys on a **byte-exact request prefix**. If the
prefix shifts between turns — tools arriving in a different order, a system
block changing, attachment blocks moving — the whole prefix is re-written at
cache-create prices instead of read at cache-read prices, and that lands on
your quota. Rewriting requests to stabilise it has been proposed (issue
#111) on the strength of someone else's numbers; this is the measurement
that says whether any of it is true of *your* traffic.

**No request is mutated and no content is stored.** Per `POST /v1/messages`,
spillway records eight more values alongside the usage counters: truncated
SHA-256 hashes of the ordered tool-name list, the same list sorted, the
tools array's raw bytes, the system block's raw bytes and the first
message's content-block *type* sequence, plus the tool count, block count
and prefix byte length. Hashes and integers only — no prompt text, no tool
description, no tool input, no attachment path, nothing reconstructable.
Parsing runs on the body spillway has already buffered for failover, and a
body it cannot parse simply records empty fingerprints; it can never fail a
request.

The ordered/sorted pair is the point. Ordered changed while sorted held
still means the tool *set* was identical and only its *order* jittered —
the one thing deterministic tool sorting would fix. `GET /api/prefix-drift`
reports, per kind of change and split by whether the account changed
between the two requests (rotation cost versus in-session churn): how many
consecutive-request pairs showed it, and the cache-create and cache-read
volume that accompanied it. `none` is the control group — what a stable
prefix costs anyway. `pairs_missing_usage` is the honesty column: responses
in an encoding the usage sniffer cannot read (issue #126) record no token
counts, and those pairs are counted separately rather than averaged in as
zeros.

Phase 2 — actually rewriting requests — is deliberately not built. §4
permits four mutations and this adds none.

The same database's `quota_samples` table — the headroom history behind the
dashboard's chart and startup quota seeding — is pruned to the last **14
days**, both once at every startup and on an hourly timer thereafter. The
dashboard's own history view tops out at 7 days (`?hours=168`), so this keeps
a full 2x margin past the longest thing that reads it. Nothing pruned this
table before; on one real installation it grew to 57,617 rows (40 MB)
answering a question with 8 possible answers, and the unindexed query behind
that answer is what caused issue #104's startup hang.

## Development

```sh
go build ./... && go vet ./... && go test ./...
```

The dashboard's JavaScript is exercised against a fake DOM by
`internal/admin/testdata/ui_dom_test.js`, run from Go when `node` is available
and skipped when it is not — the repo stays `go build`-only.

## Licence

MIT. See [LICENSE](LICENSE).
