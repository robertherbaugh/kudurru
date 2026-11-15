package dnsdist

import "testing"

func TestZoneSerialHistoryAcrossPolls(t *testing.T) {
	history := zoneSerialHistory{}
	flush := map[string]bool{}
	observe := func(address string, next map[string]uint32) {
		for _, name := range history.observe(address, next) {
			flush[name] = true
		}
	}
	observe("first", map[string]uint32{"same.test.": 7, "gone.test.": 8})
	if len(flush) != 0 {
		t.Fatal("initial empty history fabricated a change")
	}
	observe("second", map[string]uint32{"same.test.": 90})
	if len(flush) != 0 {
		t.Fatal("backend observations were merged")
	}
	observe("first", map[string]uint32{"same.test.": 7, "new.test.": 11})
	if len(flush) != 0 {
		t.Fatal("unchanged/new zones fabricated a change")
	}
	if _, ok := history["first"]["gone.test."]; ok {
		t.Fatal("removed zone retained")
	}
	observe("first", map[string]uint32{"same.test.": 8})
	if !flush["same.test."] || len(flush) != 1 {
		t.Fatal("later poll lost previous serial")
	}
	unsupported := map[string]bool{"first": true, "second": true}
	history.retain(map[string]bool{"first": true}, unsupported)
	if len(history) != 1 || history["first"]["same.test."] != 8 || unsupported["second"] || !unsupported["first"] {
		t.Fatal("retired backend cleanup changed live history")
	}
	clear(flush)
	observe("second", map[string]uint32{"same.test.": 91})
	if len(flush) != 0 {
		t.Fatal("retired/readded backend borrowed old history")
	}
}
