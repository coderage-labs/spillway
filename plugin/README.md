# spillway plugin

A slash command that asks the local spillway daemon how the pool is doing.

```
/spillway:status         # pool status
/spillway:status arena   # focus on one account
```

Why a command rather than a notification: the status line and desktop
notifications only exist on the machine running the daemon. A slash command
runs inside the session, so it reaches a session driven from the phone over
Remote Control — which is exactly when you cannot see the machine.

Read-only. It talks to the loopback admin API and never touches credentials.

## Install

```sh
/plugin install spillway@<marketplace>     # once published
```

Locally, point Claude Code at this directory as a plugin source.
