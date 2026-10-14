// lattice-modelrepo stores model blobs on the Pi's large partition so the Mac
// gateways can free local disk and re-fetch when a model is needed again. It
// serves two ecosystem shapes under one content-verified store: Ollama
// (sha256 blobs + JSON manifests) and HF-hosted weights (bundled model
// directories with a per-file index). The service stores and serves bytes
// only — it never touches a Mac's Ollama or HF directories, and it has no
// DELETE: eviction is a human `rm` after a verified push.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pegamonstro/ai-tools-inference-lattice/pkg/latticeconfig"
)

// The store layout lives under this fixed repo name inside the configured
// volume, so the partition can host other data without the service owning it.
const repoSubpath = "inference-repo/blob-llm"

var (
	storeVolume   = latticeconfig.Env("LATTICE_MODELREPO_STORE", "/mnt/offsite")
	telemetryPath = latticeconfig.Env("LATTICE_MODELREPO_TELEMETRY", "/var/log/lattice/telemetry-modelrepo.jsonl")

	// blobRoot is a package var so tests can point it at a t.TempDir().
	blobRoot string
)

type telemetry struct {
	Plane     string `json:"plane"`
	RequestID string `json:"request_id"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	Bytes     int64  `json:"bytes"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Error     string `json:"error,omitempty"`
}

func logTelemetry(t telemetry) {
	f, err := os.OpenFile(telemetryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("telemetry error: %v\n", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(t)
	f.Write(append(b, '\n'))
}

// logged wraps a handler to emit one telemetry line on every exit path with
// the request id passed by the caller (or a generated fallback).
func logged(next func(http.ResponseWriter, *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = requestId()
		}
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		err := next(rec, r)
		if err != nil {
			rec.status = statusFor(err)
			if rec.written == 0 {
				http.Error(w, err.Error(), rec.status)
			} else {
				fmt.Printf("late error after %d bytes: %v\n", rec.written, err)
			}
		}
		entry := telemetry{
			Plane:     "modelrepo",
			RequestID: reqID,
			Method:    r.Method,
			Path:      r.URL.Path,
			Status:    rec.status,
			Bytes:     rec.written,
			ElapsedMS: time.Since(start).Milliseconds(),
			Error:     errorString(err),
		}
		logTelemetry(entry)
	})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

func statusFor(err error) int {
	if he, ok := err.(*httpError); ok {
		return he.status
	}
	return http.StatusInternalServerError
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(msg string) error { return &httpError{http.StatusBadRequest, msg} }
func notFound(msg string) error   { return &httpError{http.StatusNotFound, msg} }
func conflict(msg string) error   { return &httpError{http.StatusConflict, msg} }
func serverErr(msg string) error  { return &httpError{http.StatusInternalServerError, msg} }

// safeRel rejects absolute paths and traversal, then returns the path joined
// under root. Every client-supplied path component goes through this — the
// service is unauthenticated, so the path itself is the only authority check.
func safeRel(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") {
		return "", badRequest("absolute path not allowed")
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, 0) {
		return "", badRequest("path traversal not allowed")
	}
	return clean, nil
}

func shaRe(hexish string) bool {
	if len(hexish) != 64 {
		return false
	}
	_, err := hex.DecodeString(hexish)
	return err == nil
}

func handleHealth(w http.ResponseWriter, _ *http.Request) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(storeVolume, &st); err != nil {
		return serverErr(err.Error())
	}
	free := st.Bavail * uint64(st.Bsize)
	return json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "store": storeVolume, "repo": blobRoot,
		"free_bytes": free,
	})
}

// ollamaBlobPath maps a hex sha to its content-addressed file and its upload
// staging file. Staging name can never collide with a final blob.
func blobPath(sha string) string { return filepath.Join(blobRoot, "blobs", sha) }

