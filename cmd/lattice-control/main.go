package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pegamonstro/ai-tools-inference-lattice/pkg/latticeconfig"
)

type Routing struct {
	Privacy        string                 `json:"privacy"`
	LatencyClass   string                 `json:"latency_class"`
	Parallelism    int                    `json:"parallelism"`
	RequestID      string                 `json:"request_id"`
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
	RequestID    string  `json:"request_id"`
	DecisionTime float64 `json:"decision_time_s"`
	Target       string  `json:"target"`
	Model        string  `json:"model"`
	Privacy      string  `json:"privacy"`
	LatencyClass string  `json:"latency_class"`
	Error        string  `json:"error,omitempty"`
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
	providers = map[string]Provider{}
	// Gateway Registry: For local inference
	gateways = map[string]Gateway{}
)

func init() {
	ollamaURL := latticeconfig.Env("LATTICE_OLLAMA_URL", "http://localhost:11434")
	gatewayURL := latticeconfig.Env("LATTICE_GATEWAY_URL", "http://localhost:8081")

	providers["ollama-cloud-primary"] = Provider{
		ID:           "ollama-cloud-primary",
		Endpoint:     ollamaURL,
		Capabilities: []string{"cloud"},
		CostPerToken: 0.00001,
		RateLimit:    100,
	}
	providers["ollama-cloud-secondary"] = Provider{
		ID:           "ollama-cloud-secondary",
		Endpoint:     ollamaURL, // Same endpoint, different account/key
		Capabilities: []string{"cloud"},
		CostPerToken: 0.000005,
		RateLimit:    10,
	}

	gateways["mac-gateway"] = Gateway{
		ID:           "mac-gateway",
		Endpoint:     gatewayURL,
		Capabilities: []string{"local"},
	}
}

var (
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
	lowPriorityQueue  = make(chan *Task, 100)
	cloudActive       int32
)

var telemetryPath = latticeconfig.Env("LATTICE_CONTROL_TELEMETRY", "/var/log/lattice/telemetry-control.jsonl")

func logTelemetry(t Telemetry) {
	f, err := os.OpenFile(telemetryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
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
			healthy := err == nil && resp != nil &&
				(resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound)
			if healthy && resp.StatusCode == http.StatusOK {
				var h struct {
					MaxContext int `json:"max_context"`
				}
				if json.NewDecoder(resp.Body).Decode(&h) == nil && h.MaxContext > 0 {
					healthMutex.Lock()
					gatewayMaxContext = h.MaxContext
					healthMutex.Unlock()
				}
			}
			if resp != nil {
				resp.Body.Close()
			}
			healthMutex.Lock()
			gatewayHealthy[id] = healthy
			healthMutex.Unlock()
			pullGatewayTelemetry(gw)
		}
		time.Sleep(10 * time.Second)
	}
}

// gatewayTelemetrySeq and gatewayTelemetryBoot describe where the relay has
// read to: the seq of the last event appended locally, and the identity of the
// gateway process that issued it. Both are recovered from the relay file at
// startup, since a seq alone cannot be trusted across a gateway restart.
// gatewayRelayKnown records whether they mean anything yet — see planRelay.
var (
	gatewayTelemetrySeq  int64
	gatewayTelemetryBoot string
	gatewayRelayKnown    bool

	// gatewayMaxContext is what the gateway reports it will honour, refreshed on
	// each health poll. It is advertised by /capabilities so the limit is
	// discoverable rather than invisible.
	gatewayMaxContext int
)

func gatewayRelayPath() string {
	return latticeconfig.Env("LATTICE_GATEWAY_TELEMETRY_LOCAL", "/var/log/lattice/telemetry-gateway.jsonl")
}

// relayAction is what a single poll of the gateway's stream should do.
type relayAction struct {
	Seed    bool   // first pull: adopt the producer's seq and deliver nothing
	Fetch   bool   // re-fetch from Since before delivering, to recover a gap
	Since   int64  // cursor for that re-fetch
	Next    int64  // cursor to adopt once delivery is done
	GapNote string // non-empty when events were provably skipped
	Lost    int64  // events known to be unrecoverable; -1 when unknowable
}

