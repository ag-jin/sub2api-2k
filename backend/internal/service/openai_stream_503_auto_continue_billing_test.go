//go:build unit

package service

// 09 — 503 续跑计费不变量门禁。
//
// 裁定 #1：每个上游尝试的 attempt-local usage 恰好计入一次；
// ForwardResult.Usage = Σ(所有尝试的 local usage)。07 已在 Forward 内完成聚合，
// handler 仍只调用一次 RecordUsage，因此这里从 RecordUsage 公开入口断言
// “一次调用 → 恰好一行 usage log，且行上 token 就是聚合值”。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIStream503AutoContinueBilling_SingleRecordUsageWritesOneRow(t *testing.T) {
	// 复用既有 fake：CreateBestEffort / Create 的调用次数分别计数。
	usageRepo := &openAIRecordUsageBestEffortLogRepoStub{}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, subRepo, nil)

	// 聚合值来自两次尝试的真实求和，而不是在这里重算：
	//   attempt1（503 中断）: input 7, output 3
	//   attempt2（续跑成功）: input 5, output 6
	//   Σ                   : input 12, output 9
	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "resp_503_auto_continue_aggregated",
			Model:     "gpt-5.1",
			Stream:    true,
			Usage:     OpenAIUsage{InputTokens: 12, OutputTokens: 9},
			Duration:  time.Second,
		},
		APIKey:  &APIKey{ID: 9001},
		User:    &User{ID: 9002},
		Account: &Account{ID: 9003},
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.createCalls+usageRepo.bestEffortCalls,
		"one RecordUsage call must write exactly one usage log row (create=%d, bestEffort=%d)",
		usageRepo.createCalls, usageRepo.bestEffortCalls)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, 12, usageRepo.lastLog.InputTokens, "usage log input tokens must be the Σ of both attempts")
	require.Equal(t, 9, usageRepo.lastLog.OutputTokens, "usage log output tokens must be the Σ of both attempts")
	require.Equal(t, 21, usageRepo.lastLog.TotalTokens())
}
