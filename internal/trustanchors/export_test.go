package trustanchors

import "time"

// SyntheticSample is compiled into this package's test binary only. It lets
// the external rollover tests (rfc5011_rollover_test.go, F28-RFC-2) place
// real authority wires on a 30-day-plus timeline, as the in-package tests do
// with their own fixture clock. Production code cannot construct a Sample.
func SyntheticSample(session string, tick time.Duration, wall time.Time) Sample {
	return Sample{session: session, tick: tick, wall: wall.UTC()}
}
