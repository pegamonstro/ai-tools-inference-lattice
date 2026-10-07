package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
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
	// Locality sits beside Target, never instead of it: Target stays the source
	// of truth and the key the per-target views are built on.
	Locality string `json:"locality"`
}

type Telemetry struct {
	RequestID    string  `json:"request_id"`
	DecisionTime float64 `json:"decision_time_s"`
	Target       string  `json:"target"`
	Model        string  `json:"model"`
	Privacy      string  `json:"privacy"`
	LatencyClass string  `json:"latency_class"`
	Locality     string  `json:"locality"`
	RequiredCap  string  `json:"required_cap,omitempty"`
	// Who asked. The frontend forwards its own caller here as X-Lattice-Client;
	// an un-headered peer (an older frontend mixed into a rollout) shows as the
	// address control actually received the decision request from.
	Client string `json:"client,omitempty"`
	Error  string `json:"error,omitempty"`
}

type Gateway struct {
	ID           string
	Endpoint     string
	Capabilities []string // locality ("local","tiny","cloud") + modality
	Models       []string // concrete models served; empty = wildcard (cloud)
	Slots        int      // local concurrency ceiling (0 = not slot-limited)
	CostPerToken float64  // cloud only
	RateLimit    int      // cloud only, req/min
	MaxContext   int      // announced context ceiling
	Priority     int      // local only: routing preference, lower wins
}

// gatewayAnnouncement is the /health payload the gateway publishes.
type gatewayAnnouncement struct {
	MaxContext   int      `json:"max_context"`
	Capabilities []string `json:"capabilities"`
	Slots        int      `json:"slots"`
	Models       []string `json:"models"`
}

func decodeHealth(body []byte) (gatewayAnnouncement, error) {
	var a gatewayAnnouncement
	err := json.Unmarshal(body, &a)
	return a, err
}

func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// hostsModel reports whether a local gateway serves a concrete model. An empty
// Models list means "announces nothing" — it hosts nothing, so a LOCAL_ONLY
// request fails closed until the gateway has announced. (Cloud is a wildcard:
// a subscription hosts any ":cloud" model, and selectGateway skips this check
// for cloud rather than encode the wildcard here.)
func hostsModel(models []string, model string) bool {
	for _, m := range models {
		if m == model {
			return true
		}
	}
	return false
}

// selectGateway is the adequacy routing rule: filter on health (announced
// gateways only), capability, model-hosting, and slot; then score cloud by
// lowest cost and local by highest priority. Priority is the tiebreak when two
// healthy gateways host the same model: the lower the Priority value, the more
// preferred. Cloud gateways are wildcards — no health poll, no hosting check,
// no slot — so their "adequacy" is cost and rate-limit alone. It returns an
// error when nothing is adequate, so the caller fails loudly rather than
// reroutes.
func selectGateway(requiredCap, model string, registry map[string]Gateway, healthy map[string]bool) (Gateway, error) {
	var best Gateway
	bestCost := math.MaxFloat64
	found := false

	for _, gw := range registry {
		if !hasCapability(gw.Capabilities, requiredCap) {
			continue
		}
		isLocal := hasCapability(gw.Capabilities, "local") || hasCapability(gw.Capabilities, "tiny")
		if isLocal {
			if !healthy[gw.ID] {
				continue
			}
			if !hostsModel(gw.Models, model) {
				continue
			}
			if gw.Slots == 0 {
				continue
			}
		}
		if requiredCap == "cloud" {
			if gw.CostPerToken < bestCost {
				bestCost = gw.CostPerToken
				best = gw
				found = true
			}
		} else if !found || gw.Priority < best.Priority {
			// First adequate local gateway, or one with a higher priority
			// (lower Priority value). Deterministic: priority comes from
			// declaration order, not map iteration order.
			best = gw
			found = true
		}
	}
	if !found {
		return Gateway{}, fmt.Errorf("no adequate gateway for capability %q model %q", requiredCap, model)
	}
	return best, nil
}

type Task struct {
	Req        Request
	Decision   Decision
	ResponseCh chan Decision
}

var (
	// gateway registry: every inference target, cloud and local, as one type.
	// Cloud entries are declared; local entries have Models/Slots/MaxContext
	// filled from the gateway's /health announcement.
	gateways = map[string]Gateway{}
)

