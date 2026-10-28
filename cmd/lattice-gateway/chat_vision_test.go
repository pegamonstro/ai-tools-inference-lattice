package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tinyPNGb64 = "aGVsbG8=" // shape only; ollamaMessages never decodes bytes

func partsMsg(parts ...map[string]any) map[string]any {
	arr := make([]any, len(parts))
	for i, p := range parts {
		arr[i] = p
	}
	return map[string]any{"role": "user", "content": arr}
}

func imgPart(dataURL string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}}
}

func textPart(t string) map[string]any { return map[string]any{"type": "text", "text": t} }

func TestOllamaMessagesContentPartsImage(t *testing.T) {
	in := []any{partsMsg(textPart("judge this"), imgPart("data:image/png;base64,"+tinyPNGb64))}
	out, err := ollamaMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	msg := out[0].(map[string]any)
	if got := msg["content"]; got != "judge this" {
		t.Fatalf("content = %v", got)
	}
	imgs, ok := msg["images"].([]string)
	if !ok || len(imgs) != 1 || imgs[0] != tinyPNGb64 {
		t.Fatalf("images = %#v", msg["images"])
	}
	// The original OpenAI part shape must not survive on the translated message.
	if _, ok := msg["content"].([]any); ok {
		t.Fatal("content still an array")
	}
}

