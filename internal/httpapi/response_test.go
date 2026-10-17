package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"easy_proxies/internal/monitor"
)

// The three error→status policies live together in response.go, which makes it
// tempting to collapse them. These tests pin the behavior each one had before
// the consolidation, including the one place they deliberately disagree.

func TestSubscriptionErrorStatus(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{"not found (Chinese)", fmt.Errorf("订阅不存在: 3"), http.StatusNotFound},
		{"not found (English)", fmt.Errorf("subscription not found"), http.StatusNotFound},
		{"empty field", fmt.Errorf("名称不能为空"), http.StatusBadRequest},
		{"required field", fmt.Errorf("必须提供 URI"), http.StatusBadRequest},
		{"invalid", fmt.Errorf("invalid port"), http.StatusBadRequest},
		{"unique", fmt.Errorf("name must be unique"), http.StatusBadRequest},
		{"unclassified", fmt.Errorf("数据库锁超时"), http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := subscriptionErrorStatus(test.err); got != test.want {
				t.Fatalf("subscriptionErrorStatus(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

func TestTagErrorStatus(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{"conflict", tagConflictError{name: "existing"}, http.StatusConflict},
		{"service unavailable", fmt.Errorf("标签服务不可用"), http.StatusServiceUnavailable},
		{"validation", fmt.Errorf("标签名称长度必须为 1..64 个字符"), http.StatusBadRequest},
		// Unclassified tag failures are 400, not 500: unlike the subscription
		// domain, everything reaching here is a rejected mutation.
		{"unclassified", fmt.Errorf("数据库锁超时"), http.StatusBadRequest},
		// And a missing node stays a 400 here even though the same words are a
		// 404 in the subscription domain below.
		{"missing node", fmt.Errorf("节点不存在: 12"), http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := tagErrorStatus(test.err); got != test.want {
				t.Fatalf("tagErrorStatus(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

func TestNodeErrorStatus(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{"not found", monitor.ErrNodeNotFound, http.StatusNotFound},
		{"conflict", monitor.ErrNodeConflict, http.StatusBadRequest},
		{"invalid", monitor.ErrInvalidNode, http.StatusBadRequest},
		{"duplicate", &monitor.NodeDuplicateError{ExistingID: 42, ExistingName: "dup"}, http.StatusConflict},
		{"wrapped not found", fmt.Errorf("lookup: %w", monitor.ErrNodeNotFound), http.StatusNotFound},
		{"unclassified", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nodeErrorStatus(test.err); got != test.want {
				t.Fatalf("nodeErrorStatus(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

// This is the reason the policies were not merged: the tag and subscription
// domains are distinguishable only by which handler invoked them, so the same
// error text legitimately maps to different codes. tag_handlers_test.go depends
// on the 400.
func TestTagAndSubscriptionDomainsDisagreeOnMissingNode(t *testing.T) {
	err := fmt.Errorf("节点不存在: 12")
	if got := tagErrorStatus(err); got != http.StatusBadRequest {
		t.Errorf("tag domain = %d, want 400", got)
	}
	if got := subscriptionErrorStatus(err); got != http.StatusNotFound {
		t.Errorf("subscription domain = %d, want 404", got)
	}
	// And neither is the node domain's answer.
	if got := nodeErrorStatus(err); got != http.StatusInternalServerError {
		t.Errorf("node domain = %d, want 500 (the error is not typed as monitor.ErrNodeNotFound)", got)
	}
}

// respondNodeError is the one wrapper that decorates the shared policy, so the
// duplicate-node fields must survive alongside the mapped status.
func TestRespondNodeErrorCarriesDuplicateDetails(t *testing.T) {
	server := &Server{}
	response := httptest.NewRecorder()
	server.respondNodeError(response, &monitor.NodeDuplicateError{ExistingID: 42, ExistingName: "existing"})

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.Code)
	}
	body := response.Body.String()
	for _, want := range []string{`"existing_node_id":42`, `"existing_node_name":"existing"`, `"error"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

func TestPathIDRejectsNonPositiveAndUnparseable(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"not a number", "abc"},
		{"zero", "0"},
		{"negative", "-1"},
		{"float", "1.5"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/groups/1", nil)
			request.SetPathValue("groupID", test.value)
			response := httptest.NewRecorder()

			id, ok := pathID(response, request, "groupID", "无效的分组 ID")
			if ok || id != 0 {
				t.Fatalf("pathID(%q) = (%d, %v), want rejected", test.value, id, ok)
			}
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.Code)
			}
			if !strings.Contains(response.Body.String(), "无效的分组 ID") {
				t.Fatalf("unexpected body: %s", response.Body.String())
			}
		})
	}
}

func TestPathIDAcceptsPositive(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/groups/7", nil)
	request.SetPathValue("groupID", "7")
	response := httptest.NewRecorder()

	id, ok := pathID(response, request, "groupID", "无效的分组 ID")
	if !ok || id != 7 {
		t.Fatalf("pathID = (%d, %v), want (7, true)", id, ok)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("a valid ID must not write a status, got %d", response.Code)
	}
}

// The public subscription routes deliberately answer a malformed ID with 404
// rather than 400, so a caller cannot probe for valid ID formats.
func TestPathIDOrNotFoundAnswers404(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/sub/abc", nil)
	request.SetPathValue("groupID", "abc")
	response := httptest.NewRecorder()

	if _, ok := pathIDOrNotFound(response, request, "groupID"); ok {
		t.Fatal("malformed ID was accepted")
	}
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestRequireStore(t *testing.T) {
	server := &Server{}
	response := httptest.NewRecorder()
	if server.requireStore(response, storeUnavailableGeneric) {
		t.Fatal("requireStore reported true with a nil store")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if !strings.Contains(response.Body.String(), storeUnavailableGeneric) {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}

	var nilServer *Server
	response = httptest.NewRecorder()
	if nilServer.requireStore(response, storeUnavailableGeneric) {
		t.Fatal("requireStore reported true on a nil server")
	}
}
