package config

import (
	"strings"
	"testing"
)

func TestDatabaseDSN(t *testing.T) {
	for _, tc := range []struct {
		dsn string
		ok  bool
	}{
		{"", true},
		{"postgres://kudurru:pw@db:5432/kudurru?sslmode=verify-full", true},
		{"postgresql://db/kudurru", true},
		{"host=db user=kudurru password=pw dbname=kudurru", true},
		{"mysql://kudurru:pw@db/kudurru", false},
		{"just-a-word", false},
	} {
		errs := Database{DSN: tc.dsn}.validate()
		if (len(errs) == 0) != tc.ok {
			t.Errorf("%q: %v", tc.dsn, errs)
		}
		for _, e := range errs {
			if strings.Contains(e.Error(), "pw") {
				t.Errorf("%q: error echoes the DSN: %v", tc.dsn, e)
			}
		}
	}
	if (Database{}).Enabled() || !(Database{DSN: "postgres://db/k"}).Enabled() {
		t.Fatal("Enabled")
	}
}
