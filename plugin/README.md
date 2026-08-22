# spillway Claude Code plugin

A `/spawn` slash command that starts detached Remote Control sessions on this
machine, routed through the spillway pool — so you can start parallel work
from the phone without remote-desktoping in.

## Install

Copy or symlink the command into your Claude Code commands directory:

```bash
mkdir -p ~/.claude/commands
ln -sf ~/Repos/spillway/plugin/commands/spawn.md ~/.claude/commands/spawn.md
```

Then in any session: `/spawn fix-tests ~/Repos/myrepo`

## Requirements

- `spillway server` running (or installed via `spillway service install`)
- `tmux` optional but recommended (`brew install tmux`) — lets you attach to a
  spawned session locally; without it the script falls back to `nohup`

## Why server mode

The script uses `claude remote-control` (server mode), not
`--remote-control --resume`: interactive resume mints a replacement session
**without history** when reattach fails, whereas server mode re-serves the same
server-side session within ~4h of shutdown.
