package vllm

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func normalizeTensorFoldMetrics(raw map[string]float64) (map[string]float64, error) {
	for _, name := range []string{"prompt_tokens_total", "generation_tokens_total", "generation_tokens_running"} {
		value, ok := raw["tensorfold:"+name]
		if !ok || value < 0 {
			return nil, fmt.Errorf("missing or invalid native TensorFold metric %s", name)
		}
	}
	metrics := map[string]float64{}
	for target, source := range map[string]string{
		"prompt_tokens_total":                   "prompt_tokens_total",
		"num_requests_running":                  "num_requests_running",
		"num_requests_waiting":                  "num_requests_waiting",
		"kv_cache_usage_perc":                   "kv_cache_usage_perc",
		"spec_decode_num_draft_tokens_total":    "spec_decode_num_draft_tokens_total",
		"spec_decode_num_accepted_tokens_total": "spec_decode_num_accepted_tokens_total",
		"e2e_request_latency_seconds_sum":       "e2e_request_latency_seconds_sum",
		"e2e_request_latency_seconds_count":     "e2e_request_latency_seconds_count",
		"request_success_total":                 "e2e_request_latency_seconds_count",
		"time_to_first_token_seconds_sum":       "time_to_first_token_seconds_sum",
		"time_to_first_token_seconds_count":     "time_to_first_token_seconds_count",
		"inter_token_latency_seconds_sum":       "request_time_per_output_token_seconds_sum",
		"inter_token_latency_seconds_count":     "request_time_per_output_token_seconds_count",
		"num_preemptions_total":                 "preemptions_total",
	} {
		if value, ok := raw["tensorfold:"+source]; ok {
			metrics["vllm:"+target] = value
		}
	}
	total := raw["tensorfold:generation_tokens_total"] + raw["tensorfold:generation_tokens_running"]
	if math.IsInf(total, 0) {
		return nil, fmt.Errorf("native TensorFold generation counter overflow")
	}
	metrics["vllm:generation_tokens_total"] = total
	return metrics, nil
}

type tensorfoldHealth struct {
	completion, prompt, cached float64
}

func tensorfoldHealthCounters(metricsURL string) (tensorfoldHealth, error) {
	var result tensorfoldHealth
	endpoint, err := url.Parse(metricsURL)
	if err != nil || !strings.HasSuffix(endpoint.Path, "/metrics") {
		return result, fmt.Errorf("metrics URL has no /metrics path")
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/metrics") + "/health"
	endpoint.RawPath = ""
	client := http.Client{Timeout: 1500 * time.Millisecond, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(endpoint.String())
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("health endpoint returned %s", response.Status)
	}
	var payload struct {
		CompletionTokensTotal *float64 `json:"completion_tokens_total"`
		PromptTokensTotal     *float64 `json:"prompt_tokens_total"`
		CachedTokensTotal     *float64 `json:"cached_tokens_total"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return result, err
	}
	if payload.CompletionTokensTotal == nil {
		return result, fmt.Errorf("health has no completion_tokens_total")
	}
	valid := func(v *float64) bool { return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0 }
	if !valid(payload.CompletionTokensTotal) {
		return result, fmt.Errorf("invalid completion_tokens_total")
	}
	result.completion = *payload.CompletionTokensTotal
	result.cached = -1
	if valid(payload.PromptTokensTotal) && valid(payload.CachedTokensTotal) {
		result.prompt = *payload.PromptTokensTotal
		result.cached = *payload.CachedTokensTotal
	}
	return result, nil
}
