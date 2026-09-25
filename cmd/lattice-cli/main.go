package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/pegamonstro/ai-tools-inference-lattice/pkg/latticeconfig"
)

type Routing struct {
	Privacy      string `json:"privacy"`
	LatencyClass string `json:"latency_class"`
	Parallelism  int    `json:"parallelism"`
	RequestID    string `json:"request_id"`
}

type Request struct {
	Model    string        `json:"model"`
	Messages []interface{} `json:"messages"`
	Routing  Routing       `json:"routing"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: lattice-cli <prompt>")
		os.Exit(1)
	}

	prompt := os.Args[1]
	frontendURL := latticeconfig.Env("LATTICE_FRONTEND_URL", "http://localhost:8080/v1/chat/completions")

	reqBody := Request{
		Model: "local-brain",
		Messages: []interface{}{
			map[string]string{"role": "user", "content": prompt},
		},
		Routing: Routing{
			Privacy:      "LOCAL_PREFERRED",
			LatencyClass: "interactive",
			Parallelism:  1,
			RequestID:    "cli-req-001",
		},
	}

	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(frontendURL, "application/json", bytes.NewBuffer(body))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	resBody, _ := io.ReadAll(resp.Body)
	fmt.Printf("Lattice Response:\n%s\n", string(resBody))
}
