//go:build !race

package activity_test

// raceSlowdown scales the search timing bounds. Without the race detector the
// measured cost is the real one, so it is 1.
const raceSlowdown = 1
