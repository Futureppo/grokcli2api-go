package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/config"
)

// TestLiveGenerationLoad is opt-in because it sends real generation requests.
// It reports only aggregate timings, statuses, usage, and process resource data.
func TestLiveGenerationLoad(t *testing.T) {
	if os.Getenv("GROK_LIVE_LOAD") != "1" {
		t.Skip("set GROK_LIVE_LOAD=1 to run real generation load")
	}
	model := strings.TrimSpace(os.Getenv("GROK_LOAD_MODEL"))
	if model == "" {
		t.Fatal("GROK_LOAD_MODEL is required")
	}
	concurrency := positiveEnvInt(t, "GROK_LOAD_CONCURRENCY", 4)
	total := positiveEnvInt(t, "GROK_LOAD_REQUESTS", concurrency*4)
	stream := os.Getenv("GROK_LOAD_STREAM") == "1"
	timeout := durationEnv(t, "GROK_LOAD_TIMEOUT", 90*time.Second)
	authsDir := os.Getenv("GROK_AUTHS_DIR")
	if authsDir == "" {
		authsDir = filepath.Join("..", "..", "auths")
	}
	cfg := config.Config{
		ChatProxyBaseURL: "https://cli-chat-proxy.grok.com", ChatProxyVersion: "v1",
		ProxyURL: os.Getenv("GROK_PROXY_URL"), AuthsDir: authsDir,
		AuthsReloadInterval: 30 * time.Second, AuthRefreshConcurrency: 4,
		AccountMaxInflight: positiveEnvInt(t, "GROK_ACCOUNT_MAX_INFLIGHT", 16), ModelsRefreshInterval: 6 * time.Hour,
		RetryMaxAttempts: 3, RetryBaseDelay: 200 * time.Millisecond,
		RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 100000,
		ClientName: "grok-shell", ClientVersion: "0.2.93", ClientSurface: "tui",
		ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	transport := &http.Transport{
		MaxIdleConns: concurrency * 2, MaxIdleConnsPerHost: concurrency * 2,
		IdleConnTimeout: 30 * time.Second,
	}
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()

	payload, err := json.Marshal(map[string]any{
		"model": model, "input": "Reply with exactly OK.", "max_output_tokens": 8,
		"stream": stream, "store": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs := make(chan int)
	results := make(chan loadResult, total)
	var wg sync.WaitGroup
	before := resourceSnapshot()
	monitor := startResourceMonitor(before.heapAlloc)
	started := time.Now()
	workers := concurrency
	if workers > total {
		workers = total
	}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				results <- runLoadRequest(client, httpServer.URL, payload, index, stream, timeout)
			}
		}()
	}
	for index := 0; index < total; index++ {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	close(results)
	elapsed := time.Since(started)
	peaks := monitor.stop()
	after := resourceSnapshot()
	report := summarizeLoad(results, total, concurrency, stream, elapsed, before, after, peaks)
	t.Log(report)
}

type loadResult struct {
	status       int
	success      bool
	latency      time.Duration
	ttfb         time.Duration
	ttft         time.Duration
	inputTokens  int64
	outputTokens int64
	failure      string
}

func runLoadRequest(client *http.Client, baseURL string, payload []byte, index int, stream bool, timeout time.Duration) loadResult {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/responses", bytes.NewReader(payload))
	if err != nil {
		return loadResult{latency: time.Since(started), failure: "request_build"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Grok-Session-ID", fmt.Sprintf("load-%d", index))
	resp, err := client.Do(req)
	if err != nil {
		failure := "network"
		if ctx.Err() != nil {
			failure = "timeout"
		}
		return loadResult{latency: time.Since(started), failure: failure}
	}
	defer resp.Body.Close()
	result := loadResult{status: resp.StatusCode}
	if !stream || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		result.latency = time.Since(started)
		if readErr != nil {
			result.failure = "read"
			return result
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			result.success = true
			result.inputTokens, result.outputTokens = responseUsage(body)
		} else {
			result.failure = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return result
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 32*1024), 4<<20)
	streamFailed := false
	for scanner.Scan() {
		line := scanner.Text()
		if result.ttfb == 0 && strings.TrimSpace(line) != "" {
			result.ttfb = time.Since(started)
		}
		if strings.HasPrefix(line, "event:") && strings.TrimSpace(strings.TrimPrefix(line, "event:")) == "error" {
			streamFailed = true
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		typeName, _ := event["type"].(string)
		if result.ttft == 0 && strings.Contains(typeName, "output_text.delta") {
			result.ttft = time.Since(started)
		}
		if typeName == "error" {
			streamFailed = true
		}
		in, out := usageFromMap(event)
		result.inputTokens = max(result.inputTokens, in)
		result.outputTokens = max(result.outputTokens, out)
		if response, ok := event["response"].(map[string]any); ok {
			in, out = usageFromMap(response)
			result.inputTokens = max(result.inputTokens, in)
			result.outputTokens = max(result.outputTokens, out)
		}
	}
	result.latency = time.Since(started)
	if err := scanner.Err(); err != nil {
		result.failure = "stream_read"
		return result
	}
	if streamFailed {
		result.failure = "stream_error"
		return result
	}
	result.success = true
	return result
}

func responseUsage(body []byte) (int64, int64) {
	var response map[string]any
	if json.Unmarshal(body, &response) != nil {
		return 0, 0
	}
	return usageFromMap(response)
}

func usageFromMap(value map[string]any) (int64, int64) {
	usage, _ := value["usage"].(map[string]any)
	return jsonInt64(usage["input_tokens"]), jsonInt64(usage["output_tokens"])
}

func jsonInt64(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case json.Number:
		parsed, _ := number.Int64()
		return parsed
	default:
		return 0
	}
}

type processResources struct {
	cpuSeconds float64
	heapAlloc  uint64
	totalAlloc uint64
	numGC      uint32
}

type resourcePeaks struct {
	heapAlloc  uint64
	goroutines int
}

type resourceMonitor struct {
	mu    sync.Mutex
	peaks resourcePeaks
	done  chan struct{}
	wg    sync.WaitGroup
}

func startResourceMonitor(initialHeap uint64) *resourceMonitor {
	monitor := &resourceMonitor{
		peaks: resourcePeaks{heapAlloc: initialHeap, goroutines: runtime.NumGoroutine()},
		done:  make(chan struct{}),
	}
	monitor.wg.Add(1)
	go func() {
		defer monitor.wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				var memory runtime.MemStats
				runtime.ReadMemStats(&memory)
				goroutines := runtime.NumGoroutine()
				monitor.mu.Lock()
				if memory.HeapAlloc > monitor.peaks.heapAlloc {
					monitor.peaks.heapAlloc = memory.HeapAlloc
				}
				if goroutines > monitor.peaks.goroutines {
					monitor.peaks.goroutines = goroutines
				}
				monitor.mu.Unlock()
			case <-monitor.done:
				return
			}
		}
	}()
	return monitor
}

