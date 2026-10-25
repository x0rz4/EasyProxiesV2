package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

type pageResultStore struct {
	store.Store
	calls [][]int64
}

func (s *pageResultStore) record(ids []int64) { s.calls = append(s.calls, ids) }
func (s *pageResultStore) ListUnlockResultsByIDs(_ context.Context, ids []int64) (map[int64]*store.UnlockResult, error) {
	s.record(ids)
	return map[int64]*store.UnlockResult{}, nil
}
func (s *pageResultStore) ListNodeDetectionResultsByIDs(_ context.Context, ids []int64) (map[int64]*store.NodeDetectionResult, error) {
	s.record(ids)
	return map[int64]*store.NodeDetectionResult{}, nil
}
func (s *pageResultStore) ListNodeIPQualityResultsByIDs(_ context.Context, ids []int64) (map[int64][]store.NodeIPQualityResult, error) {
	s.record(ids)
	return map[int64][]store.NodeIPQualityResult{}, nil
}

func TestPagedResultReadsNeverWidenIDs(t *testing.T) {
	for _, input := range []struct {
		raw    string
		ids    []int64
		status int
	}{
		{"2,2,7", []int64{2, 7}, 200}, {"", []int64{}, 200},
		{"0", nil, 400}, {"-1", nil, 400}, {"2,x", nil, 400},
		{strings.Repeat("1,", 200) + "1", nil, 400},
	} {
		t.Run(input.raw, func(t *testing.T) {
			for _, handler := range []string{"runtime", "unlock", "checks"} {
				db := &pageResultStore{}
				mgr := mustNewManager(t)
				mgr.Register(monitor.NodeInfo{Tag: "unrelated", NodeID: 99})
				s := &Server{store: db, mgr: mgr}
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/?node_ids="+input.raw, nil)
				switch handler {
				case "runtime":
					s.handleNodes(w, r)
				case "unlock":
					s.handleUnlockResults(w, r)
				case "checks":
					s.handleNodeCheckResults(w, r)
				}
				if w.Code != input.status {
					t.Fatalf("%s status=%d body=%s", handler, w.Code, w.Body.String())
				}
				for _, ids := range db.calls {
					if !reflect.DeepEqual(ids, input.ids) {
						t.Fatalf("read IDs=%v want=%v", ids, input.ids)
					}
				}
				if strings.Contains(w.Body.String(), "unrelated") {
					t.Fatal("unrelated runtime was loaded")
				}
			}
		})
	}
}
