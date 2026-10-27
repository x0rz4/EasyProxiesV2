package boxmgr

import (
	"strings"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
)

func TestAppliedFingerprintKeepsConnectionAndPolicyScopeWithoutRetainingText(t *testing.T) {
	cfg := &config.Config{Nodes: []config.NodeConfig{{ID: 1, Name: "fixture", URI: "http://fixture:synthetic@127.0.0.1:8080", IdentityHash: strings.Repeat("a", 64)}}}
	g := config.GroupPoolConfig{ID: 1, Enabled: true, ExplicitNodeIDs: []int64{1}, DispatchMode: "fixed", RecoveryEnabled: true, RecoverySuccessThreshold: 3}
	before := groupAppliedFingerprint(cfg, g)
	if len(before) != 64 || strings.Contains(before, "synthetic") {
		t.Fatal("fingerprint retains source text")
	}
	metadata := cfg.Clone()
	metadata.Nodes[0].Region = "hk"
	metadata.Nodes[0].Country = "fixture"
	metadata.Nodes[0].Tags = []string{"new"}
	metadata.Nodes[0].CanonicalJSON = "metadata"
	if groupAppliedFingerprint(metadata, g) != before {
		t.Fatal("YAML-excluded metadata became runtime connection identity")
	}
	emptyRefill := g
	emptyRefill.Refill = []string{}
	if groupAppliedFingerprint(cfg, emptyRefill) != before {
		t.Fatal("nil and empty refill changed equality")
	}
	for _, change := range []func(*config.Config, *config.GroupPoolConfig){
		func(c *config.Config, _ *config.GroupPoolConfig) { c.Nodes[0].Name = "changed" },
		func(c *config.Config, _ *config.GroupPoolConfig) {
			c.Nodes[0].URI = "http://different:synthetic@127.0.0.1:8080"
		},
		func(c *config.Config, _ *config.GroupPoolConfig) { c.Nodes[0].Port = 1234 },
		func(c *config.Config, _ *config.GroupPoolConfig) { c.Nodes[0].Username = "changed" },
		func(c *config.Config, _ *config.GroupPoolConfig) { c.Nodes[0].Password = "changed" },
		func(c *config.Config, _ *config.GroupPoolConfig) { c.SkipCertVerify = true },
		func(_ *config.Config, g *config.GroupPoolConfig) { g.DispatchMode = "random" },
		func(_ *config.Config, g *config.GroupPoolConfig) { g.RecoverySuccessThreshold = 4 },
		func(_ *config.Config, g *config.GroupPoolConfig) { g.FailureWindow = time.Minute },
		func(_ *config.Config, g *config.GroupPoolConfig) {
			g.HealPolicy = group.HealPolicy{TargetSize: 1, Refill: []string{group.PromoteStandby}}
		},
	} {
		updated := cfg.Clone()
		policy := g
		change(updated, &policy)
		if groupAppliedFingerprint(updated, policy) == before {
			t.Fatal("connection or policy change was ignored")
		}
	}
}
