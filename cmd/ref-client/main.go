package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type Routing struct {
	Privacy       string `json:"privacy"`
	LatencyClass  string `json:"latency_class"`
	Parallelism   int    `json:"parallelism"`
	RequestID     string `json:"request_id"`
}

type Request struct {
	Model    string        `json:"model"`
	Messages []interface{} `json:"messages"`
	Routing  Routing       `json:"routing"`
	Stream   bool          `json:"stream"`
}

func main() {
	req := Request{
		Model: "local-brain",
		Messages: []interface{}{
			map[string]string{"role": "user", "content": "Hello Lattice!"},
		},
		Routing: Routing{
			Privacy:      "LOCAL_ONLY",
			LatencyClass: "interactive",
			Parallelism:  1,
			RequestID:    "test-req-001",
		},
		Stream: false,
	}

	body, _ := json.Marshal(req)
	resp, err := http.Post("http://localhost:8080/v1/chat/completions", "application/json", bytes.NewBuffer(body))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("Response: %s\n", string(b))
}
