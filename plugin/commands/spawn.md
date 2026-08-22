---
description: Start a detached Remote Control Claude session on this machine, routed through the spillway pool
---

Start a detached Claude Code session with Remote Control enabled so it can be
driven from the Claude mobile/web app, without opening a terminal.

Arguments: `$ARGUMENTS` — expected as `<name> [cwd] [-- <claude args>]`.

Run the spawn script (adjust the path if spillway lives elsewhere):

```bash
~/Repos/spillway/plugin/spawn-session.sh $ARGUMENTS
```

Then report back to the user:
- the session name and where it will appear (Claude mobile/web app)
- the log path the script printed
- whether it went into tmux (attachable locally) or plain nohup

If the script reports spillway is not listening, tell the user to start it
(`spillway service install`, or `spillway server` in a terminal) — do not try
to launch `claude` directly as a workaround, since that bypasses the account
pool and silently spends one account's quota with no rotation.

The spawned session runs **unpinned** through the pool, so quota rotation
applies to it exactly as to any other session.
