//go:build race

package main

// raceEnabled relaxes timing assertions under the race detector.
const raceEnabled = true
