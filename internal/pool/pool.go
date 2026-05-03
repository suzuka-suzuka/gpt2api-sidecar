package pool

import (
	"context"
	"errors"
	"sync"
	"time"

	"gpt2api-sidecar/internal/config"
)

var ErrNoAvailable = errors.New("no available account")

type Snapshot struct {
	Name      string
	AuthToken string
	DeviceID  string
	SessionID string
	ProxyURL  string
	Cookies   string
}

type AccountState struct {
	Name               string    `json:"name"`
	Persona            string    `json:"persona,omitempty"`
	NoImageFailures24h int       `json:"no_image_failures_24h,omitempty"`
	Busy               bool      `json:"busy"`
	Disabled           bool      `json:"disabled"`
	CooldownUntil      time.Time `json:"cooldown_until,omitempty"`
	LastUsedAt         time.Time `json:"last_used_at,omitempty"`
}

type account struct {
	snapshot   Snapshot
	busy       bool
	disabled   bool
	lastUsedAt time.Time
}

type Pool struct {
	mu          sync.Mutex
	accounts    []*account
	minInterval time.Duration
	nextIndex   int
	state       StateStore
}

type Lease struct {
	pool     *Pool
	account  *account
	snapshot Snapshot
	once     sync.Once
}

type NoImageFailurePolicy struct {
	AccountName     string
	Persona         string
	Plan            string
	Count           int
	Threshold       int
	Window          time.Duration
	Cooldown        time.Duration
	CooldownUntil   time.Time
	CooldownApplied bool
}

const (
	noImageFailureWindow      = 24 * time.Hour
	freeNoImageThreshold      = 1
	freeNoImageCooldown       = 12 * time.Hour
	paidNoImageThreshold      = 3
	paidNoImageCooldown       = 3 * time.Hour
	personaChatGPTPaid        = "chatgpt-paid"
	personaChatGPTFreeAccount = "chatgpt-freeaccount"
	personaChatGPTNoAuth      = "chatgpt-noauth"
	planPaid                  = "paid"
	planFree                  = "free"
	planUnknownTreatedAsFree  = "unknown_as_free"
)

func New(accounts []config.AccountConfig, minInterval time.Duration) *Pool {
	return NewWithStore(accounts, minInterval, nil)
}

func NewWithStore(accounts []config.AccountConfig, minInterval time.Duration, state StateStore) *Pool {
	if state == nil {
		state = NewMemoryStateStore()
	}
	items := make([]*account, 0, len(accounts))
	for _, cfg := range accounts {
		if cfg.Enabled != nil && !*cfg.Enabled {
			continue
		}
		items = append(items, &account{
			snapshot: Snapshot{
				Name:      cfg.Name,
				AuthToken: cfg.AuthToken,
				DeviceID:  cfg.DeviceID,
				SessionID: cfg.SessionID,
				ProxyURL:  cfg.ProxyURL,
				Cookies:   cfg.Cookies,
			},
		})
	}
	return &Pool{
		accounts:    items,
		minInterval: minInterval,
		state:       state,
	}
}

func (p *Pool) Reload(accounts []config.AccountConfig, minInterval time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	existing := make(map[string]*account, len(p.accounts))
	for _, acc := range p.accounts {
		if acc == nil {
			continue
		}
		existing[acc.snapshot.Name] = acc
	}

	items := make([]*account, 0, len(accounts))
	for _, cfg := range accounts {
		if cfg.Enabled != nil && !*cfg.Enabled {
			continue
		}
		snap := Snapshot{
			Name:      cfg.Name,
			AuthToken: cfg.AuthToken,
			DeviceID:  cfg.DeviceID,
			SessionID: cfg.SessionID,
			ProxyURL:  cfg.ProxyURL,
			Cookies:   cfg.Cookies,
		}
		if acc, ok := existing[cfg.Name]; ok {
			authChanged := acc.snapshot.AuthToken != snap.AuthToken ||
				acc.snapshot.DeviceID != snap.DeviceID ||
				acc.snapshot.SessionID != snap.SessionID ||
				acc.snapshot.ProxyURL != snap.ProxyURL ||
				acc.snapshot.Cookies != snap.Cookies
			acc.snapshot = snap
			if authChanged {
				acc.disabled = false
				_ = p.state.ClearAccount(context.Background(), cfg.Name)
			}
			items = append(items, acc)
			continue
		}
		items = append(items, &account{snapshot: snap})
	}

	p.accounts = items
	p.minInterval = minInterval
	if len(p.accounts) == 0 || p.nextIndex >= len(p.accounts) {
		p.nextIndex = 0
	}
}

