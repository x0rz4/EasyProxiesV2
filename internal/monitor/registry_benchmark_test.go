package monitor

import (
	"fmt"
	"runtime"
	"testing"
)

func BenchmarkRegistryIdleNodes(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			infos := make([]NodeInfo, size)
			for i := range infos {
				infos[i] = NodeInfo{NodeID: int64(i + 1), Tag: fmt.Sprint(i), Name: "fixture"}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				registry := newNodeRegistry(&Manager{})
				for _, info := range infos {
					registry.register(info)
				}
				runtime.KeepAlive(registry)
			}
		})
	}
}
