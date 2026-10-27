package httpapi

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/nodeview"
	"easy_proxies/internal/store"
)

func BenchmarkNodeViewPipeline(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			db, err := store.Open(filepath.Join(b.TempDir(), "view.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			nodes := make([]*store.Node, size)
			for i := range nodes {
				nodes[i] = &store.Node{Name: fmt.Sprintf("节点-Node-%05d", size-i), URI: fmt.Sprintf("http://127.0.0.1:%d", 10000+i), Region: "hk", Source: store.NodeSourceManual, Enabled: true}
			}
			if _, err := db.CreateNodes(ctx, nodes); err != nil {
				b.Fatal(err)
			}
			s := &Server{store: db}
			for _, withFacts := range []bool{false, true} {
				name := "load"
				if withFacts {
					name = "load_facts"
				}
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						view, err := s.loadNodeView(ctx, withFacts)
						if err != nil || view.Revision() == "" {
							b.Fatalf("load: %v", err)
						}
					}
				})
			}
			b.Run("facts_only", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if err := db.WithReadTx(ctx, func(tx store.Store) error {
						_, err := nodefacts.NewLoader(tx).Load(ctx, nil)
						return err
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
			view, err := s.loadNodeView(ctx, true)
			if err != nil {
				b.Fatal(err)
			}
			for _, sort := range []string{"id", "name", "fraud_score"} {
				b.Run("query_"+sort, func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						result, err := view.Query(nodeview.Spec{Version: 2}, nodeview.Page{Limit: 50, Sort: sort})
						if err != nil || result.Total != size || len(result.Items) != 50 {
							b.Fatalf("query: %v", err)
						}
					}
				})
			}
			result, err := view.Query(nodeview.Spec{Version: 2}, nodeview.Page{Limit: 50})
			if err != nil {
				b.Fatal(err)
			}
			b.Run("serialize_page", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if _, err := json.Marshal(result); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
