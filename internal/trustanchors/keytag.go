package trustanchors

import "github.com/miekg/dns"

// KeyTag returns the RFC 4034 key tag and algorithm of the key's identity, the
// retained DNSKEY with the REVOKE bit cleared. This is the tag of the key's DS
// record, so it stays the same when the key is revoked (a revoked DNSKEY's own
// tag changes, RFC 5011 section 2.1). It is diagnostic output only: it is not
// unique, and nothing may identify, accept or deny a key by it.
func (k Key) KeyTag() (tag uint16, algorithm uint8, err error) {
	rr, err := dns.NewRR(k.DNSKEY)
	if err != nil {
		return 0, 0, ErrInvalid
	}
	key, ok := rr.(*dns.DNSKEY)
	if !ok || key == nil {
		return 0, 0, ErrInvalid
	}
	identity := *key
	identity.Flags &^= dns.REVOKE
	return identity.KeyTag(), identity.Algorithm, nil
}
