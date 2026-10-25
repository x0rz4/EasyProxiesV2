package httpapi

import (
	"net/http"
	"strconv"
	"strings"
)

// requestedNodeIDs distinguishes an absent filter (legacy full list) from an
// explicit empty page. Empty and invalid input must never widen into a full read.
func requestedNodeIDs(w http.ResponseWriter, r *http.Request) ([]int64, bool) {
	values, present := r.URL.Query()["node_ids"]
	if !present {
		return nil, true
	}
	ids := []int64{}
	seen := map[int64]bool{}
	if len(values) != 1 {
		writeAPIError(w, 400, "node_ids 必须只出现一次")
		return nil, false
	}
	if values[0] == "" {
		return ids, true
	}
	parts := strings.Split(values[0], ",")
	if len(parts) > 200 {
		writeAPIError(w, 400, "每次最多读取 200 个节点")
		return nil, false
	}
	for _, part := range parts {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			writeAPIError(w, 400, "node_ids 必须为正整数列表")
			return nil, false
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, true
}
