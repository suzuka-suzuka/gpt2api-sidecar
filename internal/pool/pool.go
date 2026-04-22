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

func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	if len(p.accounts) == 0 {
		return nil, ErrNoAvailable
	}

	backoff := 200 * time.Millisecond
	for {
		now := time.Now()

		p.mu.Lock()
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
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, acc := range p.accounts {
		if acc.snapshot.Name == name {
			acc.cooldownUntil = time.Now().Add(cooldown)
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
