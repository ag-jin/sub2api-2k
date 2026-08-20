package opencode

import (
	"encoding/json"
	"errors"
	"time"
)

const (
	DefaultBaseURL = "https://opencode.ai/zen/go/v1"
	UsagePath      = "/usage"
)

// UsageStatus is the upstream status for one OpenCode quota window.
type UsageStatus string

const (
	UsageStatusOK          UsageStatus = "ok"
	UsageStatusRateLimited UsageStatus = "rate-limited"
)

// UsageWindow is the normalized OpenCode quota window returned to the service.
type UsageWindow struct {
	Status   UsageStatus `json:"status"`
	Percent  float64     `json:"percent"`
	ResetsAt *time.Time  `json:"resets_at,omitempty"`
}

func (w *UsageWindow) UnmarshalJSON(data []byte) error {
	var raw struct {
		Status   UsageStatus `json:"status"`
		Percent  float64     `json:"percent"`
		ResetsAt string      `json:"resetsAt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("invalid usage window")
	}
	if raw.Percent < 0 {
		return errors.New("invalid usage percent")
	}
	var resetAt *time.Time
	if raw.ResetsAt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw.ResetsAt)
		if err != nil {
			return errors.New("invalid usage reset time")
		}
		resetAt = &parsed
	}
	w.Status = raw.Status
	w.Percent = raw.Percent
	w.ResetsAt = resetAt
	return nil
}

// UsageSnapshot is the normalized response from GET {base}/usage.
type UsageSnapshot struct {
	Rolling UsageWindow `json:"rolling"`
	Weekly  UsageWindow `json:"weekly"`
	Monthly UsageWindow `json:"monthly"`
}

type usageResponse struct {
	Usage *UsageSnapshot `json:"usage"`
}

// UsageError is a stable, credential-safe usage failure category.
type UsageError struct {
	Code       string
	HTTPStatus int
}

func (e *UsageError) Error() string {
	if e == nil || e.Code == "" {
		return "opencode usage error"
	}
	return "opencode usage error: " + e.Code
}

func NewUsageError(code string) error {
	return &UsageError{Code: code}
}

// ParseUsageResponse parses an upstream usage envelope without exposing body data.
func ParseUsageResponse(body []byte) (*UsageSnapshot, error) {
	var parsed usageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("invalid usage response")
	}
	if parsed.Usage == nil {
		return nil, errors.New("usage object is required")
	}
	if parsed.Usage.Rolling.Status == "" || parsed.Usage.Weekly.Status == "" || parsed.Usage.Monthly.Status == "" {
		return nil, errors.New("usage windows are required")
	}
	return parsed.Usage, nil
}
