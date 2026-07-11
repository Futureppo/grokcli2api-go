package grok

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
)

// TestLiveCredentialPool is opt-in because it refreshes the real OAuth files
// and contacts non-generative upstream endpoints for every account. It never
// logs account identifiers, token values, subjects, emails, or response bodies.
func TestLiveCredentialPool(t *testing.T) {
	if os.Getenv("GROK_LIVE_AUTH_PROBE") != "1" {
		t.Skip("set GROK_LIVE_AUTH_PROBE=1 to probe the local credential pool")
	}
	authsDir := os.Getenv("GROK_AUTHS_DIR")
	if authsDir == "" {
		authsDir = filepath.Join("..", "..", "auths")
	}
	cfg := config.Config{
		ChatProxyBaseURL: "https://cli-chat-proxy.grok.com", ChatProxyVersion: "v1",
		ProxyURL: os.Getenv("GROK_PROXY_URL"), AuthRefreshConcurrency: 2, ModelsRefreshInterval: 6 * time.Hour,
		ClientName: "grok-shell", ClientVersion: "0.2.93", ClientSurface: "tui", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
		RetryMaxAttempts: 1, RetryBaseDelay: 200 * time.Millisecond, RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour,
	}
	httpClient, err := NewHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: authsDir, Surface: "tui", ReloadInterval: time.Hour,
		RefreshConcurrency: 2, AffinityTTL: time.Hour, AffinityMaxEntries: 1024,
	}, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	client, err := NewClient(cfg, pool, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	modelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err = client.RefreshModels(modelCtx, true)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if pending := pool.AccountsNeedingModelRefresh(6 * time.Hour); len(pending) != 0 {
		t.Fatalf("model catalogs were not persisted for %d credential accounts", len(pending))
	}

	ids := pool.AccountIDs()
	jobs := make(chan string)
	results := make(chan int, len(ids))
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				results <- probeAccountStatus(client, pool, id, "billing?format=credits")
			}
		}()
	}
	for _, id := range ids {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	close(results)
	billing := map[int]int{}
	for status := range results {
		billing[status]++
	}
	t.Logf("anonymous probe summary: accounts=%d aggregate_models=%d billing=%s", len(ids), len(pool.Models()), statusSummary(billing))
	if billing[http.StatusOK] != len(ids) {
		t.Fatalf("billing probe did not succeed for every account: %s", statusSummary(billing))
	}
}

func probeAccountStatus(client *Client, pool *auth.Pool, accountID, path string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lease, err := pool.AcquireAccountForMetadata(ctx, accountID)
	if err != nil {
		return 0
	}
	defer lease.Release()
	resp, _, err := client.do(ctx, lease, http.MethodGet, path, nil, NewID(), "", false, false)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func statusSummary(counts map[int]int) string {
	return fmt.Sprint(counts)
}
