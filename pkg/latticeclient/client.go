package latticeclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type PrivacyLevel string

const (
	LocalOnly      PrivacyLevel = "LOCAL_ONLY"
	LocalPreferred PrivacyLevel = "LOCAL_PREFERRED"
	CloudAllowed   PrivacyLevel = "CLOUD_ALLOWED"
)

type LatencyClass string

const (
	Interactive LatencyClass = "interactive"
	Batch       LatencyClass = "batch"
)

type Routing struct {
	Privacy        PrivacyLevel           `json:"privacy"`
	LatencyClass   LatencyClass           `json:"latency_class"`
	Parallelism    int                    `json:"parallelism"`
	RequestID      string                 `json:"request_id"`
	ProviderParams map[string]interface{} `json:"provider_params"`
}

type Request struct {
	Model    string        `json:"model"`
	Messages []interface{} `json:"messages"`
	Routing  Routing       `json:"routing"`
}

type Response struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type LatticeClient struct {
	FrontendURL string
	HTTPClient  *http.Client
}

func NewLatticeClient(url string) *LatticeClient {
	return &LatticeClient{
		FrontendURL: url,
		HTTPClient:  &http.Client{Timeout: 60 * time.Second},
	}
}

// Execute makes a request to the Lattice Frontend
func (c *LatticeClient) Execute(model string, messages []interface{}, routing Routing) (string, error) {
	reqBody := Request{
		Model:    model,
		Messages: messages,
		Routing:  routing,
	}

	body, _ := json.Marshal(reqBody)
	resp, err := c.HTTPClient.Post(c.FrontendURL+"/v1/chat/completions", "application/json", bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("lattice error: %d", resp.StatusCode)
	}

	var res Response
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	if len(res.Choices) == 0 {
		return "", fmt.Errorf("no response from model")
	}

	return res.Choices[0].Message.Content, nil
}

// LatticeAwareAgent is a helper for agents to decide routing based on task type
type LatticeAwareAgent struct {
	Client *LatticeClient
}

func (a *LatticeAwareAgent) Ask(model string, prompt string, taskType string) (string, error) {
	var routing Routing
	routing.RequestID = fmt.Sprintf("agent-%d", time.Now().UnixNano())
	routing.Parallelism = 1

	switch taskType {
	case "urgent":
		routing.Privacy = CloudAllowed
		routing.LatencyClass = Interactive
	case "background":
		routing.Privacy = LocalPreferred
		routing.LatencyClass = Batch
	case "secret":
		routing.Privacy = LocalOnly
		routing.LatencyClass = Interactive
	default:
		routing.Privacy = CloudAllowed
		routing.LatencyClass = Interactive
	}

	messages := []interface{}{
		map[string]string{"role": "user", "content": prompt},
	}

	return a.Client.Execute(model, messages, routing)
}