func init() {
	ollamaURL := latticeconfig.Env("LATTICE_OLLAMA_URL", "http://localhost:11434")

	gateways["ollama-cloud-primary"] = Gateway{
		ID:           "ollama-cloud-primary",
		Endpoint:     ollamaURL,
		Capabilities: []string{"cloud"},
		CostPerToken: 0.00001,
		RateLimit:    100,
	}
	gateways["ollama-cloud-secondary"] = Gateway{
		ID:           "ollama-cloud-secondary",
		Endpoint:     ollamaURL, // Same endpoint, different account/key
		Capabilities: []string{"cloud"},
		CostPerToken: 0.000005,
		RateLimit:    10,
	}

	// Local gateways are declared as an id=endpoint list; the health poll fills
	// in each one's Models/Slots/MaxContext from its /health announcement. The
	// default keeps the single-gateway deployment unchanged: LATTICE_GATEWAY_URL
	// names the sole local host, registered as "mac-gateway".
	localList := latticeconfig.Env("LATTICE_LOCAL_GATEWAYS",
		"mac-gateway="+latticeconfig.Env("LATTICE_GATEWAY_URL", "http://localhost:8081"))
	for _, gw := range parseLocalGatewayList(localList) {
		gateways[gw.ID] = gw
	}
}

// parseLocalGatewayList turns an "id=endpoint,id=endpoint" declaration into the
// corresponding local Gateway entries. Malformed entries are skipped and logged
// rather than failing the whole plane, so one bad line cannot take down routing.
//
// Declaration order is the routing priority: the first entry is preferred when
// two gateways host the same model. This is how "M6 first, M1 fallback" is
// expressed without a separate weight field in config.
func parseLocalGatewayList(list string) []Gateway {
	var out []Gateway
	for i, entry := range strings.Split(list, ",") {
		id, endpoint, ok := strings.Cut(strings.TrimSpace(entry), "=")
		id, endpoint = strings.TrimSpace(id), strings.TrimSpace(endpoint)
		if !ok || id == "" || endpoint == "" {
			fmt.Printf("skipping malformed local gateway declaration %q\n", entry)
			continue
		}
		out = append(out, Gateway{ID: id, Endpoint: endpoint, Capabilities: []string{"local"}, Priority: i})
	}
	return out
}

// localityFor reports which registry a target was chosen from. It is derived
// rather than tracked per branch so that no exit path can forget it, and so that
// a new target is classified by where it was registered rather than by its name.
//
// The values are the same strings the registries already use in Capabilities, so
// there is one vocabulary and no translation table to drift.
func localityFor(targetID string) string {
	healthMutex.RLock()
	gw, ok := gateways[targetID]
	healthMutex.RUnlock()
	if !ok {
		return "unknown"
	}
	if hasCapability(gw.Capabilities, "cloud") {
		return "cloud"
	}
	return "local"
}

var (
	capabilities = map[string]struct {
		local string
		cloud string
	}{
		// general → MoE (fast, few active params per token); coding → dense
		// (full attention, 32 K context). Measured 2026-09-28, docs/measurements.md.
		"local-brain": {local: "gemma4:12b", cloud: "gemma4:31b-cloud"},
		"local-coder": {local: "qwen2.5-coder:3b", cloud: "deepseek-v4-pro:cloud"},
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
			if !hasCapability(gw.Capabilities, "local") && !hasCapability(gw.Capabilities, "tiny") {
				continue // declared cloud: no /health, no telemetry relay
			}
			resp, err := http.Get(gw.Endpoint + "/health")
			healthy := err == nil && resp != nil &&
				(resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound)
			if healthy && resp != nil && resp.StatusCode == http.StatusOK {
				body, rerr := io.ReadAll(resp.Body)
				if rerr == nil {
					if a, derr := decodeHealth(body); derr == nil {
						healthMutex.Lock()
						g2 := gateways[id]
						g2.Models = a.Models
						g2.Slots = a.Slots
						g2.MaxContext = a.MaxContext
						if len(a.Capabilities) > 0 {
							g2.Capabilities = a.Capabilities
						}
						gateways[id] = g2
						healthMutex.Unlock()
					}
				}
			}
			if resp != nil {
				resp.Body.Close()
			}
			healthMutex.Lock()
			gatewayHealthy[id] = healthy
			healthMutex.Unlock()
			pullGatewayTelemetry(id, gw)
		}
		time.Sleep(10 * time.Second)
	}
}

