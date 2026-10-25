package api

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/fleet"
	"github.com/awlx/vllm-dashboard/internal/store"
)

func TestGzipJSON(t *testing.T) {
	handler := GzipJSON(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") != "" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		WriteJSON(w, map[string]string{"status": "ok"})
	})
	request := func(method, target, encoding string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		if encoding != "" {
			r.Header.Set("Accept-Encoding", encoding)
		}
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}

	response := request(http.MethodGet, "/", "br, gzip")
	if response.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("response is not compressed")
	}
	if response.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatal("missing Vary header")
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if err := json.NewDecoder(reader).Decode(&body); err != nil || body["status"] != "ok" {
		t.Fatalf("decoded %v, %v", body, err)
	}

	for _, test := range []struct{ name, method, target, encoding string }{
		{"no header", http.MethodGet, "/", ""},
		{"gzip refused", http.MethodGet, "/", "gzip;q=0"},
		{"error status", http.MethodGet, "/?fail=1", "gzip"},
		{"head", http.MethodHead, "/", "gzip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := request(test.method, test.target, test.encoding)
			if encoding := response.Header().Get("Content-Encoding"); encoding != "" {
				t.Fatalf("Content-Encoding = %q, want none", encoding)
			}
		})
	}
}

func TestSettingsRoute(t *testing.T) {
	cfg := config.Default()
	cfg.PollInterval = "1500ms"
	mux := http.NewServeMux()
	Register(mux, fleet.New(cfg, store.New(cfg), process.NewCache("", "")))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/settings", nil))
	var settings Settings
	if err := json.Unmarshal(recorder.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if settings != (Settings{PollIntervalMs: 1500, HistoryWindowSeconds: 300, LongIntervalSeconds: 60}) {
		t.Fatalf("settings %+v", settings)
	}
	if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content type %q", recorder.Header().Get("Content-Type"))
	}
}
