package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testServer points the store at a fresh temp dir and returns the mux ready
// for requests. Every test gets its own store — no disk writes escape them.
func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	blobRoot = t.TempDir()
	telemetryPath = filepath.Join(blobRoot, "telemetry-modelrepo.jsonl")
	for _, sub := range []string{"blobs", filepath.Join("ollama", "manifests")} {
		if err := os.MkdirAll(filepath.Join(blobRoot, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(newMux())
	t.Cleanup(srv.Close)
	return srv
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// putBlob stages bytes at offset 0 then finalizes, returning the response.
func putBlob(srv *httptest.Server, sha string, body []byte, offset int64) *http.Response {
	var req *http.Request
	if offset > 0 {
		req, _ = http.NewRequest(http.MethodPut, srv.URL+"/v1/blobs/"+sha+"?part="+fmt.Sprint(offset), bytes.NewReader(body))
	} else {
		req, _ = http.NewRequest(http.MethodPut, srv.URL+"/v1/blobs/"+sha, bytes.NewReader(body))
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func mustBody(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestBlobPushFinalizeAndDownload(t *testing.T) {
	srv := testServer(t)
	content := []byte("gemma-weights-payload")
	sha := digest(content)

	if resp := putBlob(srv, sha, content, 0); resp.StatusCode != 200 {
		t.Fatalf("PUT: %d: %s", resp.StatusCode, mustBody(t, resp))
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/blobs/"+sha+"/finalize", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("finalize: %d: %s", resp.StatusCode, mustBody(t, resp))
	}

	// The finalized name is content-addressed; no .part residue.
	if _, err := os.Stat(filepath.Join(blobRoot, "blobs", sha)); err != nil {
		t.Fatalf("final blob missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(blobRoot, "blobs", sha+".part")); !os.IsNotExist(err) {
		t.Fatalf("part file not promoted")
	}

	head, _ := http.NewRequest(http.MethodHead, srv.URL+"/v1/blobs/"+sha, nil)
	hresp, err := srv.Client().Do(head)
	if err != nil {
		t.Fatal(err)
	}
	if hresp.StatusCode != 200 || hresp.ContentLength != int64(len(content)) {
		t.Fatalf("HEAD: %d len %d", hresp.StatusCode, hresp.ContentLength)
	}

	got, _ := http.Get(srv.URL + "/v1/blobs/" + sha)
	body := mustBody(t, got)
	if body != string(content) {
		t.Fatalf("GET roundtrip mismatch: %q", body)
	}
}

func TestShaMismatchRejectedAndPartRemoved(t *testing.T) {
	srv := testServer(t)
	sha := digest([]byte("expected"))
	resp := putBlob(srv, sha, []byte("different bytes entirely"), 0)
	if resp.StatusCode != 200 {
		t.Fatalf("PUT stage: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/blobs/"+sha+"/finalize", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("finalize mismatch: %d: %s", resp.StatusCode, mustBody(t, resp))
	}
	if _, err := os.Stat(filepath.Join(blobRoot, "blobs", sha)); !os.IsNotExist(err) {
		t.Fatalf("bad blob kept under a name it does not earn")
	}
	if _, err := os.Stat(filepath.Join(blobRoot, "blobs", sha+".part")); !os.IsNotExist(err) {
		t.Fatalf("mismatched part not cleaned up")
	}
}

func TestOffsetResumeAndList(t *testing.T) {
	srv := testServer(t)
	content := []byte(strings.Repeat("x", 4096))
	sha := digest(content)
	if resp := putBlob(srv, sha, content[:1024], 0); resp.StatusCode != 200 {
		t.Fatalf("part 1: %d", resp.StatusCode)
	}
	if resp := putBlob(srv, sha, content[1024:], 1024); resp.StatusCode != 200 {
		t.Fatalf("part 2: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/blobs/"+sha+"/finalize", nil)
	resp, _ := srv.Client().Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("finalize after resume: %d: %s", resp.StatusCode, mustBody(t, resp))
	}

	got, _ := http.Get(srv.URL + "/v1/blobs/" + sha + "?offset=1024")
	body, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if !bytes.Equal(body, content[1024:]) {
		t.Fatalf("offset GET mismatch")
	}

	list, _ := http.Get(srv.URL + "/v1/blobs")
	var listing struct {
		Blobs []map[string]any `json:"blobs"`
	}
	json.NewDecoder(list.Body).Decode(&listing)
	list.Body.Close()
	if len(listing.Blobs) != 1 || listing.Blobs[0]["size"].(float64) != float64(len(content)) {
		t.Fatalf("list: %+v", listing.Blobs)
	}
}

func TestDoubleFinalizeIsConflict(t *testing.T) {
	srv := testServer(t)
	content := []byte("once")
	sha := digest(content)
	putBlob(srv, sha, content, 0)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/blobs/"+sha+"/finalize", nil)
	first, _ := srv.Client().Do(req)
	second, _ := srv.Client().Do(req)
	if first.StatusCode != 200 || second.StatusCode != 409 {
		t.Fatalf("finalize statuses: first %d, second %d", first.StatusCode, second.StatusCode)
	}
}

func TestOllamaManifestPassthrough(t *testing.T) {
	srv := testServer(t)
	manifest := `{"schemaVersion":2,"config":{"digest":"sha256:abc"},"layers":[]}`
	rel := "registry.ollama.ai/library/gemma3/4b"
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/"+manifestPrefix+rel, strings.NewReader(manifest))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("PUT manifest: %d: %s", resp.StatusCode, mustBody(t, resp))
	}
	stored := filepath.Join(blobRoot, "ollama", "manifests", rel)
	if b, err := os.ReadFile(stored); err != nil || string(b) != manifest {
		t.Fatalf("manifest passthrough mismatch: %v %q", err, b)
	}
	got, _ := http.Get(srv.URL + "/" + manifestPrefix + rel)
	if mustBody(t, got) != manifest {
		t.Fatalf("GET manifest mismatch")
	}
}

// URL layers normalize ".." before the mux sees it, so traversal is asserted
// where it is actually enforced: the path validator itself.
func TestSafeRelRejectsTraversal(t *testing.T) {
	for _, evil := range []string{"../../etc/passwd", "/abs/name", "..", ".", "a/../../b", "null\x00byte"} {
		if _, err := safeRel(evil); err == nil {
			t.Fatalf("safeRel accepted %q", evil)
		}
	}
	for _, good := range []string{"registry.ollama.ai/library/gemma3/4b", "models--filipstrand--Z-Image-Turbo-mflux-4bit/index.json", "sub/dir/file.bin"} {
		if _, err := safeRel(good); err != nil {
			t.Fatalf("safeRel rejected %q: %v", good, err)
		}
	}
}

func TestHfBundleIndexAndFiles(t *testing.T) {
	srv := testServer(t)
	weights := []byte("fake-safetensors")
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/hf/models--filipstrand--Z-Image-Turbo-mflux-4bit/files/model.safetensors", bytes.NewReader(weights))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("PUT hf file: %d: %s", resp.StatusCode, mustBody(t, resp))
	}
	index := fmt.Sprintf(`{"files":[{"path":"model.safetensors","sha256":%q,"size":%d}]}`, digest(weights), len(weights))
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/v1/hf/models--filipstrand--Z-Image-Turbo-mflux-4bit/index.json", strings.NewReader(index))
	resp, _ = srv.Client().Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("PUT index: %d: %s", resp.StatusCode, mustBody(t, resp))
	}

	got, _ := http.Get(srv.URL + "/v1/hf/models--filipstrand--Z-Image-Turbo-mflux-4bit/files/model.safetensors")
	if mustBody(t, got) != string(weights) {
		t.Fatalf("hf file roundtrip mismatch")
	}
	got, _ = http.Get(srv.URL + "/v1/hf/models--filipstrand--Z-Image-Turbo-mflux-4bit/index.json")
	if mustBody(t, got) != index {
		t.Fatalf("index roundtrip mismatch")
	}
}

func TestTelemetryWrittenOnExitPaths(t *testing.T) {
	srv := testServer(t)
	// A failure (missing blob HEAD) and a success (health) both land one line each.
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/v1/blobs/"+strings.Repeat("0", 64), nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mustBody(t, resp)
	http.Get(srv.URL + "/health")

	b, err := os.ReadFile(telemetryPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected telemetry on both exit paths, got %d lines", len(lines))
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("telemetry not JSONL: %v", err)
	}
	if row["plane"] != "modelrepo" || row["request_id"] == "" || row["error"] == "" {
		t.Fatalf("failure row missing keys: %v", row)
	}
}