// gatewayRelay is where the relay has read to for one gateway: the seq of the
// last event appended locally, and the identity of the gateway process that
// issued it. known records whether they mean anything yet — see planRelay.
//
// The cursor is per-gateway, not global: each gateway's counter and boot id are
// its own, so a shared state would have every alternating poll flip the boot id
// and re-relay both rings in full. Each cursor is recovered from the relay file
// at startup, where relayed rows carry the gateway that produced them.
type gatewayRelay struct {
	seq   int64
	boot  string
	known bool
}

var (
	gatewayRelayMu      sync.Mutex
	gatewayRelayCursors = map[string]gatewayRelay{}
)

func saveRelayCursor(gwID string, st gatewayRelay) {
	gatewayRelayMu.Lock()
	gatewayRelayCursors[gwID] = st
	gatewayRelayMu.Unlock()
}

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

// recoverRelayCursors reads where the relay has already delivered each gateway,
// so a control-plane restart resumes where it left off rather than seeding past
// the events that arrived while it was down. The relay file is the durable
// record of what has been delivered, so this needs no separate state; rows are
// attributed by the gateway field stamped with each relayed event. Rows without
// that field predate attribution (or are torn) and cannot be trusted to any
// gateway, so they are skipped — those gateways seed, at most once.
func recoverRelayCursors(path string) map[string]gatewayRelay {
	out := map[string]gatewayRelay{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var row struct {
			Seq     int64  `json:"seq"`
			Boot    string `json:"boot"`
			Gateway string `json:"gateway"`
		}
		if err := json.Unmarshal(line, &row); err != nil || row.Seq <= 0 || row.Gateway == "" {
			continue
		}
		out[row.Gateway] = gatewayRelay{seq: row.Seq, boot: row.Boot, known: true}
	}
	return out
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
func gapMarker(gwID string, seq int64, note string, lost int64) []byte {
	msg := fmt.Sprintf("%s (count unknown)", note)
	if lost >= 0 {
		msg = fmt.Sprintf("%s (%d lost)", note, lost)
	}
	b, _ := json.Marshal(map[string]interface{}{
		"gateway":    gwID,
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
func pullGatewayTelemetry(gwID string, gw Gateway) {
	gatewayRelayMu.Lock()
	st := gatewayRelayCursors[gwID]
	gatewayRelayMu.Unlock()

	payload, err := fetchGatewayTelemetry(gw.Endpoint, st.seq)
	if err != nil {
		return
	}

	// A boot id we have never seen only counts as a change once we have one to
	// compare against: on the first poll there is nothing to contradict.
	bootChanged := st.boot != "" && payload.Boot != "" && payload.Boot != st.boot

	action := planRelay(st.known, st.seq, payload.Seq, payload.OldestSeq, bootChanged)
	if action.Seed {
		saveRelayCursor(gwID, gatewayRelay{seq: action.Next, boot: payload.Boot, known: true})
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
		if err := appendRelay(path, stampRelayEvent(gwID, ev)); err != nil {
			fmt.Printf("Gateway telemetry relay error: %v\n", err)
			return
		}
	}

	// Written after the surviving events, so it reads chronologically and the
	// file's final line for this gateway still carries the cursor a later
	// restart recovers from.
	if action.GapNote != "" {
		fmt.Printf("Gateway telemetry gap: %s\n", action.GapNote)
		if err := appendRelay(path, gapMarker(gwID, action.Next, action.GapNote, action.Lost)); err != nil {
			fmt.Printf("Gateway telemetry relay error: %v\n", err)
		}
	}

	saveRelayCursor(gwID, gatewayRelay{seq: action.Next, boot: payload.Boot, known: true})
}

// stampRelayEvent tags an event with the gateway that produced it before it is
// appended, so the relay file can attribute its rows for restart recovery —
// two gateways restart their counters independently, and an unattributed seq
// cannot be trusted to either. Events that fail to parse are relayed unmodified
// rather than dropped; an already-stamped event (a forward-compatible gateway)
// passes through untouched.
func stampRelayEvent(gwID string, ev json.RawMessage) []byte {
	var row map[string]json.RawMessage
	if err := json.Unmarshal(ev, &row); err != nil {
		return []byte(ev)
	}
	if _, exists := row["gateway"]; exists {
		return []byte(ev)
	}
	idb, err := json.Marshal(gwID)
	if err != nil {
		return []byte(ev)
	}
	row["gateway"] = idb
	out, err := json.Marshal(row)
	if err != nil {
		return []byte(ev)
	}
	return out
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
	type gwView struct {
		ID           string
		Capabilities []string
		Slots        int
		Models       []string
		MaxContext   int
	}

	healthMutex.RLock()
	// context_length is the ceiling every local gateway honours, so a client is
	// never promised a context its model cannot serve.
	ctxCap := 0
	views := make([]gwView, 0)
	for id, gw := range gateways {
		if !hasCapability(gw.Capabilities, "local") && !hasCapability(gw.Capabilities, "tiny") {
			continue
		}
		if ctxCap == 0 || gw.MaxContext < ctxCap {
			ctxCap = gw.MaxContext
		}
		views = append(views, gwView{id, gw.Capabilities, gw.Slots, gw.Models, gw.MaxContext})
	}
	healthMutex.RUnlock()
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })

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

	gatewayList := make([]map[string]interface{}, 0, len(views))
	for _, v := range views {
		gatewayList = append(gatewayList, map[string]interface{}{
			"id":           v.ID,
			"capabilities": v.Capabilities,
			"slots":        v.Slots,
			"models":       v.Models,
			"max_context":  v.MaxContext,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"context_length": ctxCap,
		"capabilities":   list,
		"gateways":       gatewayList,
	})
}

// remoteHost strips the port an address rides on; callerID prefers the value
// the frontend forwarded about its own caller.
func callerID(r *http.Request) string {
	if c := r.Header.Get("X-Lattice-Client"); c != "" {
		return c
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func handleRoute(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	// Who to attribute the decision to: the frontend's forwarded caller, else
	// the peer that reached this route directly.
	caller := callerID(r)

	var req Request

	// Deferred so that every exit path draws exactly one line. A refusal is the
	// event the routing policy exists to produce, so a failure path that returned
	// before this write would leave the policy unobservable at the moment it
	// acted.
	tele := Telemetry{Client: caller}
	defer func() {
		tele.RequestID = req.Routing.RequestID
		tele.DecisionTime = time.Since(t0).Seconds()
		tele.Privacy = req.Routing.Privacy
		tele.LatencyClass = req.Routing.LatencyClass
		// Derived from the target the body settled on, so every exit path is
		// covered by construction: the refusal paths never set a target, and
		// localityFor("") is "unknown".
		tele.Locality = localityFor(tele.Target)
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

	// 1. Determine Target Capability
	requiredCap, err := requiredCapability(req.Routing.Privacy, req.Routing.LatencyClass, req.Model)
	if err != nil {
		tele.Error = err.Error()
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	tele.RequiredCap = requiredCap

	// 2. Routing Logic
	healthMutex.RLock()
	registry := make(map[string]Gateway, len(gateways))
	for k, v := range gateways {
		registry[k] = v
	}
	healthy := make(map[string]bool, len(gatewayHealthy))
	for k, v := range gatewayHealthy {
		healthy[k] = v
	}
	healthMutex.RUnlock()

	var targetID, endpoint, modelName string

	concrete := resolveModel(req.Model, requiredCap)
	gw, err := selectGateway(requiredCap, concrete, registry, healthy)
	if err != nil {
		tele.Error = err.Error()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	targetID = gw.ID
	endpoint = gw.Endpoint
	modelName = concrete

	tele.Target = targetID
	tele.Model = modelName

	decision := Decision{
		Target:    targetID,
		Endpoint:  endpoint,
		ModelName: modelName,
		Locality:  localityFor(targetID),
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
	gatewayRelayCursors = recoverRelayCursors(gatewayRelayPath())
	if len(gatewayRelayCursors) > 0 {
		names := make([]string, 0, len(gatewayRelayCursors))
		for id, st := range gatewayRelayCursors {
			names = append(names, fmt.Sprintf("%s@%d(%s)", id, st.seq, st.boot))
		}
		sort.Strings(names)
		fmt.Printf("Gateway telemetry relay resuming: %s\n", strings.Join(names, ", "))
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