// planRelay decides how the relay should proceed for one poll.
//
// The gateway's seq lives in memory and its ring is bounded, so a poll can
// arrive on the far side of a discontinuity in two ways: the gateway restarted
// (its counter began again, so the numbers we hold were re-issued) or the ring
// evicted events the consumer never received. The response cannot report
// either — a hole looks exactly like a quiet stream — so the consumer detects
// them by comparing its cursor against the gateway's identity and against both
// ends of what it still retains.
//
// known says whether the cursor means anything yet. It cannot be inferred from
// the cursor itself: zero is both "we have read nothing" and a legitimate
// resynced position, and conflating them makes every later poll re-seed and
// swallow the next event.
//
// bootChanged says the gateway is a different process than the one that issued
// our cursor. It is what catches a restart whose counter has not yet fallen
// behind the cursor, which the seq comparison alone cannot see.
func planRelay(known bool, cursor, producerSeq, oldestSeq int64, bootChanged bool) relayAction {
	if !known {
		// Nothing recovered from the relay file: everything buffered predates
		// this process, so replaying it would duplicate lines already on the
		// Bee screen. Seed and deliver nothing.
		return relayAction{Seed: true, Next: producerSeq, Lost: -1}
	}

	restarted := bootChanged || producerSeq < cursor
	evicted := oldestSeq > cursor+1
	if !restarted && !evicted {
		return relayAction{Next: producerSeq, Lost: -1}
	}

	// Resyncing recovers everything the ring still holds, so the only events
	// gone for good are the ones it has already evicted — and after a restart
	// the count runs from 1, because that is where the new counter began.
	//
	// A restart polled before the new run has logged anything costs nothing,
	// and must not be reported as a loss: restarts are routine now, and a red
	// line on every one would train the display's reader to ignore it. The
	// unknown case is a gateway too old to report oldest_seq, where nothing can
	// be said about what it discarded.
	lost := int64(-1)
	switch {
	case restarted && producerSeq == 0:
		lost = 0
	case restarted && oldestSeq > 0:
		lost = oldestSeq - 1
	case evicted:
		lost = oldestSeq - cursor - 1
	}

	action := relayAction{Next: producerSeq, Lost: lost}
	if oldestSeq > 0 {
		action.Fetch = true
		action.Since = oldestSeq - 1
	}
	// A restart that cost nothing needs no line on the display; only real loss
	// earns a marker, so a benign restart does not read as a failure.
	if lost != 0 {
		if restarted {
			action.GapNote = "gateway restarted, its event counter began again"
		} else {
			action.GapNote = fmt.Sprintf("%d events evicted before they were relayed", lost)
		}
	}
	return action
}

type gatewayTelemetryPayload struct {
	Seq       int64             `json:"seq"`
	OldestSeq int64             `json:"oldest_seq"`
	Boot      string            `json:"boot"`
	Events    []json.RawMessage `json:"events"`
}

func fetchGatewayTelemetry(endpoint string, since int64) (*gatewayTelemetryPayload, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("%s/telemetry?since=%d", endpoint, since))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway telemetry: status %d", resp.StatusCode)
	}
	var payload gatewayTelemetryPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return &payload, nil
}

// recoverRelayState reads the seq and boot id of the last event already
// relayed, so a control-plane restart resumes where it left off rather than
// seeding past the events that arrived while it was down. The relay file is
// already the durable record of what has been delivered, so this needs no new
// state. A zero seq means nothing is known, which the caller treats as a fresh
// start.
func recoverRelayState(path string) (seq int64, boot string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, ""
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return 0, ""
	}
	const tailBytes = 8192
	if off := st.Size() - tailBytes; off > 0 {
		// Landing mid-row only spoils the first line, which we never want.
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return 0, ""
		}
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return 0, ""
	}

	lines := strings.Split(string(buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var row struct {
			Seq  int64  `json:"seq"`
			Boot string `json:"boot"`
		}
		if err := json.Unmarshal([]byte(line), &row); err == nil && row.Seq > 0 {
			return row.Seq, row.Boot
		}
	}
	return 0, ""
}

