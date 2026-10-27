package boxmgr

import (
	"testing"

	"github.com/sagernet/sing-box"
)

func TestRuntimeResourcesCountsOverlapAndExcludesStoppedSlots(t *testing.T) {
	m := &Manager{
		currentBox: &box.Box{}, runtimeNodes: map[string]runtimeNode{"a": {}, "b": {}, "c": {}},
		groupSlots: map[int64]*groupRuntimeSlot{
			1: {box: &box.Box{}, members: map[string]runtimeNode{"a": {}, "b": {}}, retiring: map[string]*groupRetirement{"old": {}}},
			2: {box: &box.Box{}, members: map[string]runtimeNode{"b": {}, "c": {}}},
			3: {members: map[string]runtimeNode{"stale": {}}},
		},
	}
	result := m.RuntimeResources()
	if result.BaseInstances != 1 || result.BaseMembers != 3 || result.GroupInstances != 2 || result.GroupMembers != 4 || result.UniqueGroupMembers != 3 || result.MaxGroupsPerMember != 2 || result.RetiringMembers != 1 || result.GroupAmplification != 4.0/3.0 {
		t.Fatalf("incorrect resources: %+v", result)
	}
	if empty := (&Manager{}).RuntimeResources(); empty.GroupAmplification != 0 || empty.BaseInstances != 0 || empty.GroupInstances != 0 {
		t.Fatalf("incorrect empty runtime: %+v", empty)
	}
}
