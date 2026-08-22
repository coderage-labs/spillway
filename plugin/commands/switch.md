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
(an account, `auto`/`off` to stop pinning, or empty — see below.)

## Resolve

Read the daemon rather than recalling anything.

```bash
spillway status --json
```

`state.pinned` names the account selection is currently directed at, and is
absent when nothing is pinned. `accounts[]` gives you `name`, `label`,
`state`, `paid` and `overThreshold`.

**With no argument**, that is the whole job. Say what is pinned (or that
selection is automatic), then list what you could switch to — label first,
since that is what the user will type back at you — marking anything that
would cost money (`paid: true`) or is already spent (`overThreshold`,
`state` of `exhausted`/`disabled`/`parked`). Do not run the switch. Stop
there.

**With an argument**, turn it into a real account name before doing anything
else. The pin is matched on `name` exactly — `ckitch@arenaentertainment.com`,
not `arena` — so a label or a prefix has to be resolved here or the daemon
just says no account by that name. Match, in order: an exact `name`; an exact
`label`; then a unique case-insensitive prefix or substring of either. If two
accounts match, ask which one and list them. Never pick for the user on a
coin toss — pinning the wrong account is a request served by the wrong
subscription, which is the thing they were trying to avoid.

If it fails, the daemon is not running. Say so and stop — `spillway service
install`, or `spillway server` in a terminal.

## Switch

One command, because the admin listener's address lives in the config and may
be a unix socket, and may require a token — `spillway switch` already knows
all three, and curling a guessed port does not.

```bash
spillway switch <resolved-name>
```

To stop pinning, whatever the user typed (`auto`, `off`, `none`, "back to
normal"):

```bash
spillway switch --auto
```

Use `--auto`, the flag. A bare `auto` is read as an account name and you will
get "no account named \"auto\"" back.

Mention once, not twice: the prompt cache is per account, so the next request
will miss it. That is the cost of switching and it is the only reason the pool
is sticky in the first place.

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
spillway switch <resolved-name> --force
```

Three other refusals have no `--force` and retrying is pointless: no account
by that name (re-resolve, or list what exists), the account is `parked`, and
the account is `disabled` — a dead credential, which needs `spillway login
claude <name>`.

## Report

Prose and short lines only — **no tables**. This is read on a phone as often
as not, and a table wider than the screen silently clips its last column,
which is where the warnings end up.

Lead with what is now true: pinned to which account, or back to automatic
selection. Then, briefly, anything the user needs to know — the cache miss,
or a headroom warning if the account you pinned is near its limit. Two or
three lines. Close by noting the pin clears with `/spillway:switch auto` and
on a daemon restart.
