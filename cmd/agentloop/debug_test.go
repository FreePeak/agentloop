package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

)

func TestDebugSubmit(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(routes(s))
	defer srv.Close()

	body := `{"goal":"debug","context":"test"}`
	resp, err := http.Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var submit map[string]any
	json.NewDecoder(resp.Body).Decode(&submit)
	resp.Body.Close()
	runID, _ := submit["run_id"].(string)

	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		getResp, _ := http.Get(srv.URL + "/v1/runs/" + runID)
		if getResp.StatusCode == 200 {
			var result RunResultResponse
			json.NewDecoder(getResp.Body).Decode(&result)
			getResp.Body.Close()
			fmt.Printf("i=%d state=%q exit=%q steps=%d\n", i, result.State, result.ExitReason, len(result.Steps))
			if len(result.Steps) > 0 {
				break
			}
		} else {
			getResp.Body.Close()
		}
	}

	// Check server state
	s.mu.Lock()
	r, ok := s.runs[runID]
	s.mu.Unlock()
	if ok {
		fmt.Printf("server: state=%q exit=%q steps=%d\n", r.State, r.ExitReason, len(r.Steps))
		for _, st := range r.Steps {
			fmt.Printf("  step %d: tool=%q result=%v\n", st.StepID, st.Tool, st.Result)
		}
	}
}
