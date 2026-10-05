package rabbitmq

import (
	"testing"
	"time"
)

func TestRetryPolicyDelay(t *testing.T) {
	p := RetryPolicy{BaseDelay: 2 * time.Second, MaxDelay: 10 * time.Second, MaxAttempts: 6}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := p.Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %s, want %s", i+1, got, w)
		}
	}
}

func TestRetryPolicyDelaysAreUnique(t *testing.T) {
	p := RetryPolicy{BaseDelay: time.Second, MaxDelay: 4 * time.Second, MaxAttempts: 10}
	got := p.delays()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("delays() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delays() = %v, want %v", got, want)
		}
	}
	if q := RetryQueue(1500 * time.Millisecond); q != "documents.retry.1500ms" {
		t.Fatalf("RetryQueue = %q", q)
	}
}
