package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

var (
	controlURL = "http://127.0.0.1:8082/route"
)

func handleChat(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 1. Ask Control Plane for Decision
	decisionReq, _ := json.Marshal(req)
	resp, err := http.Post(controlURL, "application/json", bytes.NewBuffer(decisionReq))
	if err != nil {
		http.Error(w, "Control plane unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	var decision struct {
		Target    string `json:"target"`
		Endpoint  string `json:"endpoint"`
		ModelName string `json:"model_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		http.Error(w, "Invalid decision from control plane", http.StatusInternalServerError)
		return
	}

	fmt.Printf("Routed request to %s (%s)\n", decision.Target, decision.Endpoint)

	// 2. Rewrite request for Target
	req["model"] = decision.ModelName
	delete(req, "routing")
	newBody, _ := json.Marshal(req)

	// 3. Proxy to Target
	targetURL, _ := url.Parse(decision.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	r.URL.Path = "/v1/chat/completions"
	r.Body = io.NopCloser(bytes.NewBuffer(newBody))
	r.ContentLength = int64(len(newBody))

	proxy.ServeHTTP(w, r)
}

func main() {
	http.HandleFunc("/v1/chat/completions", handleChat)
	fmt.Println("Lattice Frontend listening on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
