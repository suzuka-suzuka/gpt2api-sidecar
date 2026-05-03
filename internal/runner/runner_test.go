package runner

import (
	"errors"
	"testing"
	"time"

	"gpt2api-sidecar/internal/pool"
)

func TestRetryableNoImageTask(t *testing.T) {
	if !retryableNoImageTask(ErrNoImageTask, errors.New("no image task")) {
		t.Fatal("expected no_image_task to be retryable")
	}
}

func TestRetryableNoImageTaskIncludesCooldownForNoImageTask(t *testing.T) {
	err := &noImageFailureCooldownError{
		reason: "no_image_task",
		policy: pool.NoImageFailurePolicy{
			Count:    1,
			Window:   24 * time.Hour,
			Plan:     "free",
			Cooldown: 12 * time.Hour,
		},
	}

	if !retryableNoImageTask(ErrRateLimited, err) {
		t.Fatal("expected no_image_task cooldown error to be retryable")
	}
}

func TestRetryableNoImageTaskSkipsOtherCooldownReasons(t *testing.T) {
	err := &noImageFailureCooldownError{
		reason: "poll_timeout",
		policy: pool.NoImageFailurePolicy{
			Count:    1,
			Window:   24 * time.Hour,
			Plan:     "free",
			Cooldown: 12 * time.Hour,
		},
	}

	if retryableNoImageTask(ErrRateLimited, err) {
		t.Fatal("did not expect poll_timeout cooldown error to be retryable")
	}
}

func TestRetryableNoImageTaskSkipsPlainRateLimit(t *testing.T) {
	if retryableNoImageTask(ErrRateLimited, errors.New("upstream rate limited")) {
		t.Fatal("did not expect plain rate limit to be retryable")
	}
}
