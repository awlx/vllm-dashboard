package main

type Sample struct {
	Time              int64   `json:"t"`
	Running           float64 `json:"running"`
	Waiting           float64 `json:"waiting"`
	KVCachePct        float64 `json:"kv_cache_pct"`
	PromptTokPerSec   float64 `json:"prompt_tps"`
	GenTokPerSec      float64 `json:"gen_tps"`
	GPUUtilPct        float64 `json:"gpu_util_pct"`
	GPUTempC          float64 `json:"gpu_temp_c"`
	GPUPowerW         float64 `json:"gpu_power_w"`
	GPUClockMHz       float64 `json:"gpu_clock_mhz"`
	TTFTMs            float64 `json:"ttft_ms"`
	InterTokenMs      float64 `json:"inter_token_ms"`
	E2ELatencyMs      float64 `json:"e2e_latency_ms"`
	PrefixCacheHitPct float64 `json:"prefix_cache_hit_pct"`
	PreemptionsTotal  float64 `json:"preemptions_total"`
	CompletedRequests float64 `json:"completed_requests"`
	ReqPerSec         float64 `json:"req_per_sec"`
	SpecAcceptPct     float64 `json:"spec_accept_pct"`
	SpecMeanAccepted  float64 `json:"spec_mean_accepted"`
	CPUUtilPct        float64 `json:"cpu_util_pct"`
	CPUClockMHz       float64 `json:"cpu_clock_mhz"`
	MemUsedPct        float64 `json:"mem_used_pct"`
	MemUsedGB         float64 `json:"mem_used_gb"`
	MemTotalGB        float64 `json:"mem_total_gb"`
	NetRxMbps         float64 `json:"net_rx_mbps"`
	NetTxMbps         float64 `json:"net_tx_mbps"`
	PromptTokensCum   float64 `json:"-"`
	GenTokensCum      float64 `json:"-"`
}

type TokenWindow struct {
	PromptTokens float64 `json:"prompt_tokens"`
	GenTokens    float64 `json:"gen_tokens"`
	TotalTokens  float64 `json:"total_tokens"`
}

type TokenUsage struct {
	LastHour TokenWindow `json:"last_hour"`
	Last24h  TokenWindow `json:"last_24h"`
}

type ModelOverview struct {
	Key      string     `json:"key"`
	Name     string     `json:"name"`
	NodeKey  string     `json:"node_key"`
	NodeName string     `json:"node_name"`
	Online   bool       `json:"online"`
	Latest   Sample     `json:"latest"`
	Info     ServerInfo `json:"info"`
	Tokens   TokenUsage `json:"tokens"`
}

// ServerInfo is static-ish serving config, polled at a lower rate than metrics.
type ServerInfo struct {
	Model                string `json:"model"`
	ServedModelName      string `json:"served_model_name"`
	MaxModelLen          string `json:"max_model_len"`
	GPUMemUtilization    string `json:"gpu_memory_utilization"`
	MaxNumBatchedTokens  string `json:"max_num_batched_tokens"`
	SpeculativeConfig    string `json:"speculative_config"`
	VLLMVersion          string `json:"vllm_version"`
	KernelVersion        string `json:"kernel_version"`
	ServiceActiveSince   string `json:"service_active_since"`
	ServiceUptimeSeconds int64  `json:"service_uptime_seconds"`
	ServiceRestarts      string `json:"service_restarts"`
}