// appendRelay appends one already-encoded line to the relay file.
func appendRelay(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// gapMarker renders a lost-event gap into the relay stream so it reaches the
// Bee display instead of only a log nobody reads. It deliberately carries
// elapsed_s — the key the feeder branches on for gateway events — plus error,
// which is how the feeder already renders a failure distinctly. The seq is the
// real cursor, so a later restart still recovers from the file's last line.
func gapMarker(seq int64, note string, lost int64) []byte {
	msg := fmt.Sprintf("%s (count unknown)", note)
	if lost >= 0 {
		msg = fmt.Sprintf("%s (%d lost)", note, lost)
	}
	b, _ := json.Marshal(map[string]interface{}{
		"seq":        seq,
		"request_id": "telemetry-gap",
		"model":      "relay",
		"elapsed_s":  0,
		"error":      msg,
	})
	return b
}

// Relaying the gateway's runtime telemetry: the gateway lives on the Mac with
// no shared filesystem, so we pull its buffered events over HTTP and append
// them to a local JSONL — the same stream the Bee feeder tails for the
// control/frontend events. The control plane is a transport here, not a
// formatter: it never emits to the Bee socket itself.
func pullGatewayTelemetry(gw Gateway) {
	payload, err := fetchGatewayTelemetry(gw.Endpoint, gatewayTelemetrySeq)
	if err != nil {
		return
	}

	// A boot id we have never seen only counts as a change once we have one to
	// compare against: on the first poll there is nothing to contradict.
	bootChanged := gatewayTelemetryBoot != "" && payload.Boot != "" && payload.Boot != gatewayTelemetryBoot

	action := planRelay(gatewayRelayKnown, gatewayTelemetrySeq, payload.Seq, payload.OldestSeq, bootChanged)
	if action.Seed {
		gatewayTelemetrySeq, gatewayTelemetryBoot = action.Next, payload.Boot
		gatewayRelayKnown = true
		return
	}

	// On a gap the cursor asked for events the gateway can no longer describe,
	// so re-fetch from the oldest it still holds rather than delivering the
	// empty answer that produced the gap in the first place.
	if action.Fetch {
		refetched, err := fetchGatewayTelemetry(gw.Endpoint, action.Since)
		if err != nil {
			return
		}
		payload = refetched
	}

	path := gatewayRelayPath()
	for _, ev := range payload.Events {
		if err := appendRelay(path, ev); err != nil {
			fmt.Printf("Gateway telemetry relay error: %v\n", err)
			return
		}
	}

	// Written after the surviving events, so it reads chronologically and the
	// file's final line still carries the cursor a later restart recovers from.
	if action.GapNote != "" {
		fmt.Printf("Gateway telemetry gap: %s\n", action.GapNote)
		if err := appendRelay(path, gapMarker(action.Next, action.GapNote, action.Lost)); err != nil {
			fmt.Printf("Gateway telemetry relay error: %v\n", err)
		}
	}

	gatewayTelemetrySeq, gatewayTelemetryBoot = action.Next, payload.Boot
	gatewayRelayKnown = true
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
		"gateways":     status,
		"cloud_active": atomic.LoadInt32(&cloudActive),
	}
	json.NewEncoder(w).Encode(res)
}

// resolveModel maps the client's model field to the name actually sent to the
// target.
//
// A capability alias is a request for policy to choose the model, so it is
// resolved per target. Anything else is a literal model name the client chose,
// and it is passed through verbatim — the previous code looked the name up in
// the capability map and used the zero value on a miss, so a literal model
// arrived at the target as an empty string and failed with nothing in telemetry
// naming what had been asked for. The rule is that model_name is never blank
// when the client named something.
func resolveModel(model, target string) string {
	c, ok := capabilities[model]
	if !ok {
		return model
	}
	if target == "cloud" {
		return c.cloud
	}
	return c.local
}

// isCloudModel reports whether a literal model name is one Ollama hosts in the
// cloud rather than on this machine. Ollama marks those in the tag — `cloud`,
// or `<size>-cloud`.
//
// The marker is the only locality signal a client can carry: an OpenAI-SDK
// agent names a model and sends no routing envelope, so without reading it a
// cloud model is routed to the Mac, which does not have it, and 404s.
//
// An untagged name is never cloud. Guessing here could send a local model to
// the cloud, which LOCAL_ONLY forbids; failing loudly at the local target is the
// safe direction.
func isCloudModel(model string) bool {
	i := strings.LastIndex(model, ":")
	if i < 0 {
		return false
	}
	tag := model[i+1:]
	return tag == "cloud" || strings.HasSuffix(tag, "-cloud")
}

// requiredCapability decides whether a request is served locally or in the cloud.
//
// Three inputs, in priority order. Privacy is a hard constraint. Model locality
// comes next, because a cloud-hosted model cannot be served by the Mac at all,
// so routing it local can only fail at the target. The latency class is last:
// it is a preference, and the default when nothing else applies is local, which
// is the machine that is actually ours.
//
// A LOCAL_ONLY request naming a cloud model is irreconcilable and is refused —
// never quietly promoted to cloud, and never quietly rerouted to a target that
// does not have the model.
func requiredCapability(privacy, latencyClass, model string) (string, error) {
	if isCloudModel(model) {
		if privacy == "LOCAL_ONLY" {
			return "", fmt.Errorf("LOCAL_ONLY cannot be served by the cloud-only model %q", model)
		}
		return "cloud", nil
	}
	if privacy == "LOCAL_ONLY" {
		return "local", nil
	}
	if latencyClass == "interactive" {
		return "cloud", nil
	}
	return "local", nil
}

