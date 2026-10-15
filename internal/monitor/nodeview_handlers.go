package monitor

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/nodeview"
	"easy_proxies/internal/store"
)

type nodeQueryInput struct {
	Spec     nodeview.Spec `json:"spec"`
	Page     nodeview.Page `json:"page"`
	FilterID int64         `json:"filter_id,omitempty"`
}

func (s *Server) nodeViewRegistry() *nodefacts.Registry {
	if s.tagSvc != nil {
		return s.tagSvc.Registry()
	}
	return nodefacts.DefaultRegistry()
}

// loadNodeView batches persistence reads in one SQLite transaction and captures
// runtime health once. The projection deliberately excludes URIs/credentials.
func (s *Server) loadNodeView(ctx context.Context, withFacts bool) (*nodeview.View, error) {
	if s.store == nil {
		return nil, fmt.Errorf("数据存储不可用")
	}
	runtime := map[int64]Snapshot{}
	if s.mgr != nil {
		for _, node := range s.mgr.Snapshot() {
			if previous, ok := runtime[node.NodeID]; !ok || node.HealthUpdatedAt.After(previous.HealthUpdatedAt) || (node.HealthUpdatedAt.Equal(previous.HealthUpdatedAt) && node.Tag < previous.Tag) {
				runtime[node.NodeID] = node
			}
		}
	}
	var nodes []nodeview.Node
	names := map[int64]string{}
	var facts []nodefacts.NodeFacts
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		managed, err := tx.ListManagedNodes(ctx, nil)
		if err != nil {
			return err
		}
		tags, err := tx.ListTags(ctx)
		if err != nil {
			return err
		}
		for _, tag := range tags {
			names[tag.ID] = tag.Name
		}
		ids := make([]int64, 0, len(managed))
		for _, node := range managed {
			ids = append(ids, node.ID)
			live, found := runtime[node.ID]
			status := "pending"
			if !node.Enabled {
				status = "disabled"
			} else if !found {
				status = "unloaded"
			} else if live.Blacklisted {
				status = "blacklisted"
			} else if found && live.InitialCheckDone && live.Available {
				status = "normal"
			} else if found && live.InitialCheckDone {
				status = "unavailable"
			}
			latency := int64(-1)
			if found {
				latency = live.LastLatencyMs
			}
			protocol, _, _ := strings.Cut(node.URI, ":")
			if strings.EqualFold(protocol, "hy2") {
				protocol = "hysteria2"
			}
			nodes = append(nodes, nodeview.Node{ID: node.ID, Name: node.Name, Region: node.Region, Country: node.Country, Tags: node.Tags,
				Source: string(node.Source), Protocol: strings.ToLower(protocol), SubscriptionIDs: node.SubscriptionIDs, Status: status, LatencyMS: latency,
				Port: node.Port, RuntimeTag: live.Tag, Healthy: node.Enabled && found && live.InitialCheckDone && live.Available && !live.Blacklisted && !live.Provisional, Enabled: node.Enabled})
		}
		if withFacts && len(ids) > 0 {
			facts, err = nodefacts.NewLoader(tx).Load(ctx, ids)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return nodeview.New(nodes, names, facts, time.Now().UTC()), nil
}

func (s *Server) handleNodeQuery(w http.ResponseWriter, r *http.Request) {
	limitTagRequestBody(w, r)
	var input nodeQueryInput
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	if s.store == nil {
		writeAPIError(w, 503, "数据存储不可用")
		return
	}
	if input.FilterID > 0 {
		filter, err := s.store.GetSavedFilter(r.Context(), input.FilterID)
		if err != nil {
			writeAPIError(w, 500, err.Error())
			return
		}
		if filter == nil {
			writeAPIError(w, 404, "筛选不存在")
			return
		}
		if err := json.Unmarshal([]byte(filter.SpecJSON), &input.Spec); err != nil {
			writeAPIError(w, 500, "保存的筛选无效")
			return
		}
	}
	if err := input.Spec.Validate(s.nodeViewRegistry()); err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	if err := input.Page.Normalize(); err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	view, err := s.loadNodeView(r.Context(), input.Spec.Condition != nil || input.Spec.Diagnostic != "" || input.Page.Sort == "speed_bps" || input.Page.Sort == "fraud_score" || input.Page.Sort == "diagnostic_latency")
	if err != nil {
		writeAPIError(w, 500, err.Error())
		return
	}
	if r.URL.Path == "/api/nodes/query-ids" {
		ids, err := view.IDs(input.Spec)
		if err != nil {
			writeAPIError(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ids": ids, "snapshot_revision": view.Revision()})
		return
	}
	result, err := view.Query(input.Spec, input.Page)
	if err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	writeJSON(w, result)
}

func (s *Server) handleNodeFacets(w http.ResponseWriter, r *http.Request) {
	view, err := s.loadNodeView(r.Context(), true)
	if err != nil {
		writeAPIError(w, 500, err.Error())
		return
	}
	if r.URL.Query().Get("runtime_only") == "true" {
		writeJSON(w, view.RuntimeFacets())
		return
	}
	writeJSON(w, view.Facets())
}

type savedFilterView struct {
	store.SavedFilter
	Spec nodeview.Spec `json:"spec"`
}

func (s *Server) handleSavedFilters(w http.ResponseWriter, r *http.Request, action string) {
	limitTagRequestBody(w, r)
	if s.store == nil {
		writeAPIError(w, 503, "数据存储不可用")
		return
	}
	if action == "list" {
		filters, err := s.store.ListSavedFilters(r.Context())
		if err != nil {
			writeAPIError(w, 500, err.Error())
			return
		}
		items := make([]savedFilterView, 0, len(filters))
		for _, filter := range filters {
			var spec nodeview.Spec
			if err := json.Unmarshal([]byte(filter.SpecJSON), &spec); err != nil {
				writeAPIError(w, 500, "保存的筛选无效")
				return
			}
			items = append(items, savedFilterView{filter, spec})
		}
		writeJSON(w, map[string]any{"filters": items})
		return
	}
	id := int64(0)
	if action != "create" {
		var err error
		id, err = strconv.ParseInt(r.PathValue("filterID"), 10, 64)
		if err != nil || id <= 0 {
			writeAPIError(w, 400, "无效筛选 ID")
			return
		}
	}
	if action == "delete" {
		if err := s.store.DeleteSavedFilter(r.Context(), id); err != nil {
			writeAPIError(w, 404, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"success": true})
		return
	}
	var input savedFilterView
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || utf8.RuneCountInString(input.Name) > 100 {
		writeAPIError(w, 400, "名称须为 1–100 个字符")
		return
	}
	if err := input.Spec.Validate(s.nodeViewRegistry()); err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	if input.Spec.Version == 0 {
		input.Spec.Version = 2
	}
	raw, err := json.MarshalCanonical(input.Spec)
	if err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	input.ID, input.SpecJSON = id, string(raw)
	if err := s.store.SaveFilter(r.Context(), &input.SavedFilter); err != nil {
		writeAPIError(w, 409, err.Error())
		return
	}
	writeJSON(w, input)
}
