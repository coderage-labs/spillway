---
description: "Pool status from the local spillway daemon — headroom, rotation, holds, and anything being billed."
allowed-tools: ["Bash"]
---

# status

Report the state of the local account pool. Works over Remote Control, which
is the point: the status line and desktop notifications only exist on the
machine, so this is the only channel that reaches a session being driven from
a phone.

Optional focus from the user: $ARGUMENTS
(e.g. an account name, or "requests" to lean on recent traffic. Ignore if empty.)

## Gather

Read the daemon rather than recalling anything.

```bash
spillway status --json
```

One command, because the admin listener's address lives in the config and may
be a unix socket, and may require a token — `spillway status` already knows
all three, and curling a guessed port does not. It prints `state` (the
pool-level view), `accounts`, and the last 20 `requests`.

If it fails, the daemon is not running. Say so and stop — `spillway service
install`, or `spillway server` in a terminal. Do not guess at pool state from
anything else.

## Report

Lead with the single fact that matters, then the detail. Keep it to a few
lines; this is a status check, not a report.

- **Can I work right now?** `usable` accounts, and the headroom of the one
  that will serve next. `quotaWindows` carries `used`/`limit` per window —
  report what is LEFT, matching the status line and dashboard.
- **Is anything wrong?** In descending order of importance:
  - `holding` — requests parked; say until when. This is why a session looks
    hung.
  - `overage` / an account with `paid: true` — requests are being **billed**.
    Say so plainly and give `overageUsed` if present.
  - `disabled` — a dead credential; needs `spillway login claude <name>`.
  - `parked` — deliberately out of rotation.
  - `reserve` / `exhausted` — spent, with the soonest `resetAt`.
- **What has been happening?** Only if it adds something: rotations or
  `overage` events in the recent requests, and which account is serving.

Do not repeat the raw JSON. If everything is healthy, say so in one line.
