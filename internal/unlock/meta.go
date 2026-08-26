package unlock

import "strings"

// StatusMeta provides consistent labels and presentation hints to API clients.
type StatusMeta struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	ShortLabel  string `json:"short_label,omitempty"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
	Severity    string `json:"severity,omitempty"`
}

var statusMetas = []StatusMeta{
	{Value: StatusUnlocked, Label: "解锁", ShortLabel: "解锁", Description: "完整能力可用", Color: "success", Severity: "success"},
	{Value: StatusPartial, Label: "部分可用", ShortLabel: "部分", Description: "仅部分入口或能力可用", Color: "warning", Severity: "warning"},
	{Value: StatusOriginalsOnly, Label: "仅自制内容", ShortLabel: "自制", Description: "仅 Netflix Originals 可用", Color: "warning", Severity: "warning"},
	{Value: StatusLocked, Label: "受限", ShortLabel: "受限", Description: "当前地区或出口被限制", Color: "error", Severity: "error"},
	{Value: StatusFailed, Label: "检测失败", ShortLabel: "失败", Description: "本轮无法可靠判断", Color: "default", Severity: "default"},
}

// ListStatusMetas returns a defensive copy for APIs and callers.
func ListStatusMetas() []StatusMeta {
	return append([]StatusMeta(nil), statusMetas...)
}

// GetStatusMeta resolves one supported result status.
func GetStatusMeta(status string) (StatusMeta, bool) {
	status = strings.ToLower(strings.TrimSpace(status))
	for _, meta := range statusMetas {
		if meta.Value == status {
			return meta, true
		}
	}
	return StatusMeta{}, false
}