func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	backoff := 200 * time.Millisecond
	for {
		now := time.Now()

		p.mu.Lock()
		if len(p.accounts) == 0 {
			p.mu.Unlock()
			return nil, ErrNoAvailable
		}
		for i := 0; i < len(p.accounts); i++ {
			idx := (p.nextIndex + i) % len(p.accounts)
			acc := p.accounts[idx]
			if acc.disabled || acc.busy {
				continue
			}
			cooldownUntil, err := p.state.CooldownUntil(context.Background(), acc.snapshot.Name)
			if err != nil {
				continue
			}
			if !cooldownUntil.IsZero() && now.Before(cooldownUntil) {
				continue
			}
			if !acc.lastUsedAt.IsZero() && now.Sub(acc.lastUsedAt) < p.minInterval {
				continue
			}

			acc.busy = true
			p.nextIndex = (idx + 1) % len(p.accounts)
			snap := acc.snapshot
			p.mu.Unlock()

			return &Lease{
				pool:     p,
				account:  acc,
				snapshot: snap,
			}, nil
		}
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrNoAvailable
			}
			return nil, ctx.Err()
		case <-time.After(backoff):
		}

		if backoff < 2*time.Second {
			backoff += backoff / 2
		}
	}
}

func (l *Lease) Snapshot() Snapshot {
	return l.snapshot
}

func (l *Lease) Release() {
	if l == nil || l.pool == nil || l.account == nil {
		return
	}

	l.once.Do(func() {
		l.pool.mu.Lock()
		defer l.pool.mu.Unlock()
		l.account.busy = false
		l.account.lastUsedAt = time.Now()
	})
}

func (p *Pool) MarkRateLimited(name string, cooldown time.Duration) {
	p.MarkCooldown(name, cooldown)
}

func (p *Pool) MarkCooldown(name string, cooldown time.Duration) {
	if cooldown <= 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	until := time.Now().Add(cooldown)
	for _, acc := range p.accounts {
		if acc.snapshot.Name == name {
			_, _ = p.state.MarkCooldown(context.Background(), name, until)
			return
		}
	}
}

func (p *Pool) MarkPersona(name, persona string) {
	if persona == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, acc := range p.accounts {
		if acc.snapshot.Name == name {
			_ = p.state.SetPersona(context.Background(), name, persona)
			return
		}
	}
}

func (p *Pool) RecordNoImageFailure(name, persona string) NoImageFailurePolicy {
	return p.recordNoImageFailureAt(name, persona, time.Now())
}

func (p *Pool) recordNoImageFailureAt(name, persona string, now time.Time) NoImageFailurePolicy {
	plan, threshold, cooldown := noImagePolicy(persona)
	result := NoImageFailurePolicy{
		AccountName: name,
		Persona:     persona,
		Plan:        plan,
		Threshold:   threshold,
		Window:      noImageFailureWindow,
		Cooldown:    cooldown,
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, acc := range p.accounts {
		if acc.snapshot.Name != name {
			continue
		}
		if persona != "" {
			_ = p.state.SetPersona(context.Background(), name, persona)
		} else if storedPersona, err := p.state.Persona(context.Background(), name); err == nil && storedPersona != "" {
			result.Persona = storedPersona
			plan, threshold, cooldown = noImagePolicy(storedPersona)
			result.Plan = plan
			result.Threshold = threshold
			result.Cooldown = cooldown
		}

		count, err := p.state.RecordNoImageFailure(context.Background(), name, now, noImageFailureWindow)
		if err != nil {
			return result
		}
		result.Count = count
		if result.Count >= threshold && cooldown > 0 {
			until := now.Add(cooldown)
			storedUntil, _ := p.state.MarkCooldown(context.Background(), name, until)
			result.CooldownApplied = true
			result.CooldownUntil = storedUntil
		}
		return result
	}

	return result
}

func noImagePolicy(persona string) (string, int, time.Duration) {
	switch persona {
	case personaChatGPTPaid:
		return planPaid, paidNoImageThreshold, paidNoImageCooldown
	case personaChatGPTFreeAccount, personaChatGPTNoAuth:
		return planFree, freeNoImageThreshold, freeNoImageCooldown
	default:
		return planUnknownTreatedAsFree, freeNoImageThreshold, freeNoImageCooldown
	}
}

func (p *Pool) MarkUnauthorized(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, acc := range p.accounts {
		if acc.snapshot.Name == name {
			acc.disabled = true
			return
		}
	}
}

func (p *Pool) States() []AccountState {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	out := make([]AccountState, 0, len(p.accounts))
	for _, acc := range p.accounts {
		persona, _ := p.state.Persona(context.Background(), acc.snapshot.Name)
		cooldownUntil, _ := p.state.CooldownUntil(context.Background(), acc.snapshot.Name)
		noImageFailures, _ := p.state.NoImageFailureCount(context.Background(), acc.snapshot.Name, now, noImageFailureWindow)
		out = append(out, AccountState{
			Name:               acc.snapshot.Name,
			Persona:            persona,
			NoImageFailures24h: noImageFailures,
			Busy:               acc.busy,
			Disabled:           acc.disabled,
			CooldownUntil:      cooldownUntil,
			LastUsedAt:         acc.lastUsedAt,
		})
	}
	return out
}
