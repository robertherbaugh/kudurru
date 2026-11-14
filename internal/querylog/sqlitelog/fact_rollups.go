package sqlitelog

import (
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// FactMetrics is an already privacy-transformed retained fact. Dictionary IDs
// stay in their original database; no strings are reconstructed or reidentified.
type FactMetrics struct {
	Node, Client, Domain, Upstream, Zone, List int64
	QType                                      uint16
	Status                                     pipeline.Status
	Proto                                      pipeline.Proto
	Rcode                                      int
	LatencyUS                                  *int64
}

// MetricRollup is one hourly contribution in the input database's ID space.
type MetricRollup struct {
	Table     RollupTable
	Node, Key int64
	Values    []int64
}

// SummarizeFacts reuses the local query log's status and histogram definitions.
// Callers bound the input before invoking it; NULL latency never becomes zero.
func SummarizeFacts(facts []FactMetrics) []MetricRollup {
	minutes := map[int64]*minuteAcc{}
	hours := map[int64]*hourAcc{}
	for _, f := range facts {
		m := minutes[f.Node]
		if m == nil {
			m = newMinuteAcc()
			minutes[f.Node] = m
			hours[f.Node] = newHourAcc()
		}
		h := hours[f.Node]
		ev := &querylog.Event{Status: f.Status, Proto: f.Proto, Rcode: f.Rcode, ReplyType: querylog.ReplyNone}
		if f.LatencyUS != nil {
			ev.Latency = time.Duration(*f.LatencyUS) * time.Microsecond
			ev.ReplyType = querylog.ReplyIP
		}
		addBase(&m.base, ev)
		blocked := int64(0)
		if isBlocked(f.Status) {
			blocked = 1
		}
		c := m.clients[f.Client]
		if c == nil {
			c = new([2]int64)
			m.clients[f.Client] = c
		}
		c[0]++
		c[1] += blocked
		m.qtypes[f.QType]++
		if f.Upstream != 0 {
			u := m.upstreams[f.Upstream]
			if u == nil {
				u = new([nUp]int64)
				m.upstreams[f.Upstream] = u
			}
			addUpstream(u, ev)
		}
		addPair(h.domains, f.Domain, 1, blocked)
		if f.List != 0 && blocked != 0 {
			addPair(h.lists, f.List, 1, 0)
		}
		if f.Zone != 0 {
			h.zones[f.Zone]++
		}
	}
	var out []MetricRollup
	for node, m := range minutes {
		for _, t := range RollupTables {
			if t.Res != 3600 {
				continue
			}
			emit := func(k int64, values []int64) {
				out = append(out, MetricRollup{Table: t, Node: node, Key: k, Values: append([]int64(nil), values...)})
			}
			switch t.Key {
			case "":
				emit(0, m.base[:])
			case "client":
				for k, v := range m.clients {
					emit(k, v[:])
				}
			case "qtype":
				for k, v := range m.qtypes {
					emit(int64(k), []int64{v})
				}
			case "upstream":
				for k, v := range m.upstreams {
					emit(k, v[:])
				}
			case "domain":
				for k, v := range hours[node].domains {
					emit(k, v[:])
				}
			case "list":
				for k, v := range hours[node].lists {
					emit(k, []int64{v[0]})
				}
			case "zone":
				for k, v := range hours[node].zones {
					emit(k, []int64{v})
				}
			}
		}
	}
	return out
}
