package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type nodeCheckStreamWriter struct {
	*httptest.ResponseRecorder
	onFlush  func()
	fail     bool
	deadline time.Time
}

func (w *nodeCheckStreamWriter) Flush() {
	w.ResponseRecorder.Flush()
	if w.onFlush != nil {
		w.onFlush()
	}
}
func (w *nodeCheckStreamWriter) Write(p []byte) (int, error) {
	if w.fail {
		return 0, errors.New("disconnected")
	}
	return w.ResponseRecorder.Write(p)
}
func (w *nodeCheckStreamWriter) SetWriteDeadline(v time.Time) error { w.deadline = v; return nil }

func finishStreamTask(task *nodeCheckTask) {
	task.mu.Lock()
	task.snapshot.Status = nodeCheckTaskCompleted
	task.mu.Unlock()
	task.publish(nodeCheckEvent{Type: "done"})
}

func TestNodeCheckTerminalSurvivesFullSubscriber(t *testing.T) {
	task := &nodeCheckTask{snapshot: nodeCheckTaskSnapshot{Status: nodeCheckTaskRunning}}
	m := &nodeCheckManager{}
	_, events, unsubscribe := m.subscribe(task)
	defer unsubscribe()
	for range 100 {
		task.publish(nodeCheckEvent{Type: "progress"})
	}
	finishStreamTask(task)
	for range cap(events) {
		if event := <-events; event.Type == "done" {
			if event.Task.Status != nodeCheckTaskCompleted {
				t.Fatal("wrong terminal state")
			}
			return
		}
	}
	t.Fatal("terminal event was dropped")
}

func TestNodeCheckStreamLifecycle(t *testing.T) {
	for _, scenario := range []string{"during-initial-flush", "already-completed", "write-failure", "server-shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			task := &nodeCheckTask{snapshot: nodeCheckTaskSnapshot{ID: "task", Status: nodeCheckTaskRunning}}
			s := &Server{done: make(chan struct{}), nodeChecks: &nodeCheckManager{tasks: map[string]*nodeCheckTask{"task": task}}}
			w := &nodeCheckStreamWriter{ResponseRecorder: httptest.NewRecorder()}
			switch scenario {
			case "during-initial-flush":
				w.onFlush = func() { w.onFlush = nil; finishStreamTask(task) }
			case "already-completed":
				finishStreamTask(task)
			case "write-failure":
				w.fail = true
			case "server-shutdown":
				w.onFlush = func() { w.onFlush = nil; close(s.done) }
			}
			r := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(t.Context())
			s.streamNodeCheckTask(w, r, "task")
			if strings.Contains(scenario, "completed") || scenario == "during-initial-flush" {
				if !strings.Contains(w.Body.String(), `"type":"done"`) {
					t.Fatal(w.Body.String())
				}
			}
			if len(task.subs) != 0 {
				t.Fatal("subscriber leaked")
			}
			if !w.deadline.IsZero() {
				t.Fatal("write deadline not reset")
			}
		})
	}
}
