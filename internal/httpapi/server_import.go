package httpapi

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/nodecodec"
)

// handleExport 导出库内全部节点的原始代理 URI（如 trojan://、vless:// 等），每行一个。
// 范围与「节点管理」列表一致：包含禁用、未加载与暂不可用的节点；导出的内容可以直接用于导入。
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	nodes, err := s.nodeMgr.ListConfigNodes(r.Context(), nil)
	if err != nil {
		s.respondNodeError(w, err)
		return
	}

	// 稳定排序，保证多次导出的顺序一致（名字升序，同名按 ID）。
	sort.SliceStable(nodes, func(i, j int) bool {
		left, right := strings.TrimSpace(nodes[i].Name), strings.TrimSpace(nodes[j].Name)
		if left != right {
			return left < right
		}
		return nodes[i].ID < nodes[j].ID
	})

	lines := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if uri := strings.TrimSpace(node.URI); uri != "" {
			lines = append(lines, uri)
		}
	}

	// 返回纯文本，每行一个 URI
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "nodes_export.txt"}))
	w.Header().Set("X-Export-Count", strconv.Itoa(len(lines)))
	_, _ = w.Write([]byte(strings.Join(lines, "\n")))
}

// ImportIssue 描述导入过程中被跳过或失败的内容，便于前端定位到具体行或节点。
type ImportIssue struct {
	Line    int    `json:"line,omitempty"` // 原文行号，0 表示未知
	Kind    string `json:"kind"`           // unsupported_protocol | invalid_uri | create_failed | parse
	Message string `json:"message"`
	URI     string `json:"uri,omitempty"`
}

// maxImportRequestBytes 限制导入请求体大小；需高于 config 的 10MB 内容上限，因为 JSON 转义会放大体积。
const maxImportRequestBytes = 12 << 20

// newParseIssue 把解析层返回的 "第 N 行: ..." 文案拆成行号与说明。
func newParseIssue(message string) ImportIssue {
	issue := ImportIssue{Kind: "parse", Message: message}
	if line, text, ok := splitLeadingLineNumber(message); ok {
		issue.Line = line
		issue.Message = text
	}
	switch {
	case strings.Contains(issue.Message, "不支持的协议"):
		issue.Kind = "unsupported_protocol"
	case strings.Contains(issue.Message, "无效的代理 URI"):
		issue.Kind = "invalid_uri"
	}
	return issue
}

// splitLeadingLineNumber 解析 "第 N 行: 说明" 形式的消息。
func splitLeadingLineNumber(message string) (int, string, bool) {
	rest, found := strings.CutPrefix(message, "第 ")
	if !found {
		return 0, message, false
	}
	index := strings.Index(rest, " 行: ")
	if index <= 0 {
		return 0, message, false
	}
	line, err := strconv.Atoi(rest[:index])
	if err != nil {
		return 0, message, false
	}
	return line, rest[index+len(" 行: "):], true
}

