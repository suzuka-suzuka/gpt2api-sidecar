package pool

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisAddr      = "127.0.0.1:6379"
	defaultRedisKeyPrefix = "gpt2api-sidecar"
	redisOpTimeout        = 2 * time.Second
)

type StateStore interface {
	CooldownUntil(ctx context.Context, name string) (time.Time, error)
	MarkCooldown(ctx context.Context, name string, until time.Time) (time.Time, error)
	ClearAccount(ctx context.Context, name string) error
	Persona(ctx context.Context, name string) (string, error)
	SetPersona(ctx context.Context, name, persona string) error
	RecordNoImageFailure(ctx context.Context, name string, at time.Time, window time.Duration) (int, error)
	NoImageFailureCount(ctx context.Context, name string, now time.Time, window time.Duration) (int, error)
}

type RedisOptions struct {
	Addr      string
	Password  string
	DB        int
	KeyPrefix string
}

type memoryAccountState struct {
	cooldownUntil   time.Time
	persona         string
	noImageFailures []time.Time
}

type MemoryStateStore struct {
	mu       sync.Mutex
	accounts map[string]*memoryAccountState
}

func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{accounts: map[string]*memoryAccountState{}}
}

func (s *MemoryStateStore) state(name string) *memoryAccountState {
	st := s.accounts[name]
	if st == nil {
		st = &memoryAccountState{}
		s.accounts[name] = st
	}
	return st
}

func (s *MemoryStateStore) CooldownUntil(ctx context.Context, name string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	until := s.state(name).cooldownUntil
	if !until.IsZero() && !time.Now().Before(until) {
		s.state(name).cooldownUntil = time.Time{}
		return time.Time{}, nil
	}
	return until, nil
}

func (s *MemoryStateStore) MarkCooldown(ctx context.Context, name string, until time.Time) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.state(name)
	if until.After(st.cooldownUntil) {
		st.cooldownUntil = until
	}
	return st.cooldownUntil, nil
}

func (s *MemoryStateStore) ClearAccount(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accounts, name)
	return nil
}

func (s *MemoryStateStore) Persona(ctx context.Context, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state(name).persona, nil
}

func (s *MemoryStateStore) SetPersona(ctx context.Context, name, persona string) error {
	if persona == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state(name).persona = persona
	return nil
}

func (s *MemoryStateStore) RecordNoImageFailure(ctx context.Context, name string, at time.Time, window time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.state(name)
	cutoff := at.Add(-window)
	filtered := st.noImageFailures[:0]
	for _, item := range st.noImageFailures {
		if item.After(cutoff) {
			filtered = append(filtered, item)
		}
	}
	st.noImageFailures = append(filtered, at)
	return len(st.noImageFailures), nil
}

func (s *MemoryStateStore) NoImageFailureCount(ctx context.Context, name string, now time.Time, window time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.state(name)
	cutoff := now.Add(-window)
	filtered := st.noImageFailures[:0]
	for _, item := range st.noImageFailures {
		if item.After(cutoff) {
			filtered = append(filtered, item)
		}
	}
	st.noImageFailures = filtered
	return len(st.noImageFailures), nil
}

type RedisStateStore struct {
	client    *redis.Client
	keyPrefix string
}

func NewRedisStateStore(ctx context.Context, opt RedisOptions) (*RedisStateStore, error) {
	if opt.Addr == "" {
		opt.Addr = defaultRedisAddr
	}
	if opt.KeyPrefix == "" {
		opt.KeyPrefix = defaultRedisKeyPrefix
	}
	client := redis.NewClient(&redis.Options{
		Addr:     opt.Addr,
		Password: opt.Password,
		DB:       opt.DB,
		Protocol: 2,
	})
	opCtx, cancel := withRedisTimeout(ctx)
	defer cancel()
	if err := client.Ping(opCtx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return &RedisStateStore{client: client, keyPrefix: opt.KeyPrefix}, nil
}

func DefaultRedisOptions() RedisOptions {
	return RedisOptions{
		Addr:      defaultRedisAddr,
		KeyPrefix: defaultRedisKeyPrefix,
	}
}

func (s *RedisStateStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *RedisStateStore) CooldownUntil(ctx context.Context, name string) (time.Time, error) {
	opCtx, cancel := withRedisTimeout(ctx)
	defer cancel()

	raw, err := s.client.Get(opCtx, s.cooldownKey(name)).Result()
	if errors.Is(err, redis.Nil) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	until, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		_ = s.client.Del(opCtx, s.cooldownKey(name)).Err()
		return time.Time{}, nil
	}
	if !time.Now().Before(until) {
		_ = s.client.Del(opCtx, s.cooldownKey(name)).Err()
		return time.Time{}, nil
	}
	return until, nil
}

