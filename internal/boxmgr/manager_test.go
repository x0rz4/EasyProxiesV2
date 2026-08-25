package boxmgr

import (
	"testing"

	"easy_proxies/internal/config"
)

func TestRuntimeConfigEqual(t *testing.T) {
	a := &config.Config{Mode: "pool", Nodes: []config.NodeConfig{{Name: "a", URI: "http://a.example:80"}}}
	b := a.Clone()
	if !runtimeConfigEqual(a, b) {
		t.Fatal("equivalent configurations were considered different")
	}
	b.Nodes[0].URI = "http://b.example:80"
	if runtimeConfigEqual(a, b) {
		t.Fatal("different configurations were considered equal")
	}
	b = a.Clone()
	b.Groups = []config.GroupPoolConfig{{ID: 1, Name: "group", BindPort: 10001}}
	if runtimeConfigEqual(a, b) {
		t.Fatal("group configuration change was ignored")
	}
}
