package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadFlatOAuthCredential(t *testing.T) {
	dir := t.TempDir()
	path := writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	cred, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "token-a" || cred.Subject != "subject-a" || cred.ClientID != "client-id" {
		t.Fatalf("unexpected credential metadata: token=%t subject=%q client=%q", cred.AccessToken != "", cred.Subject, cred.ClientID)
	}
	if cred.session().Token != "token-a" || cred.session().UserID != "subject-a" {
		t.Fatalf("unexpected session: %#v", cred.session())
	}
}

func TestRefreshRotatesAndPersistsCredential(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "client-id" || r.Form.Get("refresh_token") != "refresh-a" {
			t.Errorf("unexpected refresh form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	path := writeTestCredential(t, dir, "a.json", "subject-a", "token-old", time.Now().Add(-time.Minute), server.URL)
	cred, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	next, err := cred.refresh(context.Background(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || next.AccessToken != "token-new" || next.RefreshToken != "refresh-new" {
		t.Fatalf("refresh result calls=%d token=%q refresh=%q", calls.Load(), next.AccessToken, next.RefreshToken)
	}
	reloaded, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AccessToken != "token-new" || !reloaded.ExpiresAt.After(time.Now()) {
		t.Fatalf("refresh was not persisted: %#v", reloaded)
	}
}

func TestConcurrentRefreshIsSingleFlight(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	path := writeTestCredential(t, dir, "a.json", "subject-a", "token-old", time.Now().Add(-time.Minute), server.URL)
	cred, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	a := &account{id: accountID(cred.Subject), credential: cred, agentID: "agent", sessionID: "session"}
	p := &Pool{
		cfg: PoolConfig{RefreshConcurrency: 4}, http: server.Client(), accounts: map[string]*account{a.id: a},
		files: map[string]fileEntry{path: {cred: cred}}, states: map[string]accountState{},
		affinity: newAffinityCache(time.Hour, 100), refreshSem: make(chan struct{}, 4), closed: make(chan struct{}),
	}
	p.active.Store([]*account{a})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.ensureFresh(context.Background(), a, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
}

func TestPoolRoundRobinAffinityAndConcurrentLease(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		writeTestCredential(t, dir, fmt.Sprintf("%d.json", i), fmt.Sprintf("subject-%d", i), fmt.Sprintf("token-%d", i), time.Now().Add(time.Hour), "")
	}
	pool := newTestPool(t, dir)
	defer pool.Close()
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		lease, err := pool.Acquire(context.Background(), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		seen[lease.AccountID()] = true
		lease.Release()
	}
	if len(seen) != 3 {
		t.Fatalf("round robin selected %d accounts", len(seen))
	}
	first, err := pool.Acquire(context.Background(), "session:one", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := first.AccountID()
	first.Release()
	pool.BindResponseID("resp-one", id)
	byResponse, err := pool.Acquire(context.Background(), "previous:resp-one", nil)
	if err != nil {
		t.Fatal(err)
	}
	if byResponse.AccountID() != id {
		t.Fatalf("response affinity moved from %s to %s", id, byResponse.AccountID())
	}
	byResponse.Release()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := pool.Acquire(context.Background(), "session:one", nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer lease.Release()
			if lease.AccountID() != id {
				t.Errorf("affinity moved from %s to %s", id, lease.AccountID())
			}
		}()
	}
	wg.Wait()
}

func TestPoolDeduplicatesAccountsBySubject(t *testing.T) {
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "same-subject", "token-a", time.Now().Add(time.Hour), "")
	writeTestCredential(t, dir, "b.json", "same-subject", "token-b", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	defer pool.Close()
	if got := len(pool.AccountIDs()); got != 1 {
		t.Fatalf("deduplicated account count = %d, want 1", got)
	}
}

func TestAffinityCacheTTLAndCapacity(t *testing.T) {
	expiring := newAffinityCache(10*time.Millisecond, 64)
	expiring.Set("session", "account")
	time.Sleep(20 * time.Millisecond)
	if _, ok := expiring.Get("session"); ok {
		t.Fatal("expired affinity entry was returned")
	}

	bounded := newAffinityCache(time.Hour, 64)
	for i := 0; i < 1000; i++ {
		bounded.Set(fmt.Sprintf("session-%d", i), "account")
	}
	total := 0
	for i := range bounded.shards {
		bounded.shards[i].Lock()
		total += len(bounded.shards[i].entries)
		bounded.shards[i].Unlock()
	}
	if total > 64 {
		t.Fatalf("affinity cache size = %d, limit 64", total)
	}
}

func TestCooldownPersistsAndAffinityMigrates(t *testing.T) {
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	writeTestCredential(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	lease, err := pool.Acquire(context.Background(), "session:one", nil)
	if err != nil {
		t.Fatal(err)
	}
	cooledID := lease.AccountID()
	lease.Release()
	pool.MarkCooldown(cooledID, "quota_exhausted", time.Hour)
	migrated, err := pool.Acquire(context.Background(), "session:one", nil)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.AccountID() == cooledID {
		t.Fatal("affinity did not migrate away from cooled account")
	}
	migrated.Release()
	pool.Close()
	stateBytes, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBytes), "subject-a") || strings.Contains(string(stateBytes), "token-a") {
		t.Fatal("scheduler state persisted credential data")
	}

	reloaded := newTestPool(t, dir)
	defer reloaded.Close()
	if lease, err := reloaded.AcquireAccount(context.Background(), cooledID); err == nil {
		lease.Release()
		t.Fatal("persisted cooldown was not restored")
	}
}

func TestHotReloadAddsAndRemovesCredentials(t *testing.T) {
	dir := t.TempDir()
	pathA := writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	defer pool.Close()
	writeTestCredential(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "")
	if err := pool.scan(); err != nil {
		t.Fatal(err)
	}
	pool.mu.RLock()
	if len(pool.accounts) != 2 {
		t.Fatalf("account count after add = %d", len(pool.accounts))
	}
	pool.mu.RUnlock()
	if err := os.Remove(pathA); err != nil {
		t.Fatal(err)
	}
	if err := pool.scan(); err != nil {
		t.Fatal(err)
	}
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	if len(pool.accounts) != 1 {
		t.Fatalf("account count after remove = %d", len(pool.accounts))
	}
}

func BenchmarkPoolAcquireTenThousandAccounts(b *testing.B) {
	p := &Pool{
		accounts: map[string]*account{}, states: map[string]accountState{},
		affinity: newAffinityCache(time.Hour, 100000), refreshSem: make(chan struct{}, 4), closed: make(chan struct{}),
	}
	active := make([]*account, 10000)
	for i := range active {
		id := fmt.Sprintf("%024d", i)
		a := &account{id: id, credential: &credential{AccessToken: "token", Subject: id, ExpiresAt: time.Now().Add(time.Hour)}, agentID: "agent", sessionID: "session"}
		p.accounts[id], active[i] = a, a
	}
	p.active.Store(active)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lease, err := p.Acquire(context.Background(), "", nil)
			if err != nil {
				b.Fatal(err)
			}
			lease.Release()
		}
	})
}

func newTestPool(t *testing.T, dir string) *Pool {
	t.Helper()
	pool, err := NewPool(context.Background(), PoolConfig{Dir: dir, Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 2, AffinityTTL: time.Hour, AffinityMaxEntries: 1024}, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func writeTestCredential(t *testing.T, dir, name, subject, token string, expires time.Time, tokenURL string) string {
	t.Helper()
	if tokenURL == "" {
		tokenURL = "https://auth.x.ai/oauth2/token"
	}
	raw := map[string]any{
		"type": "xai", "auth_kind": "oauth", "access_token": token,
		"refresh_token": "refresh-a", "client_id": "client-id", "sub": subject,
		"expired": expires.UTC().Format(time.RFC3339Nano), "expires_in": 3600,
		"token_endpoint": tokenURL,
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
