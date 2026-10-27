package nodeview

import (
	"fmt"
	"testing"
	"time"

	"easy_proxies/internal/nodefacts"
)

func BenchmarkViewConstruction(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			nodes := make([]Node, size)
			facts := make([]nodefacts.NodeFacts, size)
			for i := range nodes {
				nodes[i] = Node{ID: int64(i + 1), Name: fmt.Sprintf("节点-%d", i), Region: "hk", Enabled: true, Tags: []string{"fast", "manual"}, SubscriptionIDs: []int64{7}}
				facts[i] = nodefacts.NewNodeFacts(nodes[i].ID, nodes[i].Name, "hk")
				for field := 0; field < 32; field++ {
					facts[i].Set(fmt.Sprintf("field.%d", field), nodefacts.IntValue(nodefacts.Known(int64(i+field))))
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = New(nodes, map[int64]string{7: "fast"}, facts, time.Time{})
			}
		})
	}
}
