// Package vllm scrapes the Prometheus /metrics endpoint of a vLLM or TensorFold server.
package vllm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var metricLineRe = regexp.MustCompile(`^([a-zA-Z0-9_:]+)(\{[^}]*\})?\s+([0-9eE\.\+\-]+)\s*$`)

// Scrape reads a /metrics endpoint and returns the sum of each vllm: series.
func Scrape(metricsURL string) (map[string]float64, error) {
	client := http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(metricsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics endpoint returned %s", resp.Status)
	}

	metrics := map[string]float64{}
	tensorfoldMetrics := map[string]float64{}
	tensorfold := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Only vllm: series are used, and never histogram buckets. Most
		// lines of a real /metrics body are buckets, so skip them before
		// the regular expression.
		isTensorfold := strings.HasPrefix(line, "tensorfold:")
		if !isTensorfold && !strings.HasPrefix(line, "vllm:") {
			continue
		}
		if end := strings.IndexAny(line, "{ "); end > 0 && strings.HasSuffix(line[:end], "_bucket") {
			continue
		}
		m := metricLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		target := metrics
		if strings.HasPrefix(name, "tensorfold:") {
			tensorfold = true
			target = tensorfoldMetrics
		} else if !strings.HasPrefix(name, "vllm:") {
			continue
		}
		val, err := strconv.ParseFloat(m[3], 64)
		if err != nil || math.IsNaN(val) || math.IsInf(val, 0) {
			continue
		}
		// sum across label variants (e.g. request_success_total has one line per finished_reason)
		total := target[name] + val
		if math.IsInf(total, 0) {
			return nil, fmt.Errorf("metrics value overflow")
		}
		target[name] = total
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(metrics) == 0 {
		if tensorfold {
			return normalizeTensorFoldMetrics(tensorfoldMetrics)
		}
		return nil, fmt.Errorf("no finite vllm metrics found")
	}
	if tensorfold {
		// TensorFold /metrics counts only finished requests; /health adds running replies.
		health, err := tensorfoldHealthCounters(metricsURL)
		if err != nil {
			return nil, fmt.Errorf("tensorfold health: %w", err)
		}
		metrics["vllm:generation_tokens_total"] = math.Max(health.completion, metrics["vllm:generation_tokens_total"])
		// TensorFold exports no vllm:prefix_cache_* counters; same token-based semantics via /health.
		if _, ok := metrics["vllm:prefix_cache_queries_total"]; !ok && health.prompt > 0 && health.cached >= 0 {
			metrics["vllm:prefix_cache_queries_total"] = health.prompt
			metrics["vllm:prefix_cache_hits_total"] = math.Min(health.cached, health.prompt)
		}
	}
	return metrics, nil
}

// ServedModelName returns the only model ID from /v1/models, or "".
func ServedModelName(metricsURL string) string {
	endpoint, err := url.Parse(metricsURL)
	if err != nil || !strings.HasSuffix(endpoint.Path, "/metrics") {
		return ""
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/metrics") + "/v1/models"
	endpoint.RawPath = ""
	client := http.Client{Timeout: 1500 * time.Millisecond, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(endpoint.String())
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ""
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil || len(payload.Data) != 1 {
		return ""
	}
	return payload.Data[0].ID
}