// An empty parts array is still an array: it must flatten to an empty content
// string without sprouting an images key.
func TestOllamaMessagesEmptyPartsFlatten(t *testing.T) {
	out, err := ollamaMessages([]any{map[string]any{"role": "user", "content": []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	msg := out[0].(map[string]any)
	if msg["content"] != "" || msg["images"] != nil {
		t.Fatalf("empty parts -> content %q, images %#v", msg["content"], msg["images"])
	}
}

func TestOllamaMessagesMultiImageAndTextJoin(t *testing.T) {
	in := []any{partsMsg(textPart("one"), imgPart("data:image/png;base64,A"), imgPart("data:image/jpeg;base64,B"), textPart("two"))}
	out, err := ollamaMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	msg := out[0].(map[string]any)
	if msg["content"] != "onetwo" {
		t.Fatalf("content = %v", msg["content"])
	}
	imgs := msg["images"].([]string)
	if len(imgs) != 2 || imgs[0] != "A" || imgs[1] != "B" {
		t.Fatalf("images = %#v", imgs)
	}
}

func TestOllamaMessagesStringContentUntouched(t *testing.T) {
	in := []any{map[string]any{"role": "user", "content": "plain"}, map[string]any{"role": "user", "content": []any{textPart("x")}}}
	out, err := ollamaMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].(map[string]any)["images"] != nil {
		t.Fatal("string content must not gain images")
	}
	if out[1].(map[string]any)["content"] != "x" {
		t.Fatal("parts content was not flattened")
	}
}

func TestOllamaMessagesRejectsNonDataURL(t *testing.T) {
	in := []any{partsMsg(imgPart("https://example.com/cat.png"))}
	_, err := ollamaMessages(in)
	if err == nil || !strings.Contains(err.Error(), "data:") {
		t.Fatalf("want error naming data: URLs, got %v", err)
	}
}

func TestOllamaMessagesToolCallsStillNormalize(t *testing.T) {
	in := []any{map[string]any{
		"role": "assistant", "content": "",
		"tool_calls": []any{map[string]any{
			"function": map[string]any{"name": "f", "arguments": `{"a":1}`},
		}},
	}}
	out, err := ollamaMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	call := out[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	var args map[string]any
	if err := json.Unmarshal(fn["arguments"].(json.RawMessage), &args); err != nil {
		t.Fatalf("arguments not re-emitted as object: %v", fn["arguments"])
	}
	if args["a"] != float64(1) {
		t.Fatal("arguments value lost")
	}
}

// A message can be a composite: a content-parts array with tool_calls beside
// it. Flattening must compose with the tool-call argument normalization, or a
// replayed agent turn still carries JSON-string arguments and Ollama 400s.
func TestOllamaMessagesPartsCompositeStillNormalizesToolCalls(t *testing.T) {
	in := []any{map[string]any{
		"role":    "assistant",
		"content": []any{textPart("calling tool"), imgPart("data:image/png;base64," + tinyPNGb64)},
		"tool_calls": []any{map[string]any{
			"id":       "call_x",
			"type":     "function",
			"function": map[string]any{"name": "f", "arguments": `{"a":1}`},
		}},
	}}
	out, err := ollamaMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	msg := out[0].(map[string]any)
	if msg["content"] != "calling tool" {
		t.Fatalf("content = %v", msg["content"])
	}
	if imgs, ok := msg["images"].([]string); !ok || imgs[0] != tinyPNGb64 {
		t.Fatalf("images = %#v", msg["images"])
	}
	call := msg["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	raw, ok := fn["arguments"].(json.RawMessage)
	if !ok {
		t.Fatalf("arguments still a %T, want object shape: %v", fn["arguments"], fn["arguments"])
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("arguments not re-emitted as object: %v (%v)", fn["arguments"], err)
	}
	if args["a"] != float64(1) {
		t.Fatal("arguments value lost")
	}
}

func TestEstimateTokensCountsImages(t *testing.T) {
	base := estimateTokens([]any{map[string]any{"role": "user", "content": "hello"}}) // 5 chars -> 2
	// Pre-translation parts shape (direct callers/tests).
	withImg := estimateTokens([]any{partsMsg(textPart("hello"), imgPart("data:image/png;base64,"+strings.Repeat("x", 8000)))})
	if withImg <= base {
		t.Fatalf("image part not counted: base=%d with=%d", base, withImg)
	}
	// 1000 flat allowance + min((8000+3)/4, 2048) = 1000 + 2000 = 3000.
	if withImg-base < 3000 || withImg-base > 3000+2 {
		t.Fatalf("image estimate %d, want ~%d", withImg-base, 2000+1000)
	}

	// Post-translation shape (production): images as a native base64 list. The
	// payload term caps at 2048 so a huge image can never pin num_ctx at the
	// maxContext ceiling; (9000+3)/4 = 2250 lands over the cap -> 1000 + 2048.
	translated := []any{map[string]any{"role": "user", "content": "hello", "images": []string{strings.Repeat("y", 9000)}}}
	if got := estimateTokens(translated); got-base != 2048+1000 {
		t.Fatalf("capped image estimate delta = %d, want %d", got-base, 2048+1000)
	}
}

// Production sizing must see the translated shape: after ollamaMessages a
// vision request carries flattened string content plus a native images list,
// and contextWindow must land above the text-only baseline while staying
// bounded — no image may pin num_ctx at the maxContext ceiling, because KV
// sizing is the memory-unsafe direction.
func TestContextWindowCountsImages(t *testing.T) {
	saved := maxContext
	maxContext = 32768
	defer func() { maxContext = saved }()

	const model = "vision-test"
	textOnly := []any{map[string]any{"role": "user", "content": "hello"}}
	baseCtx := contextWindow(model, textOnly, 100, nil) // estimate 2 -> minimum bucket 2048

	// Three 5000-char payloads: 1000 flat + (5000+3)/4 = 1250 cap-free each;
	// estimate 2 + 3*2250 = 6752 -> doubling lands num_ctx at 8192.
	translated := []any{map[string]any{
		"role":    "user",
		"content": "hello",
		"images":  []string{strings.Repeat("A", 5000), strings.Repeat("B", 5000), strings.Repeat("C", 5000)},
	}}
	if got := estimateTokens(translated); got != 6752 {
		t.Fatalf("translated estimate = %d, want 6752", got)
	}
	if ctx := contextWindow(model, translated, 100, nil); ctx <= baseCtx || ctx != 8192 {
		t.Fatalf("vision request num_ctx = %d, want above baseline %d at 8192", ctx, baseCtx)
	}

	// A 1 MiB base64 payload must stay bounded: 2 + (1000 + 2048 capped) -> 4096,
	// far below the 32768 ceiling.
	huge := []any{map[string]any{"role": "user", "content": "hello", "images": []string{strings.Repeat("Q", 1<<20)}}}
	if got := estimateTokens(huge); got != 2+1000+2048 {
		t.Fatalf("huge image estimate = %d, want %d", got, 2+1000+2048)
	}
	if ctx := contextWindow(model, huge, 100, nil); ctx != 4096 || ctx >= 32768 {
		t.Fatalf("huge image num_ctx = %d, want 4096, well below the 32768 ceiling", ctx)
	}
}

// newTestRouter returns a test router with a mock Ollama that returns a minimal valid response.
func newTestRouter() *httptest.ResponseRecorder {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"test","message":{"role":"assistant","content":"ok"},"prompt_eval_count":1,"eval_count":1}`))
	}))
	defer ollama.Close()

	budgeter = NewMemoryBudgeter()
	providers = map[string]Provider{"ollama": &OllamaProvider{Endpoint: ollama.URL}}
	registry = nil
	ollamaURL = ollama.URL
	inferenceSlots = newPrioritySemaphore(1, 1.0)

	mux := newRouter()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestHandleInferenceNonDataURLReturns400(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Ollama should not be called for invalid request")
	}))
	defer ollama.Close()

	budgeter = NewMemoryBudgeter()
	providers = map[string]Provider{"ollama": &OllamaProvider{Endpoint: ollama.URL}}
	registry = nil
	ollamaURL = ollama.URL
	inferenceSlots = newPrioritySemaphore(1, 1.0)

	mux := newRouter()

	body := `{"model":"test","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "data:") {
		t.Fatalf("error body should mention data: URLs: %s", w.Body.String())
	}
}

func TestHandleInferenceStreamNonDataURLReturns400(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Ollama should not be called for invalid request")
	}))
	defer ollama.Close()

	budgeter = NewMemoryBudgeter()
	providers = map[string]Provider{"ollama": &OllamaProvider{Endpoint: ollama.URL}}
	registry = nil
	ollamaURL = ollama.URL
	inferenceSlots = newPrioritySemaphore(1, 1.0)

	mux := newRouter()

	body := `{"model":"test","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "data:") {
		t.Fatalf("error body should mention data: URLs: %s", w.Body.String())
	}
}

func TestHandleInferenceMalformedPartReturns400(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Ollama should not be called for invalid request")
	}))
	defer ollama.Close()

	budgeter = NewMemoryBudgeter()
	providers = map[string]Provider{"ollama": &OllamaProvider{Endpoint: ollama.URL}}
	registry = nil
	ollamaURL = ollama.URL
	inferenceSlots = newPrioritySemaphore(1, 1.0)

	mux := newRouter()

	// Content part that is not an object (string instead of object)
	body := `{"model":"test","messages":[{"role":"user","content":["not an object"]}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
