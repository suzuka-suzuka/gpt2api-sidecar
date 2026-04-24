package queue

import (
	"context"
	"errors"
	"sync"
)

var ErrQueueFull = errors.New("queue full")

type Stats struct {
	Limit      int `json:"limit"`
	Active     int `json:"active"`
	Pending    int `json:"pending"`
	MaxPending int `json:"max_pending"`
}

type waiter struct {
	ready    chan struct{}
	granted  bool
	canceled bool
}

type Gate struct {
	mu         sync.Mutex
	limit      int
	maxPending int
	active     int
	pending    int
	waiters    []*waiter
}

type Lease struct {
	gate *Gate
	once sync.Once
}

func New(limit, maxPending int) *Gate {
	if limit <= 0 {
		limit = 1
	}
	return &Gate{
		limit:      limit,
		maxPending: maxPending,
	}
}

func (g *Gate) Acquire(ctx context.Context) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	w := &waiter{ready: make(chan struct{})}

	g.mu.Lock()
	if g.active < g.limit && g.pending == 0 {
		g.active++
		g.mu.Unlock()
		return &Lease{gate: g}, nil
	}
	if g.maxPending > 0 && g.pending >= g.maxPending {
		g.mu.Unlock()
		return nil, ErrQueueFull
	}
	g.waiters = append(g.waiters, w)
	g.pending++
	g.mu.Unlock()

	select {
	case <-w.ready:
		return &Lease{gate: g}, nil
	case <-ctx.Done():
		g.mu.Lock()
		if w.granted {
			g.mu.Unlock()
			return &Lease{gate: g}, nil
		}
		if !w.canceled {
			w.canceled = true
			g.pending--
		}
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (l *Lease) Release() {
	if l == nil || l.gate == nil {
		return
	}

	l.once.Do(func() {
		l.gate.release()
	})
}

func (g *Gate) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()

	return Stats{
		Limit:      g.limit,
		Active:     g.active,
		Pending:    g.pending,
		MaxPending: g.maxPending,
	}
}

func (g *Gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()

	for len(g.waiters) > 0 {
		w := g.waiters[0]
		g.waiters = g.waiters[1:]
		if w == nil || w.canceled || w.granted {
			continue
		}

		w.granted = true
		g.pending--
		close(w.ready)
		return
	}

	if g.active > 0 {
		g.active--
	}
}
