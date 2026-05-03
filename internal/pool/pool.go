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
	Name          string    `json:"name"`
	Busy          bool      `json:"busy"`
	Disabled      bool      `json:"disabled"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	LastUsedAt    time.Time `json:"last_used_at,omitempty"`
}

type account struct {
	snapshot      Snapshot
	busy          bool
	disabled      bool
	lastUsedAt    time.Time
	cooldownUntil time.Time
}

type Pool struct {
	mu          sync.Mutex
	accounts    []*account
	minInterval time.Duration
	nextIndex   int
}

type Lease struct {
	pool     *Pool
	account  *account
	snapshot Snapshot
	once     sync.Once
}

func New(accounts []config.AccountConfig, minInterval time.Duration) *Pool {
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
				acc.cooldownUntil = time.Time{}
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
			if !acc.cooldownUntil.IsZero() && now.Before(acc.cooldownUntil) {
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
			if until.After(acc.cooldownUntil) {
				acc.cooldownUntil = until
			}
			return
		}
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

	out := make([]AccountState, 0, len(p.accounts))
	for _, acc := range p.accounts {
		out = append(out, AccountState{
			Name:          acc.snapshot.Name,
			Busy:          acc.busy,
			Disabled:      acc.disabled,
			CooldownUntil: acc.cooldownUntil,
			LastUsedAt:    acc.lastUsedAt,
		})
	}
	return out
}
