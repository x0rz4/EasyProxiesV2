package boxmgr

import (
	"fmt"
	"testing"

	"easy_proxies/internal/config"
)

func BenchmarkAppliedGroupFingerprint(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			cfg := &config.Config{}
			for i := 0; i < size; i++ {
				cfg.Nodes = append(cfg.Nodes, config.NodeConfig{ID: int64(i + 1), Name: "fixture", URI: fmt.Sprintf("http://fixture-%d:synthetic@127.0.0.1:8080", i), Region: "hk", IdentityHash: fmt.Sprintf("%064x", i+1)})
			}
			group := config.GroupPoolConfig{ID: 1, Name: "fixture", Enabled: true, Regions: []string{"hk"}, RegionRuleVersion: 2, DispatchMode: "fixed"}
			var fingerprint string
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				fingerprint = groupAppliedFingerprint(cfg, group)
			}
			b.StopTimer()
			b.ReportMetric(float64(len(fingerprint)), "stored-B")
		})
	}
}
