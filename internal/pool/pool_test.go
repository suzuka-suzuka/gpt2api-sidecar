package pool

import (
	"testing"
	"time"

	"gpt2api-sidecar/internal/config"
)

func TestRecordNoImageFailureFreeCoolsOnFirstFailure(t *testing.T) {
	p := New([]config.AccountConfig{{Name: "free", AuthToken: "token"}}, 0)
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	result := p.recordNoImageFailureAt("free", personaChatGPTFreeAccount, now)

	if !result.CooldownApplied {
		t.Fatal("expected cooldown on first free-account no-image failure")
	}
	if result.Count != 1 {
		t.Fatalf("count = %d, want 1", result.Count)
	}
	if result.Threshold != freeNoImageThreshold {
		t.Fatalf("threshold = %d, want %d", result.Threshold, freeNoImageThreshold)
	}
	if result.Cooldown != freeNoImageCooldown {
		t.Fatalf("cooldown = %s, want %s", result.Cooldown, freeNoImageCooldown)
	}
}

func TestRecordNoImageFailurePaidCoolsOnThirdFailure(t *testing.T) {
	p := New([]config.AccountConfig{{Name: "paid", AuthToken: "token"}}, 0)
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	first := p.recordNoImageFailureAt("paid", personaChatGPTPaid, now)
	second := p.recordNoImageFailureAt("paid", personaChatGPTPaid, now.Add(time.Hour))
	third := p.recordNoImageFailureAt("paid", personaChatGPTPaid, now.Add(2*time.Hour))

	if first.CooldownApplied {
		t.Fatal("did not expect cooldown on first paid-account no-image failure")
	}
	if second.CooldownApplied {
		t.Fatal("did not expect cooldown on second paid-account no-image failure")
	}
	if !third.CooldownApplied {
		t.Fatal("expected cooldown on third paid-account no-image failure")
	}
	if third.Count != 3 {
		t.Fatalf("count = %d, want 3", third.Count)
	}
	if third.Threshold != paidNoImageThreshold {
		t.Fatalf("threshold = %d, want %d", third.Threshold, paidNoImageThreshold)
	}
	if third.Cooldown != paidNoImageCooldown {
		t.Fatalf("cooldown = %s, want %s", third.Cooldown, paidNoImageCooldown)
	}
}

func TestRecordNoImageFailureDropsOldFailures(t *testing.T) {
	p := New([]config.AccountConfig{{Name: "paid", AuthToken: "token"}}, 0)
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	p.recordNoImageFailureAt("paid", personaChatGPTPaid, now.Add(-25*time.Hour))
	second := p.recordNoImageFailureAt("paid", personaChatGPTPaid, now)
	third := p.recordNoImageFailureAt("paid", personaChatGPTPaid, now.Add(time.Hour))

	if second.Count != 1 {
		t.Fatalf("second count = %d, want 1 after old failure was dropped", second.Count)
	}
	if third.Count != 2 {
		t.Fatalf("third count = %d, want 2", third.Count)
	}
	if third.CooldownApplied {
		t.Fatal("did not expect cooldown before three paid-account failures within 24h")
	}
}
