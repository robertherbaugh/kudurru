package dnsserver

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// bindingOutput refuses an unbound emission, as the root output does once a
// canonical commit is not yet published (an unbound output keeps full data
// currency). A second binding is refused: the pipeline's own binding must
// not be repeated.
type bindingOutput struct {
	mu    sync.Mutex
	bound []*dns.Msg
	emits int
}

func (o *bindingOutput) BindResponse(response *pipeline.Response) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if response == nil || response.Msg == nil || len(o.bound) != 0 {
		return errors.New("binding refused")
	}
	o.bound = append(o.bound, response.Msg.Copy())
	return nil
}

func (o *bindingOutput) Emit(fn func() error) error {
	o.mu.Lock()
	unbound := len(o.bound) == 0
	o.emits++
	o.mu.Unlock()
	if unbound {
		return errors.New("unbound output refused")
	}
	return fn()
}

func (o *bindingOutput) Next() error { return errors.New("unexpected continuation") }

// A TSIG refusal is answered by the transport before any pipeline work. It
// binds its own message to the captured output, so an output policy that
// admits node-produced refusals of the served publication also admits it;
// unbound, it was withheld and the client timed out. A verified request is
// still bound exactly once, by the pipeline.
func TestCapturedTSIGRefusalBindsOutput(t *testing.T) {
	right := testKeyring(t)
	wrong, err := NewStaticKeyring(TSIGKey{Name: "XFR-Key", Algorithm: "hmac-sha256", Secret: []byte("fedcba9876543210fedcba9876543210")})
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []Protocol{ProtoUDP, ProtoTCP, ProtoDoT, ProtoDoH, ProtoDoH3, ProtoDoQ} {
		t.Run(string(protocol), func(t *testing.T) {
			outputs := make(chan *bindingOutput, 4)
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: protocol}}, stages: []pipeline.Stage{localAnswer{}}, handler: func(h *Handler) {
				fixedTransportCapture(right)(h)
				h.Output = func(req *pipeline.Request) (Output, error) {
					if req.Serving == nil || req.Ctx.Err() != nil {
						return nil, pipeline.ErrServingCapture
					}
					output := &bindingOutput{}
					outputs <- output
					return output, nil
				}
			}})
			exchange := func(keys *StaticKeyring) *dns.Msg {
				t.Helper()
				q := query("scoped.example.")
				q.Id = 0
				q.SetTsig("XFR-Key.", dns.HmacSHA256, 300, time.Now().Unix())
				wire, _, err := dns.TsigGenerateWithProvider(q, TSIGProvider(keys), "", false)
				if err != nil {
					t.Fatal(err)
				}
				switch protocol {
				case ProtoUDP, ProtoTCP, ProtoDoT:
					answer, err := captureNativeExchange(captureNativeDial(t, f, protocol), protocol, wire)
					if err != nil {
						t.Fatal("no native answer", err)
					}
					return encryptedResponse(t, answer)
				default:
					return encryptedResponse(t, encryptedExchange(t, f, protocol, http.MethodPost, wire))
				}
			}
			check := func(output *bindingOutput, rcode int) {
				t.Helper()
				output.mu.Lock()
				defer output.mu.Unlock()
				if len(output.bound) != 1 || output.bound[0].Rcode != rcode || output.emits != 1 {
					t.Fatalf("output bound %d messages and emitted %d times", len(output.bound), output.emits)
				}
			}
			refusal := exchange(wrong)
			if refusal.Rcode != dns.RcodeNotAuth || refusal.IsTsig() == nil || refusal.IsTsig().Error != dns.RcodeBadSig || len(refusal.Answer) != 0 {
				t.Fatal("wrong-key request was not refused BADSIG")
			}
			check(<-outputs, dns.RcodeNotAuth)
			checkA(t, exchange(right))
			check(<-outputs, dns.RcodeSuccess)
		})
	}
}
