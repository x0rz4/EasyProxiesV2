package boxmgr

import "easy_proxies/internal/controlplane"

var _ controlplane.RuntimeResourceReader = (*Manager)(nil)

// RuntimeResources counts installed members, including standby members that
// still own outbounds. It does not build configs, clone nodes or take reloadMu.
// The result is an observation across slots, not a transactional topology dump.
func (m *Manager) RuntimeResources() controlplane.RuntimeResources {
	var result controlplane.RuntimeResources
	m.mu.RLock()
	if m.currentBox != nil {
		result.BaseInstances = 1
		result.BaseMembers = len(m.runtimeNodes)
	}
	m.mu.RUnlock()
	m.groupSlotsMu.Lock()
	slots := make([]*groupRuntimeSlot, 0, len(m.groupSlots))
	for _, slot := range m.groupSlots {
		slots = append(slots, slot)
	}
	m.groupSlotsMu.Unlock()
	membership := make(map[string]int)
	for _, slot := range slots {
		slot.mu.RLock()
		result.RetiringMembers += len(slot.retiring)
		if slot.box != nil {
			result.GroupInstances++
			result.GroupMembers += len(slot.members)
			for key := range slot.members {
				membership[key]++
				if membership[key] > result.MaxGroupsPerMember {
					result.MaxGroupsPerMember = membership[key]
				}
			}
		}
		slot.mu.RUnlock()
	}
	result.UniqueGroupMembers = len(membership)
	if len(membership) > 0 {
		result.GroupAmplification = float64(result.GroupMembers) / float64(len(membership))
	}
	return result
}