func (s *RedisStateStore) MarkCooldown(ctx context.Context, name string, until time.Time) (time.Time, error) {
	if until.IsZero() {
		return time.Time{}, nil
	}

	existing, err := s.CooldownUntil(ctx, name)
	if err != nil {
		return time.Time{}, err
	}
	if existing.After(until) {
		return existing, nil
	}

	ttl := time.Until(until)
	if ttl <= 0 {
		return time.Time{}, nil
	}
	opCtx, cancel := withRedisTimeout(ctx)
	defer cancel()
	if err := s.client.Set(opCtx, s.cooldownKey(name), until.Format(time.RFC3339Nano), ttl).Err(); err != nil {
		return time.Time{}, err
	}
	return until, nil
}

func (s *RedisStateStore) ClearAccount(ctx context.Context, name string) error {
	opCtx, cancel := withRedisTimeout(ctx)
	defer cancel()
	return s.client.Del(opCtx, s.cooldownKey(name), s.personaKey(name), s.noImageKey(name)).Err()
}

func (s *RedisStateStore) Persona(ctx context.Context, name string) (string, error) {
	opCtx, cancel := withRedisTimeout(ctx)
	defer cancel()

	persona, err := s.client.Get(opCtx, s.personaKey(name)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return persona, err
}

func (s *RedisStateStore) SetPersona(ctx context.Context, name, persona string) error {
	if persona == "" {
		return nil
	}
	opCtx, cancel := withRedisTimeout(ctx)
	defer cancel()
	return s.client.Set(opCtx, s.personaKey(name), persona, 0).Err()
}

func (s *RedisStateStore) RecordNoImageFailure(ctx context.Context, name string, at time.Time, window time.Duration) (int, error) {
	opCtx, cancel := withRedisTimeout(ctx)
	defer cancel()

	key := s.noImageKey(name)
	cutoffScore := strconv.FormatInt(at.Add(-window).UnixMilli(), 10)
	member := fmt.Sprintf("%d:%s", at.UnixNano(), uuid.NewString())
	pipe := s.client.TxPipeline()
	pipe.ZRemRangeByScore(opCtx, key, "-inf", cutoffScore)
	pipe.ZAdd(opCtx, key, redis.Z{Score: float64(at.UnixMilli()), Member: member})
	countCmd := pipe.ZCard(opCtx, key)
	pipe.Expire(opCtx, key, window+time.Hour)
	if _, err := pipe.Exec(opCtx); err != nil {
		return 0, err
	}
	return int(countCmd.Val()), nil
}

func (s *RedisStateStore) NoImageFailureCount(ctx context.Context, name string, now time.Time, window time.Duration) (int, error) {
	opCtx, cancel := withRedisTimeout(ctx)
	defer cancel()

	key := s.noImageKey(name)
	cutoffScore := strconv.FormatInt(now.Add(-window).UnixMilli(), 10)
	pipe := s.client.TxPipeline()
	pipe.ZRemRangeByScore(opCtx, key, "-inf", cutoffScore)
	countCmd := pipe.ZCard(opCtx, key)
	pipe.Expire(opCtx, key, window+time.Hour)
	if _, err := pipe.Exec(opCtx); err != nil {
		return 0, err
	}
	return int(countCmd.Val()), nil
}

func (s *RedisStateStore) cooldownKey(name string) string {
	return s.key("cooldown_until", name)
}

func (s *RedisStateStore) personaKey(name string) string {
	return s.key("persona", name)
}

func (s *RedisStateStore) noImageKey(name string) string {
	return s.key("no_image_failures", name)
}

func (s *RedisStateStore) key(kind, name string) string {
	return fmt.Sprintf("%s:account:%s:%s", s.keyPrefix, name, kind)
}

func withRedisTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, redisOpTimeout)
}
