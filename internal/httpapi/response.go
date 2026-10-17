package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/monitor"
)

// This file centralizes the HTTP response plumbing that handlers otherwise
// repeat: decoding a request body, serializing a response, turning a domain
// error into a status code, reading a path parameter that must be a positive
// ID, and refusing a request whose backing service is unavailable.
//
// The error mapping is deliberately three policies in one place rather than a
// single function. Each domain classifies its own errors and they genuinely
// disagree: a tag mutation reporting "节点不存在" is a 400 (the request body
// named a node that is not there), while the subscription domain reports the
// same words as a 404 — and tag_handlers_test.go pins that 400. Collapsing them
// into one message-sniffing function would silently change status codes the
// API contract and the WebUI both rely on. Only the node policy is type-driven;
// the other two stay message-driven because the store layer returns
// operator-facing text that has not been typed yet.
//
// Note what is deliberately absent: a single errorToStatus(err) dispatcher. It
// cannot be written correctly today, because the tag and subscription domains
// are distinguishable only by which handler called them, not by the error's
// type or wording. A caller therefore names its policy explicitly.
//
// A generic handle[Req, Resp](fn) wrapper was tried here and removed. Measured
// against the handlers that exist, it shortened none of them: all 14 that
// decode a body carry 3–19 error branches of their own, so the wrapper strips
// only the outer decode/serialize pair and every branch stays. The shape it
// fits best, handleReload, cannot use it without losing the existing_node_id
// fields respondNodeError attaches on monitor.ErrNodeConflict. Reintroducing it for the
// planned httpapi split should start from that measurement, not from the idea.

// pathID reads a path parameter that must parse as a positive int64, writing a
// 400 with message when it does not:
//
//	groupID, ok := pathID(w, r, "groupID", "无效的分组 ID")
//	if !ok { return }
func pathID(w http.ResponseWriter, r *http.Request, name, message string) (int64, bool) {
	value, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || value <= 0 {
		writeAPIError(w, http.StatusBadRequest, message)
		return 0, false
	}
	return value, true
}

// pathIDOrNotFound is the public-subscription variant of pathID: an unknown or
// malformed group ID is indistinguishable from a missing one there, so the
// route answers a bare 404 instead of a 400 that would confirm the ID's shape.
func pathIDOrNotFound(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	value, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || value <= 0 {
		http.NotFound(w, r)
		return 0, false
	}
	return value, true
}

// Store-unavailable messages. The three routes word this differently, and the
// difference is user-visible, so each keeps its own constant rather than being
// flattened into one wording.
const (
	storeUnavailableCredentials    = "API 凭证不可用：数据存储未初始化"
	storeUnavailableGeneric        = "数据存储不可用"
	storeUnavailableNotInitialized = "数据存储未初始化"
)

// requireStore reports whether the data store is available, writing a 503 with
// message when it is not.
func (s *Server) requireStore(w http.ResponseWriter, message string) bool {
	if s == nil || s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, message)
		return false
	}
	return true
}

// nodeErrorStatus is the node-management policy. It is type-driven, which is
// what lets respondNodeError also carry extra response fields (existing_node_id
// and friends) rather than just a status.
func nodeErrorStatus(err error) int {
	var duplicate *monitor.NodeDuplicateError
	switch {
	case errors.Is(err, monitor.ErrNodeNotFound):
		return http.StatusNotFound
	case errors.As(err, &duplicate):
		return http.StatusConflict
	case errors.Is(err, monitor.ErrNodeConflict), errors.Is(err, monitor.ErrInvalidNode):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// subscriptionErrorStatus maps subscription-domain errors. The store reports
// operator-facing Chinese text, so this stays message-driven until those errors
// become typed. Unrecognized failures are 500.
func subscriptionErrorStatus(err error) int {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "不存在") || strings.Contains(message, "not found") {
		return http.StatusNotFound
	}
	if strings.Contains(message, "不能为空") || strings.Contains(message, "必须") || strings.Contains(message, "invalid") || strings.Contains(message, "unique") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// tagErrorStatus maps tag-mutation errors. Note the deliberate divergence from
// subscriptionErrorStatus: anything unrecognized is a 400 here, and a "不存在"
// message stays a 400 too rather than becoming a 404.
func tagErrorStatus(err error) int {
	var conflict tagConflictError
	if errors.As(err, &conflict) {
		return http.StatusConflict
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "不可用") && strings.Contains(message, "服务") {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}

// maxDefaultJSONBodyBytes bounds request bodies for handlers that do not
// declare their own budget. JSON API payloads are small; the memory-DoS
// surface is the low-privilege endpoints (/api/auth, /api/public/*, group and
// subscription CRUD) that previously read the body without any cap. Handlers
// needing a larger budget (import: 12 MB, tag routes: 64 KB) wrap r.Body
// with http.MaxBytesReader themselves and call decodeJSONBody.
const maxDefaultJSONBodyBytes = 1 << 20

func decodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxDefaultJSONBodyBytes)
	return decodeJSONBody(r, dst)
}

// decodeJSONBody decodes without imposing a body limit; the caller owns the
// budget.
func decodeJSONBody(r *http.Request, dst any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("请求格式错误: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("请求只能包含一个 JSON 对象")
	}
	return nil
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"error": message})
}