// handleCapabilities exposes the client-facing namespace — the capability
// aliases — plus the ceiling the gateway will actually honour. A probing client
// that finds this stops concluding the API is absent, which is what happened
// when the only route was the chat endpoint.
func handleCapabilities(w http.ResponseWriter, r *http.Request) {
	healthMutex.RLock()
	ctxCap := gatewayMaxContext
	healthMutex.RUnlock()

	ids := make([]string, 0, len(capabilities))
	for id := range capabilities {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	list := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		list = append(list, map[string]string{
			"id":    id,
			"local": capabilities[id].local,
			"cloud": capabilities[id].cloud,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"context_length": ctxCap,
		"capabilities":   list,
	})
}

func handleRoute(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()

	var req Request

	// Deferred so that every exit path draws exactly one line. A refusal is the
	// event the routing policy exists to produce, so a failure path that returned
	// before this write would leave the policy unobservable at the moment it
	// acted.
	tele := Telemetry{}
	defer func() {
		tele.RequestID = req.Routing.RequestID
		tele.DecisionTime = time.Since(t0).Seconds()
		tele.Privacy = req.Routing.Privacy
		tele.LatencyClass = req.Routing.LatencyClass
		logTelemetry(tele)
	}()

	bodyBytes, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		tele.Error = err.Error()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Until a decision names the resolved model, the request is identified by
	// what it asked for — a request that never got a decision still has a name.
	tele.Model = req.Model

	fmt.Printf("Routing Request [%s]: Model=%s, Privacy=%s, Latency=%s\n",
		req.Routing.RequestID, req.Model, req.Routing.Privacy, req.Routing.LatencyClass)

	healthMutex.RLock()
	healthy := gatewayHealthy
	healthMutex.RUnlock()

	// 1. Determine Target Capability
	requiredCap, err := requiredCapability(req.Routing.Privacy, req.Routing.LatencyClass, req.Model)
	if err != nil {
		tele.Error = err.Error()
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	// 2. Routing Logic
	var targetID, endpoint, modelName string

	if requiredCap == "cloud" {
		// Find the cheapest provider that supports the "cloud" capability.
		var bestProvider Provider
		found := false
		minCost := math.MaxFloat64

		for _, p := range providers {
			for _, cap := range p.Capabilities {
				if cap == "cloud" && p.CostPerToken < minCost {
					minCost = p.CostPerToken
					bestProvider = p
					found = true
				}
			}
		}

		if found {
			targetID = bestProvider.ID
			endpoint = bestProvider.Endpoint
			modelName = resolveModel(req.Model, "cloud")
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
						modelName = resolveModel(req.Model, "local")
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
			tele.Error = "No healthy local gateway found"
			http.Error(w, "No healthy local gateway found", http.StatusServiceUnavailable)
			return
		}
	}

	if targetID == "" {
		tele.Error = "No suitable provider found"
		http.Error(w, "No suitable provider found", http.StatusServiceUnavailable)
		return
	}

	tele.Target = targetID
	tele.Model = modelName

	decision := Decision{
		Target:    targetID,
		Endpoint:  endpoint,
		ModelName: modelName,
	}

	if requiredCap == "cloud" {
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

	json.NewEncoder(w).Encode(decision)
}

func main() {
	// Resume the gateway relay where the last run left off. Without this the
	// cursor starts at zero and the first pull seeds past everything buffered
	// while this process was down — events that were never relayed to anyone.
	gatewayTelemetrySeq, gatewayTelemetryBoot = recoverRelayState(gatewayRelayPath())
	gatewayRelayKnown = gatewayTelemetrySeq > 0
	if gatewayRelayKnown {
		fmt.Printf("Gateway telemetry relay resuming at seq %d (boot %s)\n", gatewayTelemetrySeq, gatewayTelemetryBoot)
	}

	go monitorHealth()
	go dispatcher()
	http.HandleFunc("/status", handleStatus)
	http.HandleFunc("/capabilities", handleCapabilities)
	http.HandleFunc("/route", handleRoute)
	addr := latticeconfig.Env("LATTICE_CONTROL_ADDR", ":8082")
	fmt.Printf("Lattice Control listening on %s...\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
