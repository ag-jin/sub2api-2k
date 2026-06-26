package handler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResponsesPreFirstTokenFailoverBudgetExhaustedStopsBeforeRetryOrSwitch(t *testing.T) {
	fs := NewFailoverState(3, false)
	fs.preFirstTokenRecoveryStartedAt = time.Now().Add(-3 * time.Second)
	mock := &mockTempUnscheduler{}
	failoverErr := newTestFailoverErr(429, true, false)

	start := time.Now()
	action := handleResponsesPreFirstTokenFailover(context.Background(), fs, mock, 100, "anthropic", failoverErr)

	require.Equal(t, FailoverExhausted, action)
	require.Less(t, time.Since(start), 100*time.Millisecond)
	require.Equal(t, failoverErr, fs.LastFailoverErr)
	require.Zero(t, fs.SameAccountRetryCount[100])
	require.Zero(t, fs.SwitchCount)
	require.NotContains(t, fs.FailedAccountIDs, int64(100))
	require.Empty(t, mock.calls)
}

func TestResponsesPreFirstTokenSelectionExhaustedDoesNotSleepPastRemainingBudget(t *testing.T) {
	fs := NewFailoverState(3, false)
	fs.LastFailoverErr = newTestFailoverErr(503, false, false)
	fs.FailedAccountIDs[100] = struct{}{}
	fs.preFirstTokenRecoveryStartedAt = time.Now().Add(-1900 * time.Millisecond)

	start := time.Now()
	action := handleResponsesPreFirstTokenSelectionExhausted(context.Background(), fs)

	require.Equal(t, FailoverExhausted, action)
	require.Less(t, time.Since(start), 100*time.Millisecond)
	require.Contains(t, fs.FailedAccountIDs, int64(100))
}
