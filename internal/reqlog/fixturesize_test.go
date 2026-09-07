//go:build !race

package reqlog

// liveFixtureRows is issue #162's live database exactly: 584,114 rows in
// `requests`, 17 days of history, 112 MB on disk. This is the size the
// before/after numbers in the PR and README were measured at, and the size
// TestRequestQueriesOnALiveSizedFixture runs at under a plain `go test`.
//
// See fixturesize_race_test.go for why the race build uses fewer.
const liveFixtureRows = 584114

// liveFixtureLabel names the size in failure messages, so a failure from the
// smaller race-build fixture cannot be misread as one at live size.
const liveFixtureLabel = "584,114"