func listBlobs(w http.ResponseWriter, _ *http.Request) error {
	entries, err := os.ReadDir(filepath.Join(blobRoot, "blobs"))
	if err != nil {
		return serverErr(err.Error())
	}
	blobs := []map[string]any{}
	for _, e := range entries {
		if e.IsDir() || !shaRe(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		blobs = append(blobs, map[string]any{"sha256": e.Name(), "size": info.Size()})
	}
	return json.NewEncoder(w).Encode(map[string]any{"blobs": blobs})
}

func oneBlob(w http.ResponseWriter, r *http.Request, sha string) error {
	if !shaRe(sha) {
		return badRequest("sha256 must be 64 hex chars")
	}
	final := blobPath(sha)
	st, statErr := os.Stat(final)
	switch r.Method {
	case http.MethodHead:
		if statErr != nil {
			return notFound("no such blob")
		}
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		w.WriteHeader(http.StatusOK)
		return nil
	case http.MethodGet:
		if statErr != nil {
			return notFound("no such blob")
		}
		f, err := os.Open(final)
		if err != nil {
			return serverErr(err.Error())
		}
		defer f.Close()
		if off := r.URL.Query().Get("offset"); off != "" {
			n, err := strconv.ParseInt(off, 10, 64)
			if err != nil || n < 0 || n > st.Size() {
				return badRequest("bad offset")
			}
			if _, err := f.Seek(n, io.SeekStart); err != nil {
				return serverErr(err.Error())
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, err = io.Copy(w, f)
		return err
	case http.MethodPut:
		return stageBlob(w, r, final)
	case http.MethodPost:
		return badRequest("POST is only for /finalize")
	default:
		return badRequest("method not allowed")
	}
}

func stageBlob(w http.ResponseWriter, r *http.Request, final string) error {
	part := final + ".part"
	flag := os.O_CREATE | os.O_WRONLY
	if r.URL.Query().Get("part") == "" {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flag, 0644)
	if err != nil {
		return serverErr(err.Error())
	}
	if off := r.URL.Query().Get("part"); off != "" {
		n, perr := strconv.ParseInt(off, 10, 64)
		if perr != nil || n < 0 {
			f.Close()
			return badRequest("bad part offset")
		}
		if _, err := f.Seek(n, io.SeekStart); err != nil {
			f.Close()
			return serverErr(err.Error())
		}
	}
	n, err := io.Copy(f, r.Body)
	cerr := f.Close()
	if err != nil {
		return serverErr(err.Error())
	}
	if cerr != nil {
		return serverErr(cerr.Error())
	}
	return json.NewEncoder(w).Encode(map[string]any{"stored": n})
}

// finalizeBlob hashes the staged part streaming and promotes it to its final
// content-addressed name only when the sha matches — a mismatching upload is
// removed, never kept under a name it does not earn.
func finalizeBlob(sha string) error {
	part := blobPath(sha) + ".part"
	final := blobPath(sha)
	if _, err := os.Stat(final); err == nil {
		return conflict("blob already finalized")
	}
	f, err := os.Open(part)
	if err != nil {
		if os.IsNotExist(err) {
			return notFound("no staged upload")
		}
		return serverErr(err.Error())
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return serverErr(err.Error())
	}
	if err := f.Close(); err != nil {
		return serverErr(err.Error())
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		os.Remove(part)
		return badRequest("sha256 mismatch")
	}
	return os.Rename(part, final)
}

// manifestPrefix is the fixed root for raw Ollama manifest passthrough.
const manifestPrefix = "/v1/ollama/manifests/"

func ollamaManifests(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		return badRequest("method not allowed")
	}
	rel, err := safeRel(strings.TrimPrefix(r.URL.Path, manifestPrefix))
	if err != nil {
		return err
	}
	path := filepath.Join(blobRoot, "ollama", "manifests", rel)
	switch r.Method {
	case http.MethodGet:
		http.ServeFile(w, r, path)
		return nil
	default: // PUT
		if mkdirErr := os.MkdirAll(filepath.Dir(path), 0755); mkdirErr != nil {
			return serverErr(mkdirErr.Error())
		}
		if err := writeFileAtomic(path, r.Body); err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(map[string]any{"stored": rel})
	}
}

// hfPrefix serves the bundle half of the store: an index.json of per-file
// shas and the files themselves, mirrored from the HF cache layout.
const hfPrefix = "/v1/hf/"

func hfBundle(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		return badRequest("method not allowed")
	}
	rel, err := safeRel(strings.TrimPrefix(r.URL.Path, hfPrefix))
	if err != nil {
		return err
	}
	path := filepath.Join(blobRoot, "hf", rel)
	if r.Method == http.MethodGet {
		http.ServeFile(w, r, path)
		return nil
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0755); mkdirErr != nil {
		return serverErr(mkdirErr.Error())
	}
	if err := writeFileAtomic(path, r.Body); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(map[string]any{"stored": rel})
}

func writeFileAtomic(path string, src io.Reader) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return serverErr(err.Error())
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		os.Remove(tmp)
		return serverErr(err.Error())
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return serverErr(err.Error())
	}
	return os.Rename(tmp, path)
}

func requestId() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "req-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return "req-" + hex.EncodeToString(b)
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/health", logged(handleHealth))
	mux.Handle("/v1/blobs", logged(listBlobs))
	mux.Handle("/v1/blobs/", http.StripPrefix("/v1/blobs/", logged(func(w http.ResponseWriter, r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/finalize") {
			sha := strings.TrimSuffix(r.URL.Path, "/finalize")
			if !shaRe(sha) {
				return badRequest("sha256 must be 64 hex chars")
			}
			return finalizeBlob(sha)
		}
		return oneBlob(w, r, r.URL.Path)
	})))
	mux.Handle(manifestPrefix, logged(ollamaManifests))
	mux.Handle(hfPrefix, logged(hfBundle))
	return mux
}

func init() {
	blobRoot = filepath.Join(storeVolume, repoSubpath)
}

// bindAddrs resolves the listeners to open. LATTICE_MODELREPO_BIND lists
// explicit "ip:port" pairs — the store serves 127.0.0.1 for this host's own
// workloads plus the tailnet address for the gateways, comma-separated. The
// service is unauthenticated; who can reach the port is a bind decision, not
// application logic, and a wildcard bind is never intended for it.
func bindAddrs() []string {
	if b := latticeconfig.Env("LATTICE_MODELREPO_BIND", ""); b != "" {
		var out []string
		for _, e := range strings.Split(b, ",") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, e)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"127.0.0.1:8090"}
}

// serveOn opens one listener and serves it for the process's lifetime. A
// machine booting can reach here before Tailscale has raised the tailnet
// interface, so an address that is not up yet is retried — the same race the
// frontend's serveOn handles.
func serveOn(addr string) {
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			fmt.Printf("lattice-modelrepo listening on %s\n", addr)
			log.Fatal(http.Serve(ln, newMux()))
		}
		fmt.Printf("lattice-modelrepo waiting for %s: %v (retry in 5s)\n", addr, err)
		time.Sleep(5 * time.Second)
	}
}

func main() {
	for _, sub := range []string{"blobs", filepath.Join("ollama", "manifests")} {
		if err := os.MkdirAll(filepath.Join(blobRoot, sub), 0755); err != nil {
			log.Fatalf("store mkdir: %v", err)
		}
	}

	fmt.Printf("lattice-modelrepo: store %s\n", blobRoot)
	for _, addr := range bindAddrs() {
		go serveOn(addr)
	}
	select {}
}
