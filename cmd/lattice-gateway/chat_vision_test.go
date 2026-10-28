package main

import (
	"encoding/json"
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

func TestEstimateTokensCountsImages(t *testing.T) {
	base := estimateTokens([]any{map[string]any{"role": "user", "content": "hello"}}) // 5 chars -> 2
	withImg := estimateTokens([]any{partsMsg(textPart("hello"), imgPart("data:image/png;base64,"+strings.Repeat("x", 8000)))})
	if withImg <= base {
		t.Fatalf("image part not counted: base=%d with=%d", base, withImg)
	}
	// (8000+3)/4 = 2000 for the payload plus the fixed 1000-token image allowance.
	if withImg-base < 3000 || withImg-base > 3000+2 {
		t.Fatalf("image estimate %d, want ~%d", withImg-base, 2000+1000)
	}
}
