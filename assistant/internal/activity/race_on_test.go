//go:build race

package activity_test

// raceSlowdown scales the search timing bounds under the race detector, whose
// instrumentation slows the allocation-heavy SQLite driver by an order of
// magnitude. The scale keeps the assertion meaningful: the window scan this
// issue is about is still slower than the scaled ceiling.
const raceSlowdown = 40

// raceEnabled reports whether this binary carries the race detector.
const raceEnabled = true