func (m *resourceMonitor) stop() resourcePeaks {
	close(m.done)
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peaks
}

func resourceSnapshot() processResources {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	sample := []metrics.Sample{
		{Name: "/cpu/classes/user:cpu-seconds"},
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
		{Name: "/cpu/classes/scavenge/total:cpu-seconds"},
	}
	metrics.Read(sample)
	cpu := 0.0
	for _, item := range sample {
		if item.Value.Kind() == metrics.KindFloat64 {
			cpu += item.Value.Float64()
		}
	}
	return processResources{cpuSeconds: cpu, heapAlloc: memory.HeapAlloc, totalAlloc: memory.TotalAlloc, numGC: memory.NumGC}
}

func summarizeLoad(results <-chan loadResult, total, concurrency int, stream bool, elapsed time.Duration, before, after processResources, peaks resourcePeaks) string {
	latencies := make([]time.Duration, 0, total)
	ttfb := make([]time.Duration, 0, total)
	ttft := make([]time.Duration, 0, total)
	statuses := map[int]int{}
	failures := map[string]int{}
	successes := 0
	var inputTokens, outputTokens int64
	for result := range results {
		latencies = append(latencies, result.latency)
		if result.ttfb > 0 {
			ttfb = append(ttfb, result.ttfb)
		}
		if result.ttft > 0 {
			ttft = append(ttft, result.ttft)
		}
		statuses[result.status]++
		if result.success {
			successes++
		} else {
			failures[result.failure]++
		}
		inputTokens += result.inputTokens
		outputTokens += result.outputTokens
	}
	mode := "non_stream"
	if stream {
		mode = "stream"
	}
	return fmt.Sprintf(
		"load_summary mode=%s concurrency=%d requests=%d elapsed=%s throughput=%.2f_req_s success=%d success_rate=%.2f%% statuses=%s failures=%s latency[p50=%s p95=%s p99=%s max=%s] ttfb[p50=%s p95=%s p99=%s] ttft[p50=%s p95=%s p99=%s] usage[input=%d output=%d] resources[cpu=%.3fs heap_delta=%dKB heap_peak_delta=%dKB alloc=%dKB gc=%d goroutines_peak=%d]",
		mode, concurrency, total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), successes, float64(successes)*100/float64(total),
		formatIntCounts(statuses), formatStringCounts(failures), percentile(latencies, 0.50), percentile(latencies, 0.95), percentile(latencies, 0.99), percentile(latencies, 1),
		percentile(ttfb, 0.50), percentile(ttfb, 0.95), percentile(ttfb, 0.99), percentile(ttft, 0.50), percentile(ttft, 0.95), percentile(ttft, 0.99),
		inputTokens, outputTokens, after.cpuSeconds-before.cpuSeconds, (int64(after.heapAlloc)-int64(before.heapAlloc))/1024,
		(int64(peaks.heapAlloc)-int64(before.heapAlloc))/1024, (after.totalAlloc-before.totalAlloc)/1024, after.numGC-before.numGC, peaks.goroutines,
	)
}

func percentile(values []time.Duration, quantile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := int(float64(len(values)-1)*quantile + 0.5)
	return values[index].Round(time.Millisecond)
}

func formatIntCounts(counts map[int]int) string {
	keys := make([]int, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%d:%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}

func formatStringCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}

func positiveEnvInt(t *testing.T, key string, fallback int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		t.Fatalf("%s must be a positive integer", key)
	}
	return value
}

func durationEnv(t *testing.T, key string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		t.Fatalf("%s must be a positive duration", key)
	}
	return value
}
