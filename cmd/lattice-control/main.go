package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type Routing struct {
	Privacy         string                 `json:"privacy"`
	LatencyClass    string                 `json:"latency_class"`
	Parallelism     int                    `json:"parallelism"`
	RequestID       string                 `json:"request_id"`
	ProviderParams map[string]interface{} `json:"provider_params"`
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

type Telemetry struct {
	RequestID     string  `json:"request_id"`
	DecisionTime float64 `json:"decision_time_s"`
	Target       string  `json:"target"`
	Model        string  `json:"model"`
	Privacy      string  `json:"privacy"`
	LatencyClass string  `json:"latency_class"`
}

type Provider struct {
	ID           string
	Endpoint     string
	Capabilities []string
	CostPerToken float64
	RateLimit    int // req/min
}

type Gateway struct {
	ID           string
	Endpoint     string
	Capabilities []string
}

type Task struct {
	Req        Request
	Decision   Decision
	ResponseCh chan Decision
}

var (
	// Provider Registry: Now supports multiple providers for the same capability
	providers = map[string]Provider{
		"ollama-cloud-primary": {
			ID:           "ollama-cloud-primary",
			Endpoint:     "http://localhost:11434",
			Capabilities: []string{"cloud"},
			CostPerToken: 0.00001,
			RateLimit:    100,
		},
		"ollama-cloud-secondary": {
			ID:           "ollama-cloud-secondary",
			Endpoint:     "http://localhost:11434", // Same endpoint, different account/key
			Capabilities: []string{"cloud"},
			CostPerToken: 0.000005,
			RateLimit:    10,
		},
	}

	// Gateway Registry: For local inference
	gateways = map[string]Gateway{
		"rpi4-internal": {
			ID:           "rpi4-internal",
			Endpoint:     "http://localhost:11434",
			Capabilities: []string{"tiny"},
		},
		"mac-gateway": {
			ID:           "mac-gateway",
			Endpoint:     "http://localhost:8081",
			Capabilities: []string{"local"},
		},
	}

	capabilities = map[string]struct {
		local string
		cloud string
	}{
		"local-brain": {local: "granite4:3b", cloud: "gemma4:31b-cloud"},
		"local-coder": {local: "hermes3:8b", cloud: "deepseek-v4-pro:cloud"},
	}

	gatewayHealthy = make(map[string]bool)
	healthMutex    sync.RWMutex

	highPriorityQueue = make(chan *Task, 100)
	lowPriorityQueue   = make(chan *Task, 100)
	cloudActive       int32
)

func logTelemetry(t Telemetry) {
	f, err := os.OpenFile("/var/log/lattice/telemetry-control.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Telemetry error: %v\n", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(t)
	f.Write(append(b, '\n'))
}

func monitorHealth() {
	for {
		for id, gw := range gateways {
			resp, err := http.Get(gw.Endpoint + "/health")
			if err != nil || (resp != nil && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound) {
				healthMutex.Lock()
				gatewayHealthy[id] = false
				healthMutex.Unlock()
			} else {
				healthMutex.Lock()
				gatewayHealthy[id] = true
				healthMutex.Unlock()
			}
		}
		time.Sleep(10 * time.Second)
	}
}

func dispatcher() {
	semaphore := make(chan struct{}, 3)
	for {
		var task *Task
		select {
		case t := <-highPriorityQueue:
			task = t
		default:
			select {
			case t := <-lowPriorityQueue:
				task = t
			default:
				select {
				case t := <-highPriorityQueue:
					task = t
				case t := <-lowPriorityQueue:
					task = t
				}
			}
		}
		go func(t *Task) {
			semaphore <- struct{}{}
			atomic.AddInt32(&cloudActive, 1)
			t.ResponseCh <- t.Decision
			atomic.AddInt32(&cloudActive, -1)
			<-semaphore
		}(task)
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	healthMutex.RLock()
	status := make(map[string]bool)
	for k, v := range gatewayHealthy {
		status[k] = v
	}
	healthMutex.RUnlock()

	res := map[string]interface{}{
		"gateways":    status,
		"cloud_active": atomic.LoadInt32(&cloudActive),
	}
	json.NewEncoder(w).Encode(res)
}

func handleRoute(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
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

	// 1. Determine Target Capability
	var requiredCap string
	if req.Routing.Privacy == "LOCAL_ONLY" {
		requiredCap = "local"
	} else if req.Routing.LatencyClass == "interactive" {
		requiredCap = "cloud"
	} else {
		requiredCap = "local"
	}

	// 2. Routing Logic
	var targetID, endpoint, modelName string

	if requiredCap == "cloud" {
		// Find the most efficient/budget-friendly provider
		var bestProvider *Provider
		minCost := 999999.9

		for id, p := range providers {
			for _, cap := range p.Capabilities {
				if cap == "cloud" && p.CostPerToken < minCost {
					minCost = p.CostPerToken
					bestProvider = &p
				}
			}
		}

		if bestProvider != nil {
			targetID = bestProvider.ID
			endpoint = bestProvider.Endpoint
			modelName = capabilities[req.Model].cloud
		}
	} else {
		// Find a healthy gateway for 'local' or 'tiny'
		found := false
		for id, gw := range gateways {
			if healthy[id] {
				for _, cap := range gw.Capabilities {
					if cap == "local" || (requiredCap == "local" && cap == "tiny") {
						targetID = id
						endpoint = gw.Endpoint
						modelName = capabilities[req.Model].local
						found = true
						break
					}
				}
			}
			if found {
				break
			}
		}

		if !found {
			http.Error(w, "No healthy local gateway found", http.StatusServiceUnavailable)
			return
		}
	}

	if targetID == "" {
		http.Error(w, "No suitable provider found", http.StatusServiceUnavailable)
		return
	}

	decision := Decision{
		Target:    targetID,
		Endpoint:  endpoint,
		ModelName: modelName,
	}

	if targetID == "rpi4-internal" && targetID != "mac-gateway" { // if it's cloud
		respCh := make(chan Decision, 1)
		task := &Task{
			Req:        req,
			Decision:   decision,
			ResponseCh: respCh,
		}

		if req.Routing.LatencyClass == "interactive" {
			highPriorityQueue <- task
		} else {
			lowPriorityQueue <- task
		}
		decision = <-respCh
	}

	logTelemetry(Telemetry{
		RequestID:    req.Routing.RequestID,
		DecisionTime: time.Since(t0).Seconds(),
		Target:       targetID,
		Model:        modelName,
		Privacy:      req.Routing.Privacy,
		LatencyClass: req.Routing.LatencyClass,
	})

	json.NewEncoder(w).Encode(decision)
}

func main() {
	go monitorHealth()
	go dispatcher()
	http.HandleFunc("/status", handleStatus)
	http.HandleFunc("/route", handleRoute)
	fmt.Println("Lattice Control listening on :8082...")
	log.Fatal(http.ListenAndServe(":8082", nil))
}
