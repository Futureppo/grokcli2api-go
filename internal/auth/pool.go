package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const stateFileName = ".grokcli2api-state.json"

var errAccountBusy = errors.New("credential account is at its in-flight limit")

type AffinityMode uint8

const (
	AffinityNone AffinityMode = iota
	AffinitySoft
	AffinityHard
)

type Affinity struct {
	Key  string
	Mode AffinityMode
}

type PoolConfig struct {
	Dir                string
	Surface            string
	ReloadInterval     time.Duration
	RefreshConcurrency int
	AccountMaxInflight int
	AffinityTTL        time.Duration
	AffinityMaxEntries int
}

type UnavailableError struct {
	Cooling    bool
	RetryAfter time.Duration
}

type ModelUnavailableError struct{ Model string }

func (e *ModelUnavailableError) Error() string {
	return "no credential account advertises model " + e.Model
}

func (e *UnavailableError) Error() string {
	if e.Cooling {
		return "all credential accounts are cooling down"
	}
	return "no usable credential accounts"
}

type account struct {
	id        string
	agentID   string
	sessionID string

	mu            sync.RWMutex
	credential    *credential
	cooldownUntil time.Time
	cooldownCause string
	disabled      bool
	disableReason string
	refreshOnce   sync.Once
	refreshLock   chan struct{}
	generation    atomic.Uint64
	inflight      atomic.Int64
}

func (a *account) currentGeneration() uint64 {
	if generation := a.generation.Load(); generation != 0 {
		return generation
	}
	a.generation.CompareAndSwap(0, 1)
	return a.generation.Load()
}

func (a *account) acquireRefresh(ctx context.Context) error {
	a.refreshOnce.Do(func() {
		a.refreshLock = make(chan struct{}, 1)
		a.refreshLock <- struct{}{}
	})
	select {
	case <-a.refreshLock:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *account) releaseRefresh() { a.refreshLock <- struct{}{} }

func (a *account) available(now time.Time) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.disabled && !now.Before(a.cooldownUntil) && a.credential != nil
}

func (a *account) requestUsable(now time.Time) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.disabled && !now.Before(a.cooldownUntil) && a.credential != nil && a.credential.usable(now)
}

