# spillway plugin

Slash commands that talk to the local spillway daemon: how the pool is doing,
and which account it should be using.

```
/spillway:status         # pool status
/spillway:status arena   # focus on one account

/spillway:switch         # what is pinned, and what else there is
/spillway:switch arena   # pin the pool to that account
/spillway:switch auto    # back to automatic selection
```

Why commands rather than a notification: the status line and desktop
notifications only exist on the machine running the daemon, and so does the
dashboard. A slash command runs inside the session, so it reaches a session
driven from the phone over Remote Control — which is exactly when you cannot
see the machine.

`status` is read-only. `switch` writes one thing: which account selection is
directed at, live, until a restart or `/spillway:switch auto`. Neither
touches credentials.

`switch` will not spend money or move a session to another provider without
being asked — spillway refuses both, and the command reports the refusal for
you to decide on rather than forcing past it.

## Install

```sh
/plugin install spillway@<marketplace>     # once published
```

Locally, point Claude Code at this directory as a plugin source.
