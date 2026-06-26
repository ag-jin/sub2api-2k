package kiro

import "testing"

func TestIsTransientStatus(t *testing.T) {
	transient := []int{408, 429, 500, 502, 503, 504, 599}
	for _, s := range transient {
		if !IsTransientStatus(s) {
			t.Errorf("status %d should be transient", s)
		}
	}
	notTransient := []int{200, 201, 400, 401, 403, 404, 422}
	for _, s := range notTransient {
		if IsTransientStatus(s) {
			t.Errorf("status %d should NOT be transient", s)
		}
	}
}

// TestIsInPlaceRetryStatus verifies 429 IS in-place retryable (aligned with
// Kiro IDE's AdaptiveRetryStrategy, which retries ThrottlingException/429 on the
// same credential), alongside 408/5xx. Credential failover only happens after
// the in-place retries are exhausted.
func TestIsInPlaceRetryStatus(t *testing.T) {
	inPlace := []int{408, 429, 500, 502, 503, 504, 599}
	for _, s := range inPlace {
		if !IsInPlaceRetryStatus(s) {
			t.Errorf("status %d should be in-place retryable", s)
		}
	}
	notRetry := []int{200, 400, 401, 403, 404}
	for _, s := range notRetry {
		if IsInPlaceRetryStatus(s) {
			t.Errorf("status %d should NOT be in-place retryable", s)
		}
	}
}

// TestIsThrottleStatus verifies only 429 selects the slower throttle backoff.
func TestIsThrottleStatus(t *testing.T) {
	if !IsThrottleStatus(429) {
		t.Error("429 should be a throttle status")
	}
	for _, s := range []int{408, 500, 502, 503, 504, 200, 400} {
		if IsThrottleStatus(s) {
			t.Errorf("status %d should NOT be a throttle status", s)
		}
	}
}

func TestRetryDelay_Backoff(t *testing.T) {
	// delays should be non-decreasing in expectation and capped at ~2.5s (2000+25% jitter)
	for attempt := 0; attempt < 10; attempt++ {
		d := RetryDelay(attempt)
		ms := d.Milliseconds()
		if ms < 1 {
			t.Errorf("attempt %d: delay too small: %dms", attempt, ms)
		}
		if ms > 2600 {
			t.Errorf("attempt %d: delay exceeds cap+jitter: %dms", attempt, ms)
		}
	}
	// attempt 0 should be around base (200ms + up to 25% jitter = 200..250)
	d0 := RetryDelay(0).Milliseconds()
	if d0 < 200 || d0 > 300 {
		t.Errorf("attempt 0 delay out of expected range: %dms", d0)
	}
}

// TestThrottleRetryDelay_Backoff verifies the 429-specific backoff aligns with
// Kiro IDE: base 500ms×5=2500ms at attempt 0, exponential growth, capped at
// 20s (+<=25% jitter), always slower than the fast RetryDelay.
func TestThrottleRetryDelay_Backoff(t *testing.T) {
	// attempt 0: 2500ms base + up to 25% jitter = 2500..3125
	d0 := ThrottleRetryDelay(0).Milliseconds()
	if d0 < 2500 || d0 > 3125 {
		t.Errorf("throttle attempt 0 delay out of expected range: %dms", d0)
	}
	for attempt := 0; attempt < 10; attempt++ {
		d := ThrottleRetryDelay(attempt).Milliseconds()
		if d < 2500 {
			t.Errorf("attempt %d: throttle delay below base: %dms", attempt, d)
		}
		// cap 20000 + 25% jitter = 25000
		if d > 25000 {
			t.Errorf("attempt %d: throttle delay exceeds cap+jitter: %dms", attempt, d)
		}
		// throttle backoff must never be faster than the fast transient backoff
		if d < RetryDelay(attempt).Milliseconds() {
			t.Errorf("attempt %d: throttle delay %dms must be >= fast delay", attempt, d)
		}
	}
}