func (a *account) supportsModel(model string) bool {
	if model == "" {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.disabled || a.credential == nil {
		return false
	}
	index := sort.SearchStrings(a.credential.Models, model)
	return index < len(a.credential.Models) && a.credential.Models[index] == model
}

func (a *account) snapshot() (*credential, time.Time, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.credential, a.cooldownUntil, a.disabled
}

type fileEntry struct {
	size    int64
	modTime time.Time
	cred    *credential
}

type persistedState struct {
	Version  int                     `json:"version"`
	Accounts map[string]accountState `json:"accounts"`
}

type accountState struct {
	CooldownUntil         time.Time `json:"cooldown_until,omitempty"`
	Reason                string    `json:"reason,omitempty"`
	Disabled              bool      `json:"disabled,omitempty"`
	CredentialFingerprint string    `json:"credential_fingerprint,omitempty"`
}

type Pool struct {
	cfg           PoolConfig
	http          *http.Client
	mu            sync.RWMutex
	accounts      map[string]*account
	files         map[string]fileEntry
	states        map[string]accountState
	active        atomic.Value // []*account
	activeByModel atomic.Value // map[string][]*account
	cursor        atomic.Uint64
	affinity      *affinityCache
	refreshSem    chan struct{}
	capacityCh    chan struct{}
	capacityMu    sync.Mutex
	rebuildCh     chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
	wg            sync.WaitGroup
	stateMu       sync.Mutex
}

type Lease struct {
	pool       *Pool
	account    *account
	credential *credential
	generation uint64
	once       sync.Once
}

func (p *Pool) newLease(a *account) *Lease {
	a.mu.RLock()
	cred := a.credential
	generation := a.currentGeneration()
	a.mu.RUnlock()
	return &Lease{pool: p, account: a, credential: cred, generation: generation}
}

func (l *Lease) Session() Session {
	if l.credential == nil {
		return Session{}
	}
	return l.credential.session()
}
func (l *Lease) AccountID() string  { return l.account.id }
func (l *Lease) AgentID() string    { return l.account.agentID }
func (l *Lease) SessionID() string  { return l.account.sessionID }
func (l *Lease) Generation() uint64 { return l.generation }
func (l *Lease) Release() {
	if l == nil || l.account == nil {
		return
	}
	l.once.Do(func() {
		l.account.inflight.Add(-1)
		l.pool.notifyCapacity()
	})
}

func NewPool(ctx context.Context, cfg PoolConfig, client *http.Client) (*Pool, error) {
	if cfg.Dir == "" {
		cfg.Dir = "./auths"
	}
	if cfg.ReloadInterval <= 0 {
		cfg.ReloadInterval = 30 * time.Second
	}
	if cfg.RefreshConcurrency < 1 {
		cfg.RefreshConcurrency = 4
	}
	if cfg.AccountMaxInflight < 1 {
		cfg.AccountMaxInflight = 16
	}
	if cfg.AffinityTTL <= 0 {
		cfg.AffinityTTL = time.Hour
	}
	if cfg.AffinityMaxEntries < 1 {
		cfg.AffinityMaxEntries = 100000
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	p := &Pool{
		cfg: cfg, http: client, accounts: map[string]*account{}, files: map[string]fileEntry{},
		states: map[string]accountState{}, affinity: newAffinityCache(cfg.AffinityTTL, cfg.AffinityMaxEntries),
		refreshSem: make(chan struct{}, cfg.RefreshConcurrency), capacityCh: make(chan struct{}),
		rebuildCh: make(chan struct{}, 1), closed: make(chan struct{}),
	}
	p.active.Store([]*account{})
	p.activeByModel.Store(map[string][]*account{})
	_ = p.loadState()
	if err := p.scan(); err != nil {
		return nil, err
	}
	if len(p.accounts) == 0 {
		return nil, ErrNoAuth
	}
	if !p.hasReadyAccount() {
		warmup, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := p.warmup(warmup)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	p.wg.Add(2)
	go p.background()
	go p.rebuildLoop()
	return p, nil
}

func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		close(p.closed)
		p.wg.Wait()
		_ = p.persistState()
	})
}

func (p *Pool) Acquire(ctx context.Context, affinity Affinity, model string, exclude map[string]struct{}) (*Lease, error) {
	cacheKey := modelAffinityKey(affinity.Key, model)
	var refreshFailed map[string]struct{}
	for {
		capacity := p.capacitySignal()
		if affinity.Key != "" {
			if id, ok := p.affinity.Get(cacheKey); ok {
				var lease *Lease
				var err error
				if affinity.Mode == AffinitySoft && !p.accountRequestUsable(id) {
					err = errAccountBusy
				} else {
					lease, err = p.acquireID(ctx, id, model, exclude)
				}
				if err == nil {
					return lease, nil
				}
				if errors.Is(err, errAccountBusy) && affinity.Mode == AffinityHard {
					if err := p.waitForCapacity(ctx, capacity); err != nil {
						return nil, err
					}
					continue
				}
				if !errors.Is(err, errAccountBusy) {
					p.affinity.Delete(cacheKey)
				}
			}
		}
		active := p.schedulingSnapshot(model)
		if len(active) == 0 {
			p.rebuildActive()
			active = p.schedulingSnapshot(model)
			if len(active) == 0 {
				if model != "" && !p.hasKnownModel(model) {
					return nil, &ModelUnavailableError{Model: model}
				}
				return nil, p.unavailable()
			}
		}
		start := int(p.cursor.Add(1)-1) % len(active)
		saturated := false
		const schedulingChoices = 4
		for batch := 0; batch < len(active); batch += schedulingChoices {
			var selected *account
			selectedInflight := int64(^uint64(0) >> 1)
			selectedUsable := false
			for offset := 0; offset < schedulingChoices && batch+offset < len(active); offset++ {
				a := active[(start+batch+offset)%len(active)]
				_, excluded := exclude[a.id]
				_, failedRefresh := refreshFailed[a.id]
				if excluded || failedRefresh || !a.available(time.Now()) || !a.supportsModel(model) {
					continue
				}
				inflight := a.inflight.Load()
				if p.cfg.AccountMaxInflight > 0 && inflight >= int64(p.cfg.AccountMaxInflight) {
					saturated = true
					continue
				}
				usable := a.requestUsable(time.Now())
				if selected == nil || usable && !selectedUsable || usable == selectedUsable && inflight < selectedInflight {
					selected, selectedInflight, selectedUsable = a, inflight, usable
				}
			}
			if selected != nil {
				if !selected.tryAcquire(p.cfg.AccountMaxInflight) {
					saturated = true
					continue
				}
				if err := p.ensureUsable(ctx, selected); err != nil {
					selected.inflight.Add(-1)
					p.notifyCapacity()
					if refreshFailed == nil {
						refreshFailed = make(map[string]struct{})
					}
					refreshFailed[selected.id] = struct{}{}
					continue
				}
				if affinity.Key != "" {
					p.affinity.Set(cacheKey, selected.id)
				}
				return p.newLease(selected), nil
			}
		}
		if saturated {
			if err := p.waitForCapacity(ctx, capacity); err != nil {
				return nil, err
			}
			continue
		}
		if model != "" && !p.hasKnownModel(model) {
			return nil, &ModelUnavailableError{Model: model}
		}
		return nil, p.unavailable()
	}
}

