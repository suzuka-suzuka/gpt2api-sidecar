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

func TestImageTaskNotStartedWhenNoUsableRefsOrTaskID(t *testing.T) {
	if !imageTaskNotStarted(nil, "") {
		t.Fatal("expected missing usable refs and task id to be treated as not started")
	}
}

func TestImageTaskStartedWhenTaskIDExists(t *testing.T) {
	if imageTaskNotStarted(nil, "task_123") {
		t.Fatal("expected task id to keep polling even without immediate refs")
	}
}

func TestImageTaskStartedWhenUsableRefsExist(t *testing.T) {
	if imageTaskNotStarted([]string{"sed:final_ref"}, "") {
		t.Fatal("expected usable refs to be treated as started")
	}
}
