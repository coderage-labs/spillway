//go:build race

package reqlog

// Under the race detector, modernc.org/sqlite is instrumented too and every
// row costs roughly 15x: seeding and pruning issue #162's full 584,114-row
// fixture took 139s under `go test -race` against 6s without it, which on
// its own doubled the time for `go test -race ./...` across the whole
// repository.
//
// 150,000 rows is the same test at a size that still cannot be confused
// with a toy fixture — SQLite's planner has real statistics to work from,
// the newest-first query still has six figures of rows it must not sort,
// and a 14-day cutoff over 17 days of history still deletes tens of
// thousands — while costing seconds rather than minutes. The plan
// assertions, which are what actually pin the fix, are size-independent and
// identical in both builds.
const liveFixtureRows = 150000

const liveFixtureLabel = "150,000 (reduced under -race)"
