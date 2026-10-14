package rpz

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/miekg/dns"
)

func (m *Manager) refresh(ctx context.Context, job transferJob) transferResult {
	ctx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()
	r := transferResult{job: job}
	for _, primary := range job.dz.Spec.Secondary.Primaries {
		if err := ctx.Err(); err != nil {
			r.err = err
			return r
		}
		soa, err := m.opts.Receiver.SOA(ctx, job.dz, primary)
		if err != nil {
			r.err = err
			continue
		}
		if soa == nil || dns.CanonicalName(soa.Hdr.Name) != dns.CanonicalName(job.spec.Origin) || soa.Hdr.Class != dns.ClassINET {
			r.err = fmt.Errorf("rpz: primary returned an invalid apex SOA")
			continue
		}
		baseline := job.z != nil && job.data != nil && job.z.AvailableAt(m.opts.Now())
		if baseline && soa.Serial != job.z.soa.Serial && !serialAfter(soa.Serial, job.z.soa.Serial) {
			r.err = fmt.Errorf("rpz: primary serial is older or ambiguous")
			continue
		}
		var records []dns.RR
		usedBase := false
		if baseline && sameSOA(soa, job.z.soa) {
			records, usedBase = job.z.Records(), true
		} else if incremental, ok := m.opts.Receiver.(incrementalReceiver); ok && baseline && soa.Serial != job.z.soa.Serial {
			records, err = incremental.IXFR(ctx, job.dz, primary, job.z.Records())
			usedBase = true
		} else {
			records, err = m.opts.Receiver.AXFR(ctx, job.dz, primary)
		}
		if err == nil && usedBase && !job.z.AvailableAt(m.opts.Now()) {
			// Even an in-flight same-serial poll/IXFR must not renew an expired
			// baseline. Recovery obtains a new complete authenticated transfer.
			records, err = m.opts.Receiver.AXFR(ctx, job.dz, primary)
		}
		if err != nil {
			r.err = err
			continue
		}
		confirmed := m.opts.Now()
		var candidateSOA *dns.SOA
		for _, rr := range records {
			if s, ok := rr.(*dns.SOA); ok {
				candidateSOA = s
				break
			}
		}
		if candidateSOA == nil {
			r.err = fmt.Errorf("rpz: transferred candidate lacks SOA")
			continue
		}
		if baseline && job.z.AvailableAt(confirmed) && candidateSOA.Serial != job.z.soa.Serial && !serialAfter(candidateSOA.Serial, job.z.soa.Serial) {
			r.err = fmt.Errorf("rpz: transferred candidate serial regressed")
			continue
		}
		spec := job.spec
		spec.ExpiresAt = confirmed.Add(time.Duration(candidateSOA.Expire) * time.Second)
		z, err := Compile(spec, records)
		if err != nil {
			r.err = err
			continue
		}
		z.confirmedAt = confirmed
		data := &envelope{Version: 2, Nonce: rand.Text(), ClusterID: m.opts.ClusterID, Identity: spec.Identity,
			Credential: job.credential, ConfirmedAt: confirmed, ExpiresAt: spec.ExpiresAt, Primary: primary}
		if job.data != nil {
			// Two alternating bounded slots retain the accepted last-good copy
			// through failed/canceled writes without accumulating orphan blobs.
			data.Slot = 1 - job.data.Slot
		}
		for _, rr := range z.records {
			data.Records = append(data.Records, rr.String())
		}
		if err := ctx.Err(); err != nil {
			r.err = err
			return r
		}
		r.z, r.data, r.err = z, data, nil
		return r
	}
	if r.err == nil {
		r.err = fmt.Errorf("rpz: no primary supplied usable data")
	}
	return r
}

func serialAfter(a, b uint32) bool { return a != b && a-b < 1<<31 }

func sameSOA(a, b *dns.SOA) bool {
	return dns.CanonicalName(a.Hdr.Name) == dns.CanonicalName(b.Hdr.Name) &&
		a.Hdr.Class == b.Hdr.Class && a.Hdr.Ttl == b.Hdr.Ttl &&
		dns.CanonicalName(a.Ns) == dns.CanonicalName(b.Ns) && dns.CanonicalName(a.Mbox) == dns.CanonicalName(b.Mbox) &&
		a.Serial == b.Serial && a.Refresh == b.Refresh && a.Retry == b.Retry && a.Expire == b.Expire && a.Minttl == b.Minttl
}
