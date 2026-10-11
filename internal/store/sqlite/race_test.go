//go:build race

package sqlite

// raceEnabled relaxes timing assertions under the race detector.
const raceEnabled = true
