//go:build !fts5

package main

// Intentional compile failure. This binary is useless without FTS5.
//
// gorm.io/driver/sqlite uses mattn/go-sqlite3, which does not compile FTS5 in
// by default. A tagless build is not merely degraded — it starts up perfectly
// happily and then fails every query touching events_fts with "no such module:
// fts5", so /api/search returns 500 and nothing else complains. That silence is
// why this guard exists: the failure has no other early warning.
//
// Build with:  CGO_ENABLED=1 go build -tags fts5     (see the Makefile)
//
// The spelling is pinned to `fts5`. mattn also accepts `sqlite_fts5`, but
// allowing both would let a build satisfy the driver while missing this file,
// which defeats the guard. Set the tag in CI, in the Makefile, and in your
// editor's gopls build flags — `go test` and `go vet` need it too.
const _ = fts5BuildTagRequired
