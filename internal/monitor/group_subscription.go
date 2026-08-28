package monitor

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"easy_proxies/internal/group"
	"easy_proxies/internal/store"
	"easy_proxies/internal/subrender"
)

type subscriptionNode struct {
	store.Node
	tag      string
	isActive bool
}

// handleGroupSubscription serves token-protected, real-time group
// subscriptions without requiring a WebUI session.
func (s *Server) handleGroupSubscription(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	groupID, err := strconv.ParseInt(r.PathValue("groupID"), 10, 64)
	if err != nil || groupID <= 0 {
		http.NotFound(w, r)
		return
	}
	groupPool, err := s.store.GetGroupPool(r.Context(), groupID)
	if err != nil || groupPool == nil || !groupPool.SubscriptionEnabled {
		http.NotFound(w, r)
		return
	}
	if !validSubscriptionToken(groupPool.SubscriptionToken, subscriptionToken(r)) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="group-subscription"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "clash"
	}
	if format != "clash" && format != "base64" && format != "uri" {
		http.Error(w, "unsupported format (use clash, base64, or uri)", http.StatusBadRequest)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	if r.Pattern == "GET /sub/{groupID}/entry" {
		mode = "entry"
	}
	if mode == "" {
		mode = groupPool.SubscriptionMode
	}
	if mode != "entry" && mode != "members" {
		http.Error(w, "unsupported mode (use members or entry)", http.StatusBadRequest)
		return
	}

	aliveOnly := !strings.EqualFold(r.URL.Query().Get("alive_only"), "false")
	nodes, upload, download, err := s.subscriptionNodes(r, groupPool, aliveOnly)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, skipped, err := s.renderGroupSubscription(r, groupPool, nodes, format, mode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	filename := safeSubscriptionFilename(groupPool.Name)
	if format == "clash" {
		filename += ".yaml"
	} else {
		filename += ".txt"
	}
	if format == "clash" {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Subscription-Userinfo", fmt.Sprintf("upload=%d; download=%d; total=0; expire=0", upload, download))
	w.Header().Set("Profile-Update-Interval", "12")
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	if skipped > 0 {
		w.Header().Set("X-Subscription-Skipped", strconv.Itoa(skipped))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) subscriptionNodes(r *http.Request, groupPool *store.GroupPool, aliveOnly bool) ([]subscriptionNode, int64, int64, error) {
	storeNodes, err := s.store.ListNodes(r.Context(), store.NodeFilter{})
	if err != nil {
		return nil, 0, 0, err
	}
	nodeByID := make(map[int64]store.Node, len(storeNodes))
	for _, node := range storeNodes {
		nodeByID[node.ID] = node
	}
	monitorByTag := make(map[string]Snapshot)
	if s.mgr != nil {
		for _, snapshot := range s.mgr.Snapshot() {
			monitorByTag[snapshot.Tag] = snapshot
		}
	}
	runtime, ok := group.GroupRuntimeSnapshots()[groupPool.ID]
	if !ok {
		return []subscriptionNode{}, 0, 0, nil
	}
	result := make([]subscriptionNode, 0, len(runtime.Members))
	var upload, download int64
	for _, member := range runtime.Members {
		if member.Status == "EVICTED" {
			continue
		}
		monitorState := monitorByTag[member.Tag]
		alive := member.Status == "ALIVE" && (!monitorState.InitialCheckDone || monitorState.Available)
		if aliveOnly && !alive {
			continue
		}
		node, exists := nodeByID[member.NodeID]
		if !exists || !node.Enabled || strings.TrimSpace(node.URI) == "" {
			continue
		}
		result = append(result, subscriptionNode{Node: node, tag: member.Tag, isActive: member.NodeID == runtime.CurrentNodeID})
		upload += monitorState.TotalUpload
		download += monitorState.TotalDownload
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].isActive != result[j].isActive {
			return result[i].isActive
		}
		return result[i].Name < result[j].Name
	})
	return result, upload, download, nil
}

func (s *Server) renderGroupSubscription(r *http.Request, groupPool *store.GroupPool, nodes []subscriptionNode, format, mode string) ([]byte, int, error) {
	groupName := strings.TrimSpace(groupPool.Name)
	if groupName == "" {
		groupName = fmt.Sprintf("Group-%d", groupPool.ID)
	}
	if mode == "entry" {
		host := s.groupExternalHost(r, groupPool)
		if host == "" {
			return nil, 0, fmt.Errorf("entry subscription requires external_ip or external_host")
		}
		entryName := groupName + "-入口"
		uri := subrender.EntryURI(entryName, groupPool.Protocol, host, groupPool.BindPort, groupPool.Username, groupPool.Password)
		switch format {
		case "uri":
			return []byte(uri + "\n"), 0, nil
		case "base64":
			return []byte(base64.StdEncoding.EncodeToString([]byte(uri + "\n"))), 0, nil
		default:
			body, err := subrender.RenderClash(groupName, "fixed", []map[string]any{subrender.EntryProxy(entryName, groupPool.Protocol, host, groupPool.BindPort, groupPool.Username, groupPool.Password)})
			return body, 0, err
		}
	}

	if len(nodes) == 0 {
		return nil, 0, fmt.Errorf("no healthy group members available")
	}
	lines := make([]string, 0, len(nodes))
	proxies := make([]map[string]any, 0, len(nodes))
	usedNames := make(map[string]int)
	skipped := 0
	for _, node := range nodes {
		lines = append(lines, node.URI)
		name := uniqueSubscriptionName(node.Name, usedNames)
		proxy, err := subrender.ClashProxy(name, node.URI)
		if err != nil {
			skipped++
			continue
		}
		proxies = append(proxies, proxy)
	}
	joined := strings.Join(lines, "\n") + "\n"
	switch format {
	case "uri":
		return []byte(joined), 0, nil
	case "base64":
		return []byte(base64.StdEncoding.EncodeToString([]byte(joined))), 0, nil
	default:
		if len(proxies) == 0 {
			return nil, skipped, fmt.Errorf("no group members can be converted to Clash format")
		}
		body, err := subrender.RenderClash(groupName, groupPool.DispatchMode, proxies)
		return body, skipped, err
	}
}

func (s *Server) groupExternalHost(r *http.Request, groupPool *store.GroupPool) string {
	if host := normalizeExternalHost(groupPool.ExternalHost); host != "" {
		return host
	}
	s.cfgMu.RLock()
	cfg := s.cfgSrc
	s.cfgMu.RUnlock()
	if cfg != nil {
		cfg.RLock()
		externalIP := cfg.ExternalIP
		cfg.RUnlock()
		if host := normalizeExternalHost(externalIP); host != "" {
			return host
		}
	}
	host := r.Host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	return normalizeExternalHost(host)
}

func normalizeExternalHost(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "://") {
		if parsed, err := url.Parse(value); err == nil {
			value = parsed.Hostname()
		}
	}
	value = strings.Trim(value, "[]")
	if value == "0.0.0.0" || value == "::" {
		return ""
	}
	return value
}

func subscriptionToken(r *http.Request) string {
	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}
	if token := r.Header.Get("X-Subscription-Token"); token != "" {
		return token
	}
	if header := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

func validSubscriptionToken(expected, supplied string) bool {
	if expected == "" || len(expected) != len(supplied) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(supplied)) == 1
}

func uniqueSubscriptionName(value string, used map[string]int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "Proxy"
	}
	used[value]++
	if used[value] == 1 {
		return value
	}
	return fmt.Sprintf("%s-%d", value, used[value])
}

func safeSubscriptionFilename(value string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(value) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		} else if builder.Len() > 0 && builder.String()[builder.Len()-1] != '-' {
			builder.WriteByte('-')
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "group-subscription"
	}
	return result
}
