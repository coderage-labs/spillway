---
description: "Point the pool at one account, or hand selection back to spillway. Reports what is pinned when asked with no argument."
allowed-tools: ["Bash"]
---

# switch

Direct the pool at a named account until told otherwise. Like `status`, this
exists because the dashboard is only reachable from the machine — a session
driven from a phone has no other way to say "stay on the work subscription".

The pin is a live instruction to the running daemon, not a config change: it
applies at once and does not survive a restart. Priority is the setting; this
is the override.

What the user asked for: $ARGUMENTS
(an account, `auto`/`off`/`none` to stop pinning, or empty to see what's
pinned.)

## Switch

One command, because the admin listener's address lives in the config and may
be a unix socket, and may require a token — `spillway switch` already knows
all three, and curling a guessed port does not. It also resolves a label or a
unique prefix to the real account name itself now, and reports what is pinned
when there is nothing to resolve, so there is nothing to do here but run it
and relay what it says.

```bash
spillway switch $ARGUMENTS
```

If it fails because the daemon is not running, say so and stop — `spillway
service install`, or `spillway server` in a terminal.

## When it refuses

Two refusals come back as a non-zero exit with the reason in the message.
Both are recoverable with `--force`. **Do not re-run with `--force` on your
own.** Report the consequence, and wait — `--force` is the user's call, and
each of these is a decision only they can make.

- **"pinning there would spend money"** — that account is out of quota, so
  every request on it would serve from paid extra usage. Say plainly that
  this will cost real money for as long as the pin is in place, and ask.
- **"pinning there changes provider mid-session"** — the sessions running now
  started on one provider and this account is the other. The client
  configured its capabilities from the first model it saw, so it may be
  handed a model it never negotiated. Say that in a line, and ask.

Only after the user has agreed to that specific consequence:

```bash
spillway switch $ARGUMENTS --force
```

Three other refusals have no `--force` and retrying is pointless: no account
matches (the CLI lists what exists — relay it), two accounts match (the CLI
lists both — ask which one, and never pick for the user on a coin toss), and
the account is `parked` or `disabled` — the latter a dead credential, which
needs `spillway login claude <name>`.

## Report

Prose and short lines only — **no tables**. This is read on a phone as often
as not, and a table wider than the screen silently clips its last column,
which is where the warnings end up.

Lead with what is now true: pinned to which account, or back to automatic
selection. Then, briefly, anything the user needs to know — the cache miss,
or a headroom warning if the account you pinned is near its limit. Two or
three lines. Close by noting the pin clears with `/spillway:switch auto` and
on a daemon restart.
