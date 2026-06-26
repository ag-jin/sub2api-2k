package kiro

import "testing"

func TestIsMonthlyRequestLimit(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`, true},
		{`{"error":{"reason":"MONTHLY_REQUEST_COUNT"}}`, true},
		{`{"message":"nope","reason":"DAILY_REQUEST_COUNT"}`, false},
		{`{"some":"MONTHLY_REQUEST_COUNT in text"}`, true}, // substring match like kiro.rs
		{`{"unrelated":"error"}`, false},
		{``, false},
	}
	for i, c := range cases {
		if got := IsMonthlyRequestLimit([]byte(c.body)); got != c.want {
			t.Errorf("case %d: IsMonthlyRequestLimit(%q)=%v want %v", i, c.body, got, c.want)
		}
	}
}

func TestIsBearerTokenInvalid(t *testing.T) {
	if !IsBearerTokenInvalid([]byte("The bearer token included in the request is invalid")) {
		t.Error("should detect invalid bearer token")
	}
	if IsBearerTokenInvalid([]byte("unrelated error")) {
		t.Error("should not match unrelated error")
	}
}

func TestSanitizeError(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		mustNot []string // substrings that must be gone
		must    []string // substrings that must remain
	}{
		{
			name:    "bearer token",
			in:      "auth failed: Bearer eyJabc.def.ghi please retry",
			mustNot: []string{"eyJabc.def.ghi"},
			must:    []string{"auth failed", "[REDACTED]"},
		},
		{
			name:    "sk key",
			in:      `invalid key sk-ABCdef123_-456 rejected`,
			mustNot: []string{"sk-ABCdef123_-456"},
			must:    []string{"invalid key", "[REDACTED]"},
		},
		{
			name:    "embedded accessToken",
			in:      `{"accessToken":"secret-value-123","reason":"x"}`,
			mustNot: []string{"secret-value-123"},
			must:    []string{"reason", "[REDACTED]"},
		},
		{
			name:    "embedded refreshToken",
			in:      `{"refreshToken":"rt-secret-xyz"}`,
			mustNot: []string{"rt-secret-xyz"},
			must:    []string{"[REDACTED]"},
		},
		{
			name:    "clientSecret",
			in:      `{"clientSecret":"cs-hidden"}`,
			mustNot: []string{"cs-hidden"},
			must:    []string{"[REDACTED]"},
		},
		{
			name:    "raw JWT",
			in:      `token is eyJhbGciOi.eyJzdWIi.SflKxwRJ here`,
			mustNot: []string{"eyJhbGciOi.eyJzdWIi.SflKxwRJ"},
			must:    []string{"token is", "[REDACTED]"},
		},
		{
			name: "no secrets untouched",
			in:   `ThrottlingException: rate exceeded`,
			must: []string{"ThrottlingException: rate exceeded"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitizeError(c.in)
			for _, mn := range c.mustNot {
				if contains(got, mn) {
					t.Errorf("expected %q to be redacted, got %q", mn, got)
				}
			}
			for _, m := range c.must {
				if !contains(got, m) {
					t.Errorf("expected %q to remain, got %q", m, got)
				}
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestIsRateLimitError(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, ``, true},
		{200, `{"__type":"ThrottlingException","message":"rate exceeded"}`, true},
		{200, `{"__type":"ServiceQuotaExceededException"}`, true},
		{403, `{"message":"some other error"}`, false},
		{200, `throttlingexception lowercased`, true}, // case-insensitive
		{500, `internal`, false},
	}
	for i, c := range cases {
		if got := IsRateLimitError(c.status, []byte(c.body)); got != c.want {
			t.Errorf("case %d: IsRateLimitError(%d,%q)=%v want %v", i, c.status, c.body, got, c.want)
		}
	}
}

func TestIsAccountSuspended(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{403, `{"__type":"AccessDeniedException","message":"TemporarilySuspended"}`, true},
		{403, `{"message":"account suspended"}`, true},
		{403, `{"__type":"AccessDeniedException","message":"no profile"}`, false}, // AccessDenied but not suspend
		{429, `ThrottlingException`, false},
		{403, `ACCESSDENIEDEXCEPTION ... SUSPEND`, true}, // case-insensitive
	}
	for i, c := range cases {
		if got := IsAccountSuspended(c.status, []byte(c.body)); got != c.want {
			t.Errorf("case %d: IsAccountSuspended(%d,%q)=%v want %v", i, c.status, c.body, got, c.want)
		}
	}
}
