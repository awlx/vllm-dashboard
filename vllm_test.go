package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverServedModelName(t *testing.T) {
	for _, testcase := range []struct{ body, want string }{
		{`{"data":[{"id":"actual-served-model"}]}`, "actual-served-model"},
		{`{"data":[{"id":"first"},{"id":"second"}]}`, ""},
		{`<html>not an API</html>`, ""},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/prefix/v1/models" {
				t.Errorf("wrong discovery path: %s", r.URL.Path)
			}
			_, _ = w.Write([]byte(testcase.body))
		}))
		got := discoverServedModelName(server.URL + "/prefix/metrics")
		server.Close()
		if got != testcase.want {
			t.Fatalf("discovered %q, want %q", got, testcase.want)
		}
	}
}

func TestScrapeVLLMUsesTensorFoldLiveGenerationTokens(t *testing.T) {
	healthBody := `{"ok": true, "backend": "tensorfold", "completion_tokens_total": 1250}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			_, _ = w.Write([]byte("tensorfold:generation_tokens_total 1000\nvllm:generation_tokens_total{model_name=\"m\"} 1000\n"))
		case "/health":
			_, _ = w.Write([]byte(healthBody))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	metrics, err := scrapeVLLM(server.URL + "/metrics")
	if err != nil || metrics["vllm:generation_tokens_total"] != 1250 {
		t.Fatalf("live tokens not used: %v %v", metrics, err)
	}
	if _, ok := metrics["vllm:prefix_cache_queries_total"]; ok {
		t.Fatalf("prefix cache derived without health counters: %v", metrics)
	}
	healthBody = `{"ok": true, "completion_tokens_total": 1250, "prompt_tokens_total": 4000, "cached_tokens_total": 3000}`
	metrics, err = scrapeVLLM(server.URL + "/metrics")
	if err != nil || metrics["vllm:prefix_cache_queries_total"] != 4000 || metrics["vllm:prefix_cache_hits_total"] != 3000 {
		t.Fatalf("prefix cache not derived from health: %v %v", metrics, err)
	}
	healthBody = `{"ok": true}`
	if _, err := scrapeVLLM(server.URL + "/metrics"); err == nil {
		t.Fatal("accepted TensorFold health without live counter")
	}
}

func TestScrapeVLLMUsesNativeTensorFoldMetrics(t *testing.T) {
	body := `tensorfold:prompt_tokens_total 4000
tensorfold:generation_tokens_total 1000
tensorfold:generation_tokens_running{slot="0"} 25
tensorfold:generation_tokens_running{slot="1"} 5
tensorfold:requests_running 2
tensorfold:num_requests_running 2
tensorfold:num_requests_waiting 1
tensorfold:kv_cache_usage_perc{stream="0"} 0.25
tensorfold:spec_decode_num_draft_tokens_total 60
tensorfold:spec_decode_num_accepted_tokens_total 42
tensorfold:e2e_request_latency_seconds_sum 6
tensorfold:e2e_request_latency_seconds_count 3
tensorfold:time_to_first_token_seconds_sum 0.6
tensorfold:time_to_first_token_seconds_count 3
tensorfold:request_time_per_output_token_seconds_sum 0.12
tensorfold:request_time_per_output_token_seconds_count 3
tensorfold:preemptions_total 1
`
	healthCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			healthCalls++
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	want := map[string]float64{
		"vllm:prompt_tokens_total":                   4000,
		"vllm:generation_tokens_total":               1030,
		"vllm:num_requests_running":                  2,
		"vllm:num_requests_waiting":                  1,
		"vllm:kv_cache_usage_perc":                   0.25,
		"vllm:spec_decode_num_draft_tokens_total":    60,
		"vllm:spec_decode_num_accepted_tokens_total": 42,
		"vllm:e2e_request_latency_seconds_sum":       6,
		"vllm:e2e_request_latency_seconds_count":     3,
		"vllm:request_success_total":                 3,
		"vllm:time_to_first_token_seconds_sum":       0.6,
		"vllm:time_to_first_token_seconds_count":     3,
		"vllm:inter_token_latency_seconds_sum":       0.12,
		"vllm:inter_token_latency_seconds_count":     3,
		"vllm:num_preemptions_total":                 1,
	}
	metrics, err := scrapeVLLM(server.URL + "/metrics")
	if err != nil || !reflect.DeepEqual(metrics, want) || healthCalls != 0 {
		t.Fatalf("native TensorFold metrics not normalized: %v %v health=%d", metrics, err, healthCalls)
	}
	body = strings.Replace(body, "generation_tokens_total 1000", "generation_tokens_total 1030", 1)
	body = strings.Replace(body, `generation_tokens_running{slot="0"} 25`, `generation_tokens_running{slot="0"} 0`, 1)
	body = strings.Replace(body, `generation_tokens_running{slot="1"} 5`, `generation_tokens_running{slot="1"} 0`, 1)
	metrics, err = scrapeVLLM(server.URL + "/metrics")
	if err != nil || metrics["vllm:generation_tokens_total"] != 1030 {
		t.Fatalf("finished stream double-counted: %v %v", metrics, err)
	}
}

func TestScrapeVLLMRejectsInvalidNativeTensorFoldMetrics(t *testing.T) {
	for _, body := range []string{
		"tensorfold:unknown 1\n",
		"tensorfold:generation_tokens_total 1\n",
		"tensorfold:prompt_tokens_total 10\ntensorfold:generation_tokens_total NaN\ntensorfold:generation_tokens_running 0\n",
		"tensorfold:prompt_tokens_total 10\ntensorfold:generation_tokens_total -1\ntensorfold:generation_tokens_running 0\n",
		"tensorfold:prompt_tokens_total 10\ntensorfold:generation_tokens_total 1e308\ntensorfold:generation_tokens_running 1e308\n",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		_, err := scrapeVLLM(server.URL + "/metrics")
		server.Close()
		if err == nil {
			t.Fatalf("accepted invalid native TensorFold metrics %q", body)
		}
	}
}

func TestScrapeVLLMIgnoresHealthForVLLM(t *testing.T) {
	healthCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			healthCalls++
			return
		}
		_, _ = w.Write([]byte("vllm:generation_tokens_total{model_name=\"m\"} 42\n"))
	}))
	defer server.Close()

	metrics, err := scrapeVLLM(server.URL + "/metrics")
	if err != nil || metrics["vllm:generation_tokens_total"] != 42 || healthCalls != 0 {
		t.Fatalf("vLLM scrape changed: %v %v health=%d", metrics, err, healthCalls)
	}
}

func TestScrapeVLLMRejectsErrorAndNonMetrics(t *testing.T) {
	for _, body := range []string{"server error", "<html>not metrics</html>", "other_metric 1\n"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if body == "server error" {
				w.WriteHeader(503)
			}
			_, _ = w.Write([]byte(body))
		}))
		_, err := scrapeVLLM(server.URL)
		server.Close()
		if err == nil {
			t.Fatalf("accepted invalid engine response %q", body)
		}
	}
}
