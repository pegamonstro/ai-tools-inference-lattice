package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type Routing struct {
	Privacy       string `json:"privacy"`
	LatencyClass  string `json:"latency_class"`
	Parallelism   int    `json:"parallelism"`
	RequestID     string `json:"request_id"`
}

type Request struct {
	Model    string  `json:"model"`
	Messages []interface{} `json:"messages"`
	Routing  Routing `json:"routing"`
	Stream   bool    `json:"stream"`
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fmt.Printf("Received Request [%s]:\n", req.Routing.RequestID)
	fmt.Printf("  Model: %s\n", req.Model)
	fmt.Printf("  Privacy: %s\n", req.Routing.Privacy)
	fmt.Printf("  Latency: %s\n", req.Routing.LatencyClass)
	fmt.Printf("  Parallelism: %d\n", req.Routing.Parallelism)

	resp := map[string]interface{}{
		"id": "ref-completion-123",
		"object": "chat.completion",
		"choices": []interface{}{
			map[string]interface{}{
				"message": map[string]string{"role": "assistant", "content": "Reference server acknowledged request."},
			},
		},
	}
	json.NewEncoder(w).Encode(resp)
}

func main() {
	http.HandleFunc("/v1/chat/completions", handleChat)
	fmt.Println("Reference Server listening on :8080...")
	http.ListenAndServe(":8080", nil)
}