// handleImport 导入节点配置，支持以下格式（自动识别，可混合）：
//   - Clash YAML 文档（含 `proxies:` 顶级键）
//   - Clash YAML 行内列表项（`- { name: ..., type: ss, ... }`，每行一个）
//   - Base64 编码的 v2ray 订阅内容
//   - 代理 URI 列表（每行一个，如 trojan://、vless://、http://、socks5:// 等）
//   - Markdown 链接或尖括号自动链接形式的代理 URI
//
// 解析出的节点统一转成标准 URI 后入库，与导出格式互通。
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxImportRequestBytes)
	var req struct {
		Content string `json:"content"` // 节点配置文本（URI / Clash YAML / Base64 均可）
	}
	if err := decodeJSONBody(r, &req); err != nil {
		var oversized *http.MaxBytesError
		message := err.Error()
		if errors.As(err, &oversized) {
			message = fmt.Sprintf("导入内容过大，上限 %d MB", maxImportRequestBytes>>20)
		}
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": message})
		return
	}

	content := strings.TrimSpace(req.Content)
	if content == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "导入内容为空"})
		return
	}

	// 统一解析为节点列表（支持 Clash YAML / Base64 / URI 列表）
	parsedNodes, parseIssues, err := config.ParseImportContentReport(content)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": fmt.Sprintf("解析导入内容失败: %v", err)})
		return
	}
	existingNodes, err := s.nodeMgr.ListConfigNodes(r.Context(), nil)
	if err != nil {
		s.respondNodeError(w, err)
		return
	}
	usedNames := make(map[string]struct{}, len(existingNodes)+len(parsedNodes))
	usedIdentities := make(map[string]monitor.ManagedNodeConfig, len(existingNodes)+len(parsedNodes))
	endpointIdentities := make(map[string]map[string]string, len(existingNodes)+len(parsedNodes))
	for _, existing := range existingNodes {
		usedNames[strings.TrimSpace(existing.Name)] = struct{}{}
		if identity, identityErr := nodecodec.ParseURI(existing.URI); identityErr == nil {
			usedIdentities[identity.Hash] = existing
			if endpointIdentities[identity.EndpointKey] == nil {
				endpointIdentities[identity.EndpointKey] = map[string]string{}
			}
			endpointIdentities[identity.EndpointKey][identity.Hash] = existing.Name
		}
	}

	var imported int
	var duplicatesSkipped int
	issues := make([]ImportIssue, 0, len(parseIssues))
	for _, message := range parseIssues {
		issues = append(issues, newParseIssue(message))
	}
	var duplicateGroups []map[string]any
	var endpointCollisions []map[string]any
	generatedNameIndex := 1

	for i := range parsedNodes {
		node := parsedNodes[i]
		node.URI = strings.TrimSpace(node.URI)
		if node.URI == "" {
			continue
		}

		identity, identityErr := nodecodec.ParseURI(node.URI)
		if identityErr != nil {
			issues = append(issues, ImportIssue{Kind: "invalid_uri", Message: fmt.Sprintf("无效的代理 URI: %v", identityErr), URI: truncateStr(node.URI, 60)})
			continue
		}
		if existing, duplicate := usedIdentities[identity.Hash]; duplicate {
			duplicatesSkipped++
			duplicateGroups = append(duplicateGroups, map[string]any{"existing_node": existing.Name, "incoming_node": node.Name})
			continue
		}
		if identities := endpointIdentities[identity.EndpointKey]; len(identities) > 0 {
			var names []string
			for _, existingName := range identities {
				names = append(names, existingName)
			}
			sort.Strings(names)
			endpointCollisions = append(endpointCollisions, map[string]any{"endpoint": identity.EndpointKey, "existing_nodes": names, "incoming_node": node.Name})
		}

		// 名称：优先用解析结果，其次从 URI fragment 提取，最后生成唯一名称。
		name := strings.TrimSpace(node.Name)
		if name == "" {
			if parsed, perr := url.Parse(node.URI); perr == nil && parsed.Fragment != "" {
				if decoded, decErr := url.QueryUnescape(parsed.Fragment); decErr == nil {
					name = decoded
				} else {
					name = parsed.Fragment
				}
			}
		}
		if name == "" {
			for {
				name = fmt.Sprintf("imported-%d", generatedNameIndex)
				generatedNameIndex++
				if _, exists := usedNames[name]; !exists {
					break
				}
			}
		} else {
			name = uniqueImportedNodeName(name, usedNames)
		}
		usedNames[name] = struct{}{}
		node.Name = name

		if !config.IsProxyURI(node.URI) {
			issues = append(issues, ImportIssue{Kind: "invalid_uri", Message: "无法识别的代理 URI", URI: truncateStr(node.URI, 60)})
			continue
		}

		if _, err := s.nodeMgr.CreateNode(r.Context(), node); err != nil {
			issues = append(issues, ImportIssue{Kind: "create_failed", Message: fmt.Sprintf("添加节点 %q 失败: %v", name, err), URI: truncateStr(node.URI, 60)})
			continue
		}
		imported++
		created := monitor.ManagedNodeConfig{Name: node.Name, URI: node.URI}
		usedIdentities[identity.Hash] = created
		if endpointIdentities[identity.EndpointKey] == nil {
			endpointIdentities[identity.EndpointKey] = map[string]string{}
		}
		endpointIdentities[identity.EndpointKey][identity.Hash] = node.Name
	}

	result := map[string]any{
		"message":            fmt.Sprintf("成功导入 %d 个节点，跳过 %d 个重复项", imported, duplicatesSkipped),
		"imported":           imported,
		"parsed":             len(parsedNodes),
		"created":            imported,
		"duplicates_skipped": duplicatesSkipped,
		"invalid":            len(issues),
	}
	if imported > 0 {
		s.enqueueRetagAll()
		// Publish the imported nodes once; the existing scheduler performs their
		// initial probes after the runtime snapshot becomes visible.
		reloadError := s.reloadAfterGroupMutation(r.Context())
		result["reloaded"] = reloadError == ""
		if reloadError != "" {
			result["reload_error"] = reloadError
		}
	}
	if len(duplicateGroups) > 0 {
		result["duplicate_groups"] = duplicateGroups
	}
	if len(endpointCollisions) > 0 {
		result["endpoint_collisions"] = endpointCollisions
	}
	if len(issues) > 0 {
		// errors 保持纯文案（含行号前缀）以兼容旧调用方，issues 提供结构化明细。
		errs := make([]string, 0, len(issues))
		for _, issue := range issues {
			if issue.Line > 0 {
				errs = append(errs, fmt.Sprintf("第 %d 行: %s", issue.Line, issue.Message))
				continue
			}
			errs = append(errs, issue.Message)
		}
		result["errors"] = errs
		result["issues"] = issues
	}
	writeJSON(w, result)
}

func uniqueImportedNodeName(base string, used map[string]struct{}) string {
	if _, exists := used[base]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

// truncateStr truncates a string to maxLen and appends "..." if truncated.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
