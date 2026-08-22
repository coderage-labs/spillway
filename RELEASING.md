# Releasing

Releases are cut by [release-please](https://github.com/googleapis/release-please),
driven entirely by commit messages.

## The loop

1. Land work on `main` with a [Conventional Commit](https://www.conventionalcommits.org)
   subject: `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `build:`, `ci:`, `perf:`.
2. release-please opens (or updates) a PR titled `chore(main): release X.Y.Z`
   carrying the version bump and the `CHANGELOG.md` entry.
3. Merging that PR is the decision to ship. It creates the tag and the GitHub
   Release, then builds and attaches the binaries.

The release PR is the only gate — nothing publishes until it is merged, and it
accumulates while you keep committing.

## Commit messages matter more than usual here

The subject line becomes the changelog entry verbatim, so write it for someone
who was not there. The body is not published: put the reasoning there and the
change in the subject.

**A commit whose type release-please does not recognise is dropped from the
changelog silently.** `spillway:`, `overage:`, `pool:` are not types — they
look like scopes but the parser reads the word before the colon as the type.
Use `feat(pool):` if you want a scope. Two historical prefixes (`pool`,
`statusline`) are mapped explicitly in `release-please-config.json` so the
existing commits survive; do not add more.

Breaking changes: `feat!:` or a `BREAKING CHANGE:` footer. Pre-1.0 these bump
the minor, not the major (`bump-minor-pre-major`), so reaching 1.0.0 stays a
deliberate act rather than a side effect of the first breaking change.

## Two GitHub restrictions this works around

Both come from the same loop-prevention rule — things done by `GITHUB_TOKEN`
do not trigger further automation — and both fail quietly rather than loudly.

- **A tag pushed by release-please does not fire `on: push: tags`.** The build
  is therefore gated on release-please's job output, not on the tag. Wiring it
  to the tag yields a Release with notes and no binaries.
- **The release PR is authored by `github-actions[bot]`, so its CI run is held
  for manual approval.** CI skips that PR by path, since it only edits
  `CHANGELOG.md` and the manifest. The release workflow re-runs the full gate
  at the released commit regardless.

## Artefacts

Six per release: `darwin`/`linux` × `arm64`/`amd64` as `.tar.gz`, and
`windows` × `arm64`/`amd64` as `.zip` — Explorer opens zip natively and needs
a third-party tool for tarballs. All cross-compiled on the Linux runner;
everything is pure Go, so `CGO_ENABLED=0` is asserted rather than hoped for.

## Escape hatch

`workflow_dispatch` on the Release workflow rebuilds and re-uploads assets for
an existing tag, for when a build failed but the release itself is fine.
