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

type PoolConfig struct {
	Dir                string
	Surface            string
	ReloadInterval     time.Duration
	RefreshConcurrency int
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
	refreshMu     sync.Mutex
	inflight      atomic.Int64
}

func (a *account) available(now time.Time) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.disabled && !now.Before(a.cooldownUntil) && a.credential != nil
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
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Disabled      bool      `json:"disabled,omitempty"`
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
	rebuildCh     chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
	wg            sync.WaitGroup
}

type Lease struct {
	pool    *Pool
	account *account
	once    sync.Once
}

func (l *Lease) Session() Session {
	cred, _, _ := l.account.snapshot()
	if cred == nil {
		return Session{}
	}
	return cred.session()
}
func (l *Lease) AccountID() string { return l.account.id }
func (l *Lease) AgentID() string   { return l.account.agentID }
func (l *Lease) SessionID() string { return l.account.sessionID }
func (l *Lease) Release() {
	if l == nil || l.account == nil {
		return
	}
	l.once.Do(func() { l.account.inflight.Add(-1) })
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
		refreshSem: make(chan struct{}, cfg.RefreshConcurrency), rebuildCh: make(chan struct{}, 1), closed: make(chan struct{}),
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

func (p *Pool) Acquire(ctx context.Context, affinity, model string, exclude map[string]struct{}) (*Lease, error) {
	cacheKey := modelAffinityKey(affinity, model)
	if affinity != "" {
		if id, ok := p.affinity.Get(cacheKey); ok {
			if lease, err := p.acquireID(ctx, id, model, exclude); err == nil {
				return lease, nil
			}
			p.affinity.Delete(cacheKey)
		}
	}
	active := p.schedulingSnapshot(model)
	if len(active) == 0 {
		p.rebuildActive()
		active = p.schedulingSnapshot(model)
		if len(active) == 0 {
			if model != "" && !p.HasModel(model) {
				return nil, &ModelUnavailableError{Model: model}
			}
			return nil, p.unavailable()
		}
	}
	start := int(p.cursor.Add(1)-1) % len(active)
	for i := 0; i < len(active); i++ {
		a := active[(start+i)%len(active)]
		if _, skipped := exclude[a.id]; skipped || !a.available(time.Now()) || !a.supportsModel(model) {
			continue
		}
		if err := p.ensureFresh(ctx, a, false); err != nil {
			continue
		}
		a.inflight.Add(1)
		if affinity != "" {
			p.affinity.Set(cacheKey, a.id)
		}
		return &Lease{pool: p, account: a}, nil
	}
	if model != "" && !p.HasModel(model) {
		return nil, &ModelUnavailableError{Model: model}
	}
	return nil, p.unavailable()
}

func (p *Pool) schedulingSnapshot(model string) []*account {
	if model == "" {
		return p.active.Load().([]*account)
	}
	return p.activeByModel.Load().(map[string][]*account)[model]
}

func (p *Pool) AcquireAccount(ctx context.Context, id string) (*Lease, error) {
	return p.acquireID(ctx, id, "", nil)
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
	if err := p.ensureFresh(ctx, a, false); err != nil {
		return nil, err
	}
	a.inflight.Add(1)
	return &Lease{pool: p, account: a}, nil
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
	if err := p.ensureFresh(ctx, a, false); err != nil {
		return nil, err
	}
	a.inflight.Add(1)
	return &Lease{pool: p, account: a}, nil
}

func (p *Pool) Bind(affinity, model, accountID string) {
	if affinity != "" && accountID != "" {
		p.affinity.Set(modelAffinityKey(affinity, model), accountID)
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
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
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

func (p *Pool) MarkCooldown(accountID, reason string, duration time.Duration) {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return
	}
	a.mu.Lock()
	until := time.Now().Add(duration)
	if until.After(a.cooldownUntil) {
		a.cooldownUntil, a.cooldownCause = until, reason
	}
	a.mu.Unlock()
	p.mu.Lock()
	p.states[accountID] = accountState{CooldownUntil: until, Reason: reason}
	p.mu.Unlock()
	p.requestRebuild()
	_ = p.persistState()
	slog.Warn("credential account cooling", "account", accountID, "reason", reason, "until", until.UTC().Format(time.RFC3339))
}

func (p *Pool) Disable(accountID, reason string) {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return
	}
	a.mu.Lock()
	a.disabled, a.disableReason = true, reason
	a.mu.Unlock()
	p.mu.Lock()
	p.states[accountID] = accountState{Disabled: true, Reason: reason}
	p.mu.Unlock()
	p.requestRebuild()
	_ = p.persistState()
	slog.Warn("credential account disabled", "account", accountID, "reason", reason)
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

func (p *Pool) ensureFresh(ctx context.Context, a *account, force bool) error {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
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
	for id, cred := range parsed {
		if existing := p.accounts[id]; existing != nil {
			existing.mu.Lock()
			changed := existing.credential == nil || existing.credential.Path != cred.Path || existing.credential.AccessToken != cred.AccessToken || existing.credential.RefreshToken != cred.RefreshToken
			existing.credential = cred
			if changed {
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
		if state, ok := p.states[id]; ok {
			if state.Disabled {
				a.disabled, a.disableReason = true, state.Reason
			} else if time.Now().Before(state.CooldownUntil) {
				a.cooldownUntil, a.cooldownCause = state.CooldownUntil, state.Reason
			}
		}
		p.accounts[id] = a
	}
	for id, a := range p.accounts {
		if _, ok := parsed[id]; !ok {
			a.mu.Lock()
			a.disabled, a.disableReason = true, "removed"
			a.mu.Unlock()
			delete(p.accounts, id)
		}
	}
	p.files = newFiles
	count := len(p.accounts)
	p.mu.Unlock()
	p.rebuildActive()
	slog.Info("credential pool loaded", "accounts", count)
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
