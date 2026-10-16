package nodecodec

import "testing"

// TestSpecRoundTripsThroughItsOwnURI locks the property that makes Spec useful:
// rendering it back to a link and re-parsing describes the same connection.
//
// The old Clash → URI conversion could not do this. Its hand-written formatters
// emitted only the fields they knew about, so a vmess node imported from Clash
// lost alterId and cipher on the way out and the loss was invisible until the
// node failed to authenticate.
//
// Identity equality is deliberately NOT asserted here. Rendering is canonical,
// so a link that stated contradictory parameters (allowInsecure=1 together with
// insecure=0) or relied on the lossy `type` default can legitimately re-identify
// on the way back — that is the very defect documented in golden_test.go, and
// fixing it is a versioned migration rather than a renderer concern.
func TestSpecRoundTripsThroughItsOwnURI(t *testing.T) {
	roundTripped := 0
	for _, c := range identityCorpus() {
		spec, err := FromURI(c.URI)
		if err != nil {
			continue
		}
		rendered := spec.ToURI()
		reparsed, err := FromURI(rendered)
		if err != nil {
			t.Errorf("%s: rendered link does not parse\n  uri:      %s\n  rendered: %s\n  error:    %v",
				c.Name, c.URI, rendered, err)
			continue
		}
		if !spec.Equal(reparsed) {
			t.Errorf("%s: round trip changed the connection\n  uri:      %s\n  rendered: %s\n  before:   %s\n  after:    %s",
				c.Name, c.URI, rendered, describe(spec), describe(reparsed))
			continue
		}
		if second := reparsed.ToURI(); second != rendered {
			t.Errorf("%s: rendering is not stable\n  first:  %s\n  second: %s", c.Name, rendered, second)
			continue
		}
		roundTripped++
	}
	if roundTripped < 60 {
		t.Fatalf("only %d links round-tripped; the shared corpus lost coverage", roundTripped)
	}
	t.Logf("round-tripped %d links through Spec.ToURI", roundTripped)
}

func describe(spec Spec) string {
	return "protocol=" + spec.Protocol + " auth=" + spec.AuthString() +
		" uuid=" + spec.Auth.UUID + " tls=" + spec.TLS.Security +
		" enabled=" + boolFlag(spec.TLS.Enabled) + " sni=" + spec.TLS.ServerName +
		" insecure=" + boolFlag(spec.TLS.Insecure) + " transport=" + spec.Transport.Type +
		" path=" + spec.Transport.Path + " host=" + spec.Transport.Host +
		" svc=" + spec.Transport.ServiceName + " cipher=" + spec.Cipher
}
