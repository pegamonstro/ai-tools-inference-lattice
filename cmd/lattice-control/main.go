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

type Telemetry struct {
	RequestID     string  `json:"request_id"`
	DecisionTime float64 `json:"decision_time_s"`
	Target       string  `json:"target"`
	Model        string  `json:"model"`
	Privacy      string  `json:"privacy"`
	LatencyClass string  `json:"latency_class"`
}

type Task struct {
	Req        Request
	Decision   Decision
	ResponseCh chan Decision
}

var (
	capabilities = map[string]struct {
		local string
		cloud string
	}{
		"local-brain": {local: "granite4:3b", cloud: "gemma4:31b-cloud"},
		"local-coder": {local: "hermes3:8b", cloud: "deepseek-v4-pro:cloud"},
	}
	macGatewayURL = "http://localhost:8081"
	cloudURL      = "http://localhost:11434"

	gatewayHealthy = true
	healthMutex    sync.RWMutex

	highPriorityQueue = make(chan *Task, 100)
	lowPriorityQueue   = make(chan *Task, 100)
	cloudActive       int32

	// Map to track active cloud requests for release
	activeRequests = make(map[string]chan struct{})
	activeMutex    sync.Mutex
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

			// Create a release channel for this request
			relCh := make(chan struct{})
			activeMutex.Lock()
			activeRequests[t.Req.Routing.RequestID] = relCh
			activeMutex.Unlock()

			t.ResponseCh <- t.Decision

			// Hold slot until release is called
			<-relCh

			activeMutex.Lock()
			delete(activeRequests, t.Req.Routing.RequestID)
			activeMutex.Unlock()

			atomic.AddInt32(&cloudActive, -1)
			<-semaphore
		}(task)
	}
}

func handleRelease(w http.ResponseWriter, r *http.Request) {
	rid := r.URL.Query().Get("request_id")
	if rid == "" {
		http.Error(w, "missing request_id", http.StatusBadRequest)
		return
	}

	activeMutex.Lock()
	relCh, ok := activeRequests[rid]
	activeMutex.Unlock()

	if !ok {
		http.Error(w, "request_id not found", http.StatusNotFound)
		return
	}

	relCh <- struct{}{}
	w.WriteHeader(http.StatusOK)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	healthMutex.RLock()
	healthy := gatewayHealthy
	healthMutex.RUnlock()

	status := map[string]interface{}{
		"gateway_healthy": healthy,
		"cloud_active":    atomic.LoadInt32(&cloudActive),
	}
	json.NewEncoder(w).Encode(status)
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

	target := "local"
	if req.Routing.LatencyClass == "interactive" && req.Routing.Privacy != "LOCAL_ONLY" {
		target = "cloud"
	}

	if !healthy && target == "local" {
		if req.Routing.Privacy == "LOCAL_ONLY" {
			http.Error(w, "Gateway unhealthy and LOCAL_ONLY requested", http.StatusServiceUnavailable)
			return
		}
		fmt.Println("  Gateway unhealthy, falling back to cloud")
		target = "cloud"
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

	if target == "cloud" {
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
		Target:       target,
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
	http.HandleFunc("/release", handleRelease)
	fmt.Println("Lattice Control listening on :8082...")
	log.Fatal(http.ListenAndServe(":8082", nil))
}
