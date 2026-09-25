package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
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

type Decision struct {
	Target    string `json:"target"`
	Endpoint  string `json:"endpoint"`
	ModelName string `json:"model_name"`
}

var (
	cloudSemaphore = make(chan struct{}, 3)
	capabilities   = map[string]struct {
		local string
		cloud string
	}{
		"local-brain": {local: "granite4:3b", cloud: "gemma4:31b-cloud"},
		"local-coder": {local: "hermes3:8b", cloud: "deepseek-v4-pro:cloud"},
	}
	macGatewayURL = "http://localhost:8081"
	cloudURL      = "http://localhost:11434"

	// Health State
	gatewayHealthy = true
	healthMutex    sync.RWMutex
)

func monitorHealth() {
	for {
		resp, err := http.Get(macGatewayURL + "/health")
		if err != nil || resp.StatusCode != http.StatusOK {
			healthMutex.Lock()
			gatewayHealthy = false
			healthMutex.Unlock()
			fmt.Println("Health check: Gateway UNHEALTHY")
		} else {
			healthMutex.Lock()
			gatewayHealthy = true
			healthMutex.Unlock()
		}
		time.Sleep(10 * time.Second)
	}
}

func handleRoute(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	var req Request
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fmt.Printf("Routing Request [%s]: Model=%s, Privacy=%s, Latency=%s\n",
		req.Routing.RequestID, req.Model, req.Routing.Privacy, req.Routing.LatencyClass)

	healthMutex.RLock()
	healthy := gatewayHealthy
	healthMutex.RUnlock()

	// 1. Routing Decision
	target := "local"
	if req.Routing.LatencyClass == "interactive" && req.Routing.Privacy != "LOCAL_ONLY" {
		target = "cloud"
	}

	// 2. Fallback if Gateway unhealthy
	if !healthy && target == "local" {
		if req.Routing.Privacy == "LOCAL_ONLY" {
			http.Error(w, "Gateway unhealthy and LOCAL_ONLY requested", http.StatusServiceUnavailable)
			return
		}
		fmt.Println("  Gateway unhealthy, falling back to cloud")
		target = "cloud"
	}

	// 3. Cloud Concurrency Gate
	if target == "cloud" {
		select {
		case cloudSemaphore <- struct{}{}:
			defer func() { <-cloudSemaphore }()
		default:
			if req.Routing.Privacy != "LOCAL_ONLY" {
				fmt.Println("  Cloud full, spilling to local")
				target = "local"
				// If we spill back to local but local is unhealthy, we are stuck
				if !healthy {
					http.Error(w, "Cloud full and Gateway unhealthy", http.StatusServiceUnavailable)
					return
				}
			} else {
				http.Error(w, "Cloud capacity exceeded and LOCAL_ONLY requested", http.StatusServiceUnavailable)
				return
			}
		}
	}

	cap, ok := capabilities[req.Model]
	if !ok {
		http.Error(w, "Unknown model alias", http.StatusBadRequest)
		return
	}

	modelName := cap.local
	endpoint := macGatewayURL
	if target == "cloud" {
		modelName = cap.cloud
		endpoint = cloudURL
	}

	decision := Decision{
		Target:    target,
		Endpoint: endpoint,
		ModelName: modelName,
	}

	json.NewEncoder(w).Encode(decision)
}

func main() {
	go monitorHealth()
	http.HandleFunc("/route", handleRoute)
	fmt.Println("Lattice Control listening on :8082...")
	log.Fatal(http.ListenAndServe(":8082", nil))
}
