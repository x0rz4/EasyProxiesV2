package builder

import (
	"encoding/json"
	"os"
	"testing"

	"easy_proxies/internal/config"
)

// identityGoldenEntry mirrors the fields of a nodecodec golden snapshot that
// this test needs. The golden file is the shared corpus: it is generated once
// from the identity codec and then replayed against every exit point.
type identityGoldenEntry struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
	Err  string `json:"err,omitempty"`
}

// TestEveryIdentifiableURIBuildsAnOutbound locks the invariant that the identity
// codec and the outbound builder accept the same set of links.
//
// They did not. builder decoded vmess payloads with a stricter base64 set than
// nodecodec and ran url.Parse before dispatching on the scheme, so links the
// importer and identity happily accepted could never be built. The failure is
// not contained to that node: boxmgr aborts the whole incremental reload batch
// when one node fails to build, and the full-rebuild path fails outright.
//
// Whenever a link is accepted by nodecodec.ParseURI it must build here, and the
// test fails loudly naming the corpus entry so the gap cannot be papered over.
func TestEveryIdentifiableURIBuildsAnOutbound(t *testing.T) {
	raw, err := os.ReadFile("../nodecodec/testdata/identity_golden.json")
	if err != nil {
		t.Fatalf("read identity golden: %v", err)
	}
	var entries []identityGoldenEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("parse identity golden: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("identity golden is empty")
	}

	built := 0
	for _, entry := range entries {
		if entry.Err != "" {
			// The identity codec rejects it, so builder is allowed to as well.
			continue
		}
		if _, err := BuildNodeOutbound("parity", config.NodeConfig{Name: entry.Name, URI: entry.URI}, false); err != nil {
			t.Errorf("%s: identity accepts the link but builder rejects it: %v\n  uri: %q",
				entry.Name, err, entry.URI)
			continue
		}
		built++
	}
	if built < 60 {
		t.Fatalf("only %d links were exercised; the shared corpus lost coverage", built)
	}
	t.Logf("verified %d/%d corpus links build an outbound", built, len(entries))
}
