package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Every call to Ollama must share one transport, and a finished stream must
// leave its connection reusable.
//
// A transport built per request cannot pool at all. Worse, a zero-value
// transport leaves IdleConnTimeout unset, and an idle connection keeps its
// transport reachable, so a connection parked idle is never closed by age and
// neither it nor its transport is ever collected. On the live gateway that
// accumulated ~1,850 sockets held open against Ollama — every one of them a
// descriptor the process could not get back.
//
// The assertion is on connections left *not closed*: after a sequence of
// streamed answers the server should be holding the single connection it is
// reusing, not one abandoned connection per request.
func TestStreamedRequestsDoNotStrandConnections(t *testing.T) {
	var mu sync.Mutex
	states := map[net.Conn]http.ConnState{}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"hermes3:8b","message":{"content":"hi"},"prompt_eval_count":1,"eval_count":1,"done":true}` + "\n"))
	}))
	srv.Config.ConnState = func(c net.Conn, st http.ConnState) {
		mu.Lock()
		states[c] = st
		mu.Unlock()
	}
	srv.Start()
	defer srv.Close()

	p := &OllamaProvider{Endpoint: srv.URL}
	const requests = 5
	for i := 0; i < requests; i++ {
		if _, _, err := p.ExecuteStream(context.Background(),
			Request{Model: "hermes3:8b", Messages: []interface{}{}}, httptest.NewRecorder()); err != nil {
			t.Fatalf("stream %d failed: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	var open []http.ConnState
	for _, st := range states {
		if st != http.StateClosed {
			open = append(open, st)
		}
	}
	if len(open) != 1 {
		t.Errorf("%d streamed requests left %d connection(s) not closed (%v), want 1",
			requests, len(open), open)
	}
}
