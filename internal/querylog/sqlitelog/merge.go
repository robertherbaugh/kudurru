package sqlitelog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"math/bits"
	"sort"
	"strconv"
	"time"
)

const distinctLimit = 10000
const sketchBits = 14

// Distinct retains exact hashes for small sets and uses HyperLogLog beyond
// 10,000 keys. Only bounded sketches cross the internal listener.
type Distinct struct {
	Values    []uint64 `json:"values,omitempty"`
	Registers []uint8  `json:"registers,omitempty"`
	exact     map[uint64]struct{}
}

func (d *Distinct) add(v uint64) {
	if len(d.Registers) == 0 {
		if d.exact == nil {
			d.exact = make(map[uint64]struct{})
			for _, h := range d.Values {
				d.exact[h] = struct{}{}
			}
			d.Values = nil
		}
		d.exact[v] = struct{}{}
		if len(d.exact) <= distinctLimit {
			return
		}
		d.Registers = make([]uint8, 1<<sketchBits)
		for h := range d.exact {
			d.addRegister(h)
		}
		d.exact = nil
		return
	}
	d.addRegister(v)
}
func (d *Distinct) addRegister(v uint64) {
	i := v >> (64 - sketchBits)
	rank := uint8(min(bits.LeadingZeros64(v<<sketchBits)+1, 65-sketchBits)) //nolint:gosec // rank is at most 51
	d.Registers[i] = max(d.Registers[i], rank)
}
func (d *Distinct) hash(v string) {
	h := sha256.Sum256([]byte(v))
	d.add(binary.BigEndian.Uint64(h[:8]))
}
func (d *Distinct) finish() {
	if d.exact != nil {
		d.Values = make([]uint64, 0, len(d.exact))
		for v := range d.exact {
			d.Values = append(d.Values, v)
		}
		sort.Slice(d.Values, func(i, j int) bool { return d.Values[i] < d.Values[j] })
		d.exact = nil
	}
}

// Merge unions another exact set or sketch.
func (d *Distinct) Merge(other Distinct) {
	for _, v := range other.Values {
		d.add(v)
	}
	for v := range other.exact {
		d.add(v)
	}
	if len(other.Registers) != 1<<sketchBits {
		return
	}
	if len(d.Registers) == 0 {
		d.Registers = make([]uint8, 1<<sketchBits)
		for _, v := range d.Values {
			d.addRegister(v)
		}
		for v := range d.exact {
			d.addRegister(v)
		}
		d.Values = nil
		d.exact = nil
	}
	for i, v := range other.Registers {
		d.Registers[i] = max(d.Registers[i], v)
	}
}

// Count returns the union cardinality and whether it is estimated.
func (d *Distinct) Count() (int64, bool) {
	if len(d.Registers) == 0 {
		if d.exact != nil {
			return int64(len(d.exact)), false
		}
		return int64(len(d.Values)), false
	}
	m := float64(len(d.Registers))
	sum := 0.0
	zeros := 0
	for _, v := range d.Registers {
		sum += math.Ldexp(1, -int(v))
		if v == 0 {
			zeros++
		}
	}
	estimate := (0.7213 / (1 + 1.079/m)) * m * m / sum
	if estimate <= 2.5*m && zeros > 0 {
		estimate = m * math.Log(m/float64(zeros))
	}
	return int64(math.Round(estimate)), true
}

// LatencyMerge carries additive histogram data, rather than averages or
// percentiles which cannot be summed across nodes.
type LatencyMerge struct {
	Sum     int64                 `json:"sum"`
	Count   int64                 `json:"count"`
	Buckets [latencyBuckets]int64 `json:"buckets"`
}

// Add combines latency accumulators.
func (l *LatencyMerge) Add(other LatencyMerge) {
	l.Sum += other.Sum
	l.Count += other.Count
	for i, v := range other.Buckets {
		l.Buckets[i] += v
	}
}

// Summary computes percentiles after merging histograms.
func (l LatencyMerge) Summary() LatencySummary { return latencySummary(l.Sum, l.Count, l.Buckets[:]) }

// SummaryShard is the internal-only summary and the data needed to merge it.
type SummaryShard struct {
	Summary
	ActiveClients Distinct     `json:"active_clients"`
	KnownClients  Distinct     `json:"known_clients"`
	Domains       Distinct     `json:"domains"`
	Histogram     LatencyMerge `json:"histogram"`
}

// UpstreamShard is the internal-only upstream traffic and histograms.
type UpstreamShard struct {
	UpstreamTop
	Histograms    map[string]LatencyMerge `json:"histograms"`
	TotalInWindow int64                   `json:"total_in_window"`
}

func (r *Reader) summaryDistinct(ctx context.Context, tx *rtx, s *snapshot, win Window, out *SummaryShard) error {
	clientExpr := "ip || char(0) || coalesce(name,'')"
	if r.d == dialectPostgres {
		clientExpr = "convert_to(ip,'UTF8') || decode('00','hex') || convert_to(coalesce(name,''),'UTF8')"
	}
	for _, dim := range []struct {
		src  keyedSource
		dict string
		col  string
		dst  *Distinct
	}{
		{r.clientSource(s), "dict_client", clientExpr, &out.ActiveClients},
		{r.domainSource(s), "dict_domain", "name", &out.Domains},
	} {
		rows, _, _, err := r.keyed(ctx, tx, s, dim.src, win.From.Unix(), win.Until.Unix(), 0, "sum(a) > 0", "", 0, false)
		if err != nil {
			return err
		}
		for offset := 0; offset < len(rows); offset += 500 {
			end := min(offset+500, len(rows))
			ids := make([]int64, 0, end-offset)
			for _, row := range rows[offset:end] {
				ids = append(ids, row.k)
			}
			names, err := r.names(dim.dict, dim.col)(ctx, tx, ids)
			if err != nil {
				return err
			}
			for _, name := range names {
				dim.dst.hash(name.Key)
			}
		}
		dim.dst.finish()
	}
	args := []any{}
	// PostgreSQL text cannot contain NUL; retain the byte string as bytea.
	expr := "ip || char(0) || coalesce(name,'')"
	if r.d == dialectPostgres {
		expr = "convert_to(ip,'UTF8') || decode('00','hex') || convert_to(coalesce(name,''),'UTF8')"
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+expr+` FROM dict_client WHERE 1=1`+tx.dictionaryWhere("dict_client", "dict_client.", &args), args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		out.KnownClients.hash(name)
	}
	out.KnownClients.finish()
	return rows.Err()
}

func candidateN(n int, candidates bool) int {
	if candidates {
		if n <= 0 {
			n = DefaultTopN
		}
		return min(n, MaxTopN*4)
	}
	return topN(n)
}

// RowCursor is the per-node position of a row in an ID or timestamp page.
func RowCursor(row QueryRow, timeOrder bool) string {
	v := strconv.FormatUint(row.ID, 10)
	if timeOrder {
		return strconv.FormatInt(row.TS.UnixMicro(), 10) + ":" + v
	}
	return v
}

// StepSeconds chooses a common UTC-aligned bucket width for all peers.
func StepSeconds(step time.Duration, win Window, now time.Time) int64 {
	return stepSeconds(step, win, now)
}
