# Contributing

## Flow

Work starts from a GitHub issue. Branch off `main`, one issue per branch, PR
back into `main`. Small, single-purpose PRs review faster and bisect cleanly
when something regresses — this repo's history is full of PRs that each did
one thing.

## Documentation is part of the change, not a follow-up

If your PR touches a user-visible surface — a CLI command or flag, an admin
endpoint, a config field, a plugin command, dashboard behaviour — update the
README in the same PR. If you decide it doesn't need one, say so in the PR
description and why.

This exists because #11 shipped four such surfaces and documented none of
them: the work was split across agents by surface, each briefed only on its
own piece, and nobody was briefed on the README. The gap was found by someone
asking, not by anything failing. Treat "does this need a README update" as
part of finishing the change, the same way you'd treat a missing test.

`TestReadmeCommandsMatchDispatch` (`cmd/spillway/docs_guard_test.go`) enforces
this mechanically for one surface: every subcommand dispatched in
`cmd/spillway/main.go`'s `switch os.Args[1]` must appear in the README's
Commands table, and the table can't list a command that doesn't dispatch. It
covers CLI commands only — admin endpoints, plugin commands and config fields
are still on trust, same as everything else in this section.

## Before you open the PR

```sh
gofmt -l .          # must print nothing
go vet ./...
go test ./... -race -count=1
```

This is what CI runs. Green locally on all three before pushing saves a round
trip.
