package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// ledgerMountedUpdates is how many signed UPDATEs the mounted ledger test
// commits: more than the ledger holds at once (zonestate.DefaultMaxReceipts),
// so the zone can take them only because commits drop expired receipts.
const ledgerMountedUpdates = zonestate.DefaultMaxReceipts + 76

// ledgerMountedFudge is the signature fudge of the bulk UPDATEs: with the
// one-minute clock margin a receipt is live for about 65 seconds, so the
// bulk stays under the ledger's rate bound on a fast runner too.
const ledgerMountedFudge = 5

// ledgerSigned builds one signed UPDATE of zone ledger.test. that adds
// name's address, optionally only if the name is not in use.
func ledgerSigned(name, address string, fudge uint16, unused bool) *dns.Msg {
	m := new(dns.Msg)
	m.SetUpdate("ledger.test.")
	rr := &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP(address)}
	if unused {
		m.NameNotUsed([]dns.RR{rr})
	} else {
		m.RemoveRRset([]dns.RR{rr})
	}
	m.Insert([]dns.RR{rr})
	m.SetTsig("updates.", dns.HmacSHA256, fudge, time.Now().Unix())
	return m
}

// ledgerExchange sends the same signed message (identical bytes: the same
// message ID, time signed and MAC) until it is answered with anything but
// SERVFAIL or until the deadline, and returns that answer's rcode. The
// client signs a copy: miekg removes the TSIG record from the message it
// signs, so sending m itself again would send it unsigned. An
// UPDATE answered SERVFAIL may or may not have committed; sending the exact
// message again is the client retry that the receipt ledger makes safe.
func ledgerExchange(client *dns.Client, address string, m *dns.Msg, deadline time.Time) (int, error) {
	for {
		answer, _, err := client.Exchange(m.Copy(), address)
		if answer != nil && answer.Rcode != dns.RcodeServerFailure {
			return answer.Rcode, nil
		}
		if !time.Now().Before(deadline) {
			if err == nil {
				err = fmt.Errorf("SERVFAIL until the deadline")
			}
			return -1, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func ledgerReadTimeout(err error) bool {
	var transport *net.OpError
	return errors.As(err, &transport) && transport.Op == "read" && transport.Timeout()
}

// A mounted primary commits more signed RFC 2136 UPDATEs to one zone than
// its receipt ledger holds, sent over DNS with real TSIG signatures (D409).
// Before D409 the ledger was append-only and after the 256th commit every
// UPDATE of the zone answered SERVFAIL "retry ledger full". It also answers
// an exact repeat of a committed UPDATE (the same signed bytes) from the
// retained outcome, NOERROR, instead of deciding it again (the update's own
// "name not in use" prerequisite would refuse it with YXDOMAIN), also after
// a restart, and refuses the exact repeat once its signature has expired
// (TSIG BADTIME), with no second effect.
func TestRunServeSQLiteSignedUpdatesBeyondTheReceiptLedger(t *testing.T) {
	cfg, secret := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.Cache.MaxEntries = 0
	st, closeStore := tsigMountedStore(t, cfg)
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "ledger"}, Spec: public.DNSZoneSpec{ZoneName: "ledger.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.ledger.test."},
			Policy: public.ZonePolicy{Notify: public.NotifyNone, Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "updates", Name: "*.ledger.test.", Types: []string{"A"}}}}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "ledger"}, Endpoints: []public.Endpoint{{DNSName: "seed.ledger.test.", RecordType: "A", Targets: []string{"192.0.2.1"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	closeStore()
	stop := tsigMountedStart(t, cfg)
	awaitMountedDNS(t, cfg, "seed.ledger.test.", "192.0.2.1")
	address := cfg.Listeners.DNS[0].Address
	client := func() *dns.Client {
		return &dns.Client{Net: "tcp", Timeout: 5 * time.Second, TsigSecret: map[string]string{"updates.": secret}}
	}

	// An exact repeat inside the signature window.
	once := ledgerSigned("once.ledger.test.", "192.0.2.10", 60, true)
	signedAt := time.Now()
	if rcode, err := ledgerExchange(client(), address, once, signedAt.Add(30*time.Second)); rcode != dns.RcodeSuccess {
		t.Fatalf("first UPDATE: rcode %d: %v", rcode, err)
	}
	if rcode, err := ledgerExchange(client(), address, once, signedAt.Add(30*time.Second)); rcode != dns.RcodeSuccess {
		t.Fatalf("exact repeat inside its window: rcode %s (%d), want NOERROR from the retained outcome: %v", dns.RcodeToString[rcode], rcode, err)
	}
	if rcode, err := ledgerExchange(client(), address, ledgerSigned("once.ledger.test.", "192.0.2.10", 60, true), time.Now().Add(30*time.Second)); rcode != dns.RcodeYXDomain {
		t.Fatalf("a newly signed request was not decided again: rcode %d: %v", rcode, err)
	}

	// An exact repeat after its signature expired is refused by TSIG.
	short := ledgerSigned("short.ledger.test.", "192.0.2.11", 2, true)
	if rcode, err := ledgerExchange(client(), address, short, time.Now().Add(2*time.Second)); rcode != dns.RcodeSuccess {
		t.Fatalf("short-lived UPDATE: rcode %d: %v", rcode, err)
	}
	// #nosec G115 -- ledgerSigned sets this fixture timestamp from time.Now().Unix().
	time.Sleep(time.Until(time.Unix(int64(short.IsTsig().TimeSigned)+2+1, 0)))
	// An UPDATE's TSIG refusal can be withheld if the captured publication
	// loses currency before the write. Only a missing reply with a read
	// timeout allows another send of the same expired packet, not a new
	// signature. Any received response must still be NOTAUTH/BADTIME.
	answer, _, err := client().Exchange(short.Copy(), address)
	if answer == nil && ledgerReadTimeout(err) {
		t.Logf("expired exact repeat received no TCP answer: %v; sending the same expired packet once more", err)
		answer, _, err = client().Exchange(short.Copy(), address)
	}
	if answer == nil || answer.Rcode != dns.RcodeNotAuth || answer.IsTsig() == nil || answer.IsTsig().Error != dns.RcodeBadTime || (err != nil && !errors.Is(err, dns.ErrAuth)) {
		t.Fatalf("expired exact repeat was not refused with BADTIME: %v (exchange: %v)", answer, err)
	}

	// Commit more UPDATEs than the ledger holds. Each one is a new signed
	// request replacing one of 16 names; a SERVFAIL (an ingress refusal, a
	// commit whose own publication was superseded, or the ledger's rate
	// bound) is followed by a newly signed request for the same change. A
	// concurrent publication can also withhold the original mutation response
	// and close its TCP connection. The client and pipeline both expire after
	// five seconds, so a withheld answer can also reach the client as a read
	// timeout before the close. Retry these ambiguous outcomes under the same
	// deadline too; replacing this RRset is idempotent. Neither refusal counts
	// as success, and every logical change still needs an actual NOERROR.
	const workers = 4
	var next, noerror, servfail, eof, readTimeout, other atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	began := time.Now()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := client()
			for {
				i := next.Add(1)
				if i > ledgerMountedUpdates {
					return
				}
				name := fmt.Sprintf("u%d.ledger.test.", i%16)
				target := fmt.Sprintf("192.0.2.%d", 20+i%200)
				deadline := time.Now().Add(3 * time.Minute)
				for {
					answer, _, err := c.Exchange(ledgerSigned(name, target, ledgerMountedFudge, false), address)
					switch {
					case err == nil && answer != nil && answer.Rcode == dns.RcodeSuccess:
						noerror.Add(1)
					case answer != nil && answer.Rcode == dns.RcodeServerFailure:
						servfail.Add(1)
					case errors.Is(err, io.EOF):
						eof.Add(1)
					case ledgerReadTimeout(err):
						readTimeout.Add(1)
					default:
						other.Add(1)
						errs <- fmt.Errorf("UPDATE %d: answer %v: %v", i, answer, err)
						return
					}
					if answer != nil && answer.Rcode == dns.RcodeSuccess {
						break
					}
					if !time.Now().Before(deadline) {
						errs <- fmt.Errorf("UPDATE %d did not complete for three minutes (%d NOERROR, %d EOF and %d read-timeout retries so far)", i, noerror.Load(), eof.Load(), readTimeout.Load())
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	elapsed := time.Since(began)
	t.Logf("in %s: %d signed UPDATEs answered NOERROR (%.1f a second), %d answers SERVFAIL, %d EOF retries, %d read-timeout retries, %d other", elapsed.Round(time.Millisecond), noerror.Load(), float64(noerror.Load())/elapsed.Seconds(), servfail.Load(), eof.Load(), readTimeout.Load(), other.Load())
	if noerror.Load() != ledgerMountedUpdates {
		t.Fatalf("%d of %d UPDATEs committed", noerror.Load(), ledgerMountedUpdates)
	}

	// An exact repeat after a restart.
	kept := ledgerSigned("kept.ledger.test.", "192.0.2.12", 300, true)
	if rcode, err := ledgerExchange(client(), address, kept, time.Now().Add(30*time.Second)); rcode != dns.RcodeSuccess {
		t.Fatalf("UPDATE before restart: rcode %d: %v", rcode, err)
	}
	stop()
	stop = tsigMountedStart(t, cfg)
	defer stop()
	awaitMountedDNS(t, cfg, "kept.ledger.test.", "192.0.2.12")
	if rcode, err := ledgerExchange(client(), address, kept, time.Now().Add(30*time.Second)); rcode != dns.RcodeSuccess {
		t.Fatalf("exact repeat after restart: rcode %s (%d), want NOERROR from the retained outcome: %v", dns.RcodeToString[rcode], rcode, err)
	}
	if rcode, err := ledgerExchange(client(), address, ledgerSigned("kept.ledger.test.", "192.0.2.12", 300, true), time.Now().Add(30*time.Second)); rcode != dns.RcodeYXDomain {
		t.Fatalf("a newly signed request after restart was not decided again: rcode %d: %v", rcode, err)
	}
	awaitMountedDNS(t, cfg, "once.ledger.test.", "192.0.2.10")
}