func (p *Pool) accountRequestUsable(id string) bool {
	p.mu.RLock()
	a := p.accounts[id]
	p.mu.RUnlock()
	return a != nil && a.requestUsable(time.Now())
}

func (a *account) tryAcquire(limit int) bool {
	if limit < 1 {
		a.inflight.Add(1)
		return true
	}
	for {
		current := a.inflight.Load()
		if current >= int64(limit) {
			return false
		}
		if a.inflight.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (p *Pool) notifyCapacity() {
	p.capacityMu.Lock()
	if p.capacityCh != nil {
		close(p.capacityCh)
	}
	p.capacityCh = make(chan struct{})
	p.capacityMu.Unlock()
}

func (p *Pool) capacitySignal() <-chan struct{} {
	p.capacityMu.Lock()
	if p.capacityCh == nil {
		p.capacityCh = make(chan struct{})
	}
	ch := p.capacityCh
	p.capacityMu.Unlock()
	return ch
}

func (p *Pool) waitForCapacity(ctx context.Context, capacity <-chan struct{}) error {
	select {
	case <-capacity:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closed:
		return ErrNoAuth
	}
}

func (p *Pool) schedulingSnapshot(model string) []*account {
	if model == "" {
		return p.active.Load().([]*account)
	}
	return p.activeByModel.Load().(map[string][]*account)[model]
}

func (p *Pool) AcquireAccount(ctx context.Context, id string) (*Lease, error) {
	for {
		capacity := p.capacitySignal()
		lease, err := p.acquireID(ctx, id, "", nil)
		if !errors.Is(err, errAccountBusy) {
			return lease, err
		}
		if err := p.waitForCapacity(ctx, capacity); err != nil {
			return nil, err
		}
	}
}

// AcquireAccountForMetadata ignores quota/rate cooldowns so non-generative
// capability discovery can still refresh an account's model catalog.
func (p *Pool) AcquireAccountForMetadata(ctx context.Context, id string) (*Lease, error) {
	p.mu.RLock()
	a := p.accounts[id]
	p.mu.RUnlock()
	if a == nil {
		return nil, ErrNoAuth
	}
	_, _, disabled := a.snapshot()
	if disabled {
		return nil, ErrNoAuth
	}
	if err := p.ensureUsable(ctx, a); err != nil {
		return nil, err
	}
	a.inflight.Add(1)
	return p.newLease(a), nil
}

func (p *Pool) acquireID(ctx context.Context, id, model string, exclude map[string]struct{}) (*Lease, error) {
	if _, skipped := exclude[id]; skipped {
		return nil, ErrNoAuth
	}
	p.mu.RLock()
	a := p.accounts[id]
	p.mu.RUnlock()
	if a == nil || !a.available(time.Now()) || !a.supportsModel(model) {
		return nil, ErrNoAuth
	}
	if !a.tryAcquire(p.cfg.AccountMaxInflight) {
		return nil, errAccountBusy
	}
	if err := p.ensureUsable(ctx, a); err != nil {
		a.inflight.Add(-1)
		p.notifyCapacity()
		return nil, err
	}
	return p.newLease(a), nil
}

func (p *Pool) Bind(affinity Affinity, model, accountID string) {
	if affinity.Key != "" && accountID != "" {
		p.affinity.Set(modelAffinityKey(affinity.Key, model), accountID)
	}
}

func (p *Pool) BindResponseID(responseID, model, accountID string) {
	if responseID != "" {
		p.affinity.Set(modelAffinityKey("previous:"+responseID, model), accountID)
	}
}

// AccountIDs returns anonymous stable identifiers for diagnostics and tests.
// It never exposes credential paths, subjects, or tokens.
func (p *Pool) AccountIDs() []string {
	p.mu.RLock()
	ids := make([]string, 0, len(p.accounts))
	for id := range p.accounts {
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

func (p *Pool) Models() []string {
	seen := map[string]struct{}{}
	p.mu.RLock()
	for _, a := range p.accounts {
		a.mu.RLock()
		if !a.disabled && a.credential != nil {
			for _, model := range a.credential.Models {
				seen[model] = struct{}{}
			}
		}
		a.mu.RUnlock()
	}
	p.mu.RUnlock()
	models := make([]string, 0, len(seen))
	for model := range seen {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (p *Pool) HasModel(model string) bool {
	if model == "" {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.supportsModel(model) {
			return true
		}
	}
	return false
}

func (p *Pool) hasKnownModel(model string) bool {
	if model == "" {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		a.mu.RLock()
		known := false
		if cred := a.credential; cred != nil {
			index := sort.SearchStrings(cred.Models, model)
			known = index < len(cred.Models) && cred.Models[index] == model
		}
		a.mu.RUnlock()
		if known {
			return true
		}
	}
	return false
}

func (p *Pool) AccountsNeedingModelRefresh(interval time.Duration) []string {
	now := time.Now()
	p.mu.RLock()
	ids := make([]string, 0, len(p.accounts))
	for id, a := range p.accounts {
		a.mu.RLock()
		needs := !a.disabled && a.credential != nil && (len(a.credential.Models) == 0 || a.credential.ModelsUpdatedAt.IsZero() || now.Sub(a.credential.ModelsUpdatedAt) >= interval)
		a.mu.RUnlock()
		if needs {
			ids = append(ids, id)
		}
	}
	p.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

func (p *Pool) UpdateModels(accountID string, models []string, updatedAt time.Time) error {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	if err := a.acquireRefresh(context.Background()); err != nil {
		return err
	}
	defer a.releaseRefresh()
	cred, _, disabled := a.snapshot()
	if cred == nil || disabled {
		return ErrNoAuth
	}
	normalized := stringSlice(models)
	if len(normalized) == 0 {
		return errors.New("upstream returned no models")
	}
	next := *cred
	next.Raw = cloneMap(cred.Raw)
	next.Models = normalized
	next.ModelsUpdatedAt = updatedAt.UTC()
	node := credentialNode(next.Raw)
	node["models"] = normalized
	node["models_updated_at"] = next.ModelsUpdatedAt.Format(time.RFC3339Nano)
	if err := writeCredentialAtomic(next.Path, next.Raw); err != nil {
		return err
	}
	a.mu.Lock()
	a.credential = &next
	a.mu.Unlock()
	p.mu.Lock()
	if info, err := os.Stat(next.Path); err == nil {
		p.files[next.Path] = fileEntry{size: info.Size(), modTime: info.ModTime(), cred: &next}
	}
	p.mu.Unlock()
	p.requestRebuild()
	return nil
}

// RebuildSchedulingSnapshot publishes a consistent account index after a
// batch of model-catalog updates. Routine state changes remain debounce-batched.
func (p *Pool) RebuildSchedulingSnapshot() {
	p.rebuildActive()
}

func (p *Pool) Refresh(ctx context.Context, accountID string) error {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	return p.ensureFresh(ctx, a, true)
}

// RefreshIfUnchanged collapses concurrent 401 recovery for requests that used
// the same credential generation. Once one caller refreshes the account, later
// callers observe the new generation and reuse it without another OAuth call.
func (p *Pool) RefreshIfUnchanged(ctx context.Context, accountID string, observedGeneration uint64) error {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	return p.refreshCredential(ctx, a, true, observedGeneration, true)
}

func (p *Pool) MarkCooldown(accountID, reason string, duration time.Duration) {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return
	}
	now := time.Now()
	until := now.Add(duration)
	a.mu.Lock()
	remaining := time.Until(a.cooldownUntil)
	extensionThreshold := remaining / 10
	if extensionThreshold < 5*time.Second {
		extensionThreshold = 5 * time.Second
	}
	changed := !now.Before(a.cooldownUntil) || until.After(a.cooldownUntil.Add(extensionThreshold))
	if changed {
		a.cooldownUntil, a.cooldownCause = until, reason
	} else {
		until = a.cooldownUntil
		reason = a.cooldownCause
	}
	a.mu.Unlock()
	if !changed {
		return
	}
	p.mu.Lock()
	p.states[accountID] = accountState{CooldownUntil: until, Reason: reason}
	p.mu.Unlock()
	p.requestRebuild()
	p.rebuildWhenCooldownExpires(until)
	_ = p.persistState()
	slog.Warn("credential account cooling", "account", accountID, "reason", reason, "until", until.UTC().Format(time.RFC3339))
}

func (p *Pool) rebuildWhenCooldownExpires(until time.Time) {
	go func() {
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		select {
		case <-timer.C:
			p.rebuildActive()
			p.notifyCapacity()
		case <-p.closed:
		}
	}()
}

func (p *Pool) Disable(accountID, reason string) {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return
	}
	a.mu.Lock()
	changed := !a.disabled || a.disableReason != reason
	a.disabled, a.disableReason = true, reason
	fingerprint := credentialFingerprint(a.credential)
	a.mu.Unlock()
	if !changed {
		return
	}
	p.mu.Lock()
	p.states[accountID] = accountState{Disabled: true, Reason: reason, CredentialFingerprint: fingerprint}
	p.mu.Unlock()
	p.requestRebuild()
	if err := p.persistState(); err != nil {
		slog.Error("credential scheduler state persistence failed", "error", err)
	}
	slog.Warn("credential account disabled", "account", accountID, "reason", reason)
}

func credentialFingerprint(cred *credential) string {
	if cred == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(cred.AccessToken + "\x00" + cred.RefreshToken))
	return hex.EncodeToString(sum[:])
}

func (p *Pool) hasReadyAccount() bool {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		cred, _, disabled := a.snapshot()
		if !disabled && cred != nil && cred.AccessToken != "" && !cred.needsRefresh(now, 0) {
			return true
		}
	}
	return false
}

func (p *Pool) warmup(ctx context.Context) error {
	p.mu.RLock()
	accounts := make([]*account, 0, len(p.accounts))
	for _, a := range p.accounts {
		accounts = append(accounts, a)
	}
	p.mu.RUnlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *account)
	results := make(chan error, len(accounts))
	workers := p.cfg.RefreshConcurrency
	if workers > len(accounts) {
		workers = len(accounts)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range jobs {
				results <- p.ensureFresh(ctx, a, true)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, a := range accounts {
			select {
			case jobs <- a:
			case <-ctx.Done():
				return
			}
		}
	}()
	var last error
	for range accounts {
		select {
		case err := <-results:
			if err == nil {
				cancel()
				wg.Wait()
				return nil
			}
			last = err
		case <-ctx.Done():
			wg.Wait()
			return fmt.Errorf("credential warmup: %w", ctx.Err())
		}
	}
	if last == nil {
		last = ErrNoAuth
	}
	wg.Wait()
	return fmt.Errorf("credential warmup: %w", last)
}

func (p *Pool) ensureUsable(ctx context.Context, a *account) error {
	cred, _, disabled := a.snapshot()
	if cred == nil || disabled {
		return ErrNoAuth
	}
	if cred.usable(time.Now()) {
		return nil
	}
	return p.ensureFresh(ctx, a, false)
}

func (p *Pool) ensureFresh(ctx context.Context, a *account, force bool) error {
	return p.refreshCredential(ctx, a, force, 0, false)
}

func (p *Pool) refreshCredential(ctx context.Context, a *account, force bool, observedGeneration uint64, compareGeneration bool) error {
	if err := a.acquireRefresh(ctx); err != nil {
		return err
	}
	defer a.releaseRefresh()
	if compareGeneration && a.currentGeneration() != observedGeneration {
		return nil
	}
	cred, _, disabled := a.snapshot()
	if cred == nil || disabled {
		return ErrNoAuth
	}
	if !force && !cred.needsRefresh(time.Now(), deterministicJitter(a.id)) {
		return nil
	}
	select {
	case p.refreshSem <- struct{}{}:
		defer func() { <-p.refreshSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	next, err := cred.refresh(refreshCtx, p.http)
	if err != nil {
		if ctx.Err() != nil || refreshCtx.Err() == context.Canceled {
			return err
		}
		var refreshErr *RefreshError
		if errors.As(err, &refreshErr) && refreshErr.Permanent {
			p.Disable(a.id, "refresh_invalid")
		} else {
			p.MarkCooldown(a.id, "refresh_backoff", time.Minute)
		}
		return err
	}
	a.mu.Lock()
	a.credential = next
	a.generation.Add(1)
	a.disabled = false
	a.disableReason = ""
	keepCooldown := time.Now().Before(a.cooldownUntil) && a.cooldownCause != "" && a.cooldownCause != "refresh_backoff"
	if !keepCooldown {
		a.cooldownUntil = time.Time{}
		a.cooldownCause = ""
	}
	a.mu.Unlock()
	p.mu.Lock()
	if !keepCooldown {
		delete(p.states, a.id)
	}
	if cached, ok := p.files[next.Path]; ok {
		if info, statErr := os.Stat(next.Path); statErr == nil {
			cached.modTime, cached.size, cached.cred = info.ModTime(), info.Size(), next
			p.files[next.Path] = cached
		}
	}
	p.mu.Unlock()
	p.requestRebuild()
	p.notifyCapacity()
	slog.Info("credential refreshed", "account", a.id)
	return nil
}

func (p *Pool) background() {
	defer p.wg.Done()
	p.refreshAllDue()
	ticker := time.NewTicker(p.cfg.ReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := p.scan(); err != nil {
				slog.Error("credential directory reload failed", "error", err)
			}
			p.refreshAllDue()
		case <-p.closed:
			return
		}
	}
}

func (p *Pool) refreshAllDue() {
	p.mu.RLock()
	accounts := make([]*account, 0, len(p.accounts))
	for _, a := range p.accounts {
		cred, _, disabled := a.snapshot()
		if !disabled && cred != nil && cred.needsRefresh(time.Now(), deterministicJitter(a.id)) {
			accounts = append(accounts, a)
		}
	}
	p.mu.RUnlock()
	if len(accounts) == 0 {
		return
	}
	jobs := make(chan *account)
	var wg sync.WaitGroup
	workers := p.cfg.RefreshConcurrency
	if workers > len(accounts) {
		workers = len(accounts)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range jobs {
				select {
				case <-p.closed:
					return
				default:
					_ = p.ensureFresh(context.Background(), a, false)
				}
			}
		}()
	}
	for _, a := range accounts {
		select {
		case jobs <- a:
		case <-p.closed:
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

func (p *Pool) scan() error {
	entries, err := os.ReadDir(p.cfg.Dir)
	if err != nil {
		return fmt.Errorf("read auths directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	seen := map[string]struct{}{}
	parsed := map[string]*credential{}
	newFiles := map[string]fileEntry{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.EqualFold(filepath.Ext(name), ".json") {
			continue
		}
		path := filepath.Join(p.cfg.Dir, name)
		info, err := entry.Info()
		if err != nil {
			continue
		}
		p.mu.RLock()
		cached, ok := p.files[path]
		p.mu.RUnlock()
		var cred *credential
		if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
			cred = cached.cred
		} else {
			cred, err = loadCredential(path, p.cfg.Surface)
			if err != nil {
				slog.Warn("credential file skipped", "reason", "invalid_format")
				continue
			}
		}
		id := accountID(cred.Subject)
		if _, duplicate := seen[id]; duplicate {
			slog.Warn("duplicate credential skipped", "account", id)
			continue
		}
		seen[id] = struct{}{}
		parsed[id] = cred
		newFiles[path] = fileEntry{size: info.Size(), modTime: info.ModTime(), cred: cred}
	}
	if len(parsed) == 0 {
		p.mu.Lock()
		hadAccounts := len(p.accounts) > 0
		for _, a := range p.accounts {
			a.mu.Lock()
			a.disabled, a.disableReason = true, "removed"
			a.mu.Unlock()
		}
		p.accounts = map[string]*account{}
		p.files = map[string]fileEntry{}
		p.mu.Unlock()
		p.rebuildActive()
		if hadAccounts {
			slog.Warn("credential pool is empty")
			return nil
		}
		return ErrNoAuth
	}
	p.mu.Lock()
	poolChanged := len(p.files) != len(newFiles) || len(p.accounts) != len(parsed)
	if !poolChanged {
		for path, next := range newFiles {
			current, ok := p.files[path]
			if !ok || current.size != next.size || !current.modTime.Equal(next.modTime) {
				poolChanged = true
				break
			}
		}
	}
	var cooldowns []time.Time
	for id, cred := range parsed {
		if existing := p.accounts[id]; existing != nil {
			existing.mu.Lock()
			credentialChanged := existing.credential == nil || existing.credential.Path != cred.Path || existing.credential.AccessToken != cred.AccessToken || existing.credential.RefreshToken != cred.RefreshToken
			existing.credential = cred
			if credentialChanged {
				existing.generation.Add(1)
				poolChanged = true
				existing.disabled = false
				existing.disableReason = ""
				existing.cooldownUntil = time.Time{}
				existing.cooldownCause = ""
				delete(p.states, id)
			}
			existing.mu.Unlock()
			continue
		}
		a := &account{id: id, credential: cred, agentID: randomHex(16), sessionID: randomUUID()}
		a.generation.Store(1)
		if state, ok := p.states[id]; ok {
			if state.Disabled {
				fingerprintChanged := state.CredentialFingerprint != "" && state.CredentialFingerprint != credentialFingerprint(cred)
				if fingerprintChanged {
					delete(p.states, id)
				} else {
					a.disabled, a.disableReason = true, state.Reason
				}
			} else if time.Now().Before(state.CooldownUntil) {
				a.cooldownUntil, a.cooldownCause = state.CooldownUntil, state.Reason
				cooldowns = append(cooldowns, state.CooldownUntil)
			}
		}
		p.accounts[id] = a
		poolChanged = true
	}
	for id, a := range p.accounts {
		if _, ok := parsed[id]; !ok {
			a.mu.Lock()
			a.disabled, a.disableReason = true, "removed"
			a.mu.Unlock()
			delete(p.accounts, id)
			poolChanged = true
		}
	}
	p.files = newFiles
	count := len(p.accounts)
	p.mu.Unlock()
	for _, until := range cooldowns {
		p.rebuildWhenCooldownExpires(until)
	}
	if poolChanged {
		p.rebuildActive()
		p.notifyCapacity()
		slog.Info("credential pool loaded", "accounts", count)
	}
	return nil
}

func (p *Pool) rebuildActive() {
	now := time.Now()
	p.mu.RLock()
	active := make([]*account, 0, len(p.accounts))
	byModel := map[string][]*account{}
	for _, a := range p.accounts {
		a.mu.RLock()
		available := !a.disabled && !now.Before(a.cooldownUntil) && a.credential != nil
		var models []string
		if available {
			models = append(models, a.credential.Models...)
		}
		a.mu.RUnlock()
		if !available {
			continue
		}
		active = append(active, a)
		for _, model := range models {
			byModel[model] = append(byModel[model], a)
		}
	}
	p.mu.RUnlock()
	sort.Slice(active, func(i, j int) bool { return active[i].id < active[j].id })
	for model := range byModel {
		accounts := byModel[model]
		sort.Slice(accounts, func(i, j int) bool { return accounts[i].id < accounts[j].id })
	}
	p.active.Store(active)
	p.activeByModel.Store(byModel)
}

func (p *Pool) requestRebuild() {
	select {
	case p.rebuildCh <- struct{}{}:
	default:
	}
}

func (p *Pool) rebuildLoop() {
	defer p.wg.Done()
	for {
		select {
		case <-p.rebuildCh:
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-timer.C:
				p.rebuildActive()
			case <-p.closed:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			}
		case <-p.closed:
			return
		}
	}
}

func (p *Pool) unavailable() error {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	var earliest time.Time
	hasCooling := false
	for _, a := range p.accounts {
		_, until, disabled := a.snapshot()
		if !disabled && now.Before(until) {
			hasCooling = true
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
		}
	}
	if hasCooling {
		return &UnavailableError{Cooling: true, RetryAfter: time.Until(earliest)}
	}
	return &UnavailableError{}
}

func (p *Pool) loadState() error {
	b, err := os.ReadFile(filepath.Join(p.cfg.Dir, stateFileName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state persistedState
	if err := json.Unmarshal(b, &state); err != nil {
		return err
	}
	now := time.Now()
	for id, item := range state.Accounts {
		if item.Disabled || now.Before(item.CooldownUntil) {
			p.states[id] = item
		}
	}
	return nil
}

func (p *Pool) persistState() error {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.mu.RLock()
	state := persistedState{Version: 1, Accounts: map[string]accountState{}}
	for id, item := range p.states {
		if item.Disabled || time.Now().Before(item.CooldownUntil) {
			state.Accounts[id] = item
		}
	}
	p.mu.RUnlock()
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := filepath.Join(p.cfg.Dir, stateFileName)
	tmp, err := os.CreateTemp(p.cfg.Dir, ".grok-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

type affinityEntry struct {
	accountID string
	expiresAt time.Time
}
type affinityShard struct {
	sync.Mutex
	entries map[string]affinityEntry
}
type affinityCache struct {
	shards []affinityShard
	ttl    time.Duration
	limit  int
}

func newAffinityCache(ttl time.Duration, maxEntries int) *affinityCache {
	const shardCount = 64
	c := &affinityCache{shards: make([]affinityShard, shardCount), ttl: ttl, limit: (maxEntries + shardCount - 1) / shardCount}
	for i := range c.shards {
		c.shards[i].entries = map[string]affinityEntry{}
	}
	return c
}

func affinityHash(key string) (string, byte) {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), sum[0]
}

func modelAffinityKey(affinity, model string) string {
	if affinity == "" {
		return ""
	}
	return affinity + "\x00" + model
}

func (c *affinityCache) Get(key string) (string, bool) {
	hash, shardKey := affinityHash(key)
	shard := &c.shards[int(shardKey)%len(c.shards)]
	shard.Lock()
	defer shard.Unlock()
	entry, ok := shard.entries[hash]
	if !ok || time.Now().After(entry.expiresAt) {
		delete(shard.entries, hash)
		return "", false
	}
	entry.expiresAt = time.Now().Add(c.ttl)
	shard.entries[hash] = entry
	return entry.accountID, true
}

func (c *affinityCache) Set(key, accountID string) {
	hash, shardKey := affinityHash(key)
	shard := &c.shards[int(shardKey)%len(c.shards)]
	shard.Lock()
	defer shard.Unlock()
	now := time.Now()
	if len(shard.entries) >= c.limit {
		var oldestKey string
		var oldest time.Time
		for k, entry := range shard.entries {
			if now.After(entry.expiresAt) {
				delete(shard.entries, k)
				continue
			}
			if oldest.IsZero() || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = k, entry.expiresAt
			}
		}
		if len(shard.entries) >= c.limit && oldestKey != "" {
			delete(shard.entries, oldestKey)
		}
	}
	shard.entries[hash] = affinityEntry{accountID: accountID, expiresAt: now.Add(c.ttl)}
}

func (c *affinityCache) Delete(key string) {
	hash, shardKey := affinityHash(key)
	shard := &c.shards[int(shardKey)%len(c.shards)]
	shard.Lock()
	delete(shard.entries, hash)
	shard.Unlock()
}
