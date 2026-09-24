package service

import (
	"context"
	"time"
)

// M10（吸收自 workbuddy-manager「自动任务与积分记录」）：
// 定时任务（保活/签到/成长链）的执行结果落账号 extra，供面板呈现。
//
// 存储口径与积分流水一致：Account.Extra 单键环形（最新在前，上限 10 条），
// 不新建表。读取端：UsageInfo.TaskRuns（随余额查询返回）。
//
// 并发边界：读-改-写非原子，与积分流水同款「观测语义可容忍极小竞争」；
// 且各任务调度器之间本就串行（每分钟 tick 单实例）。

const (
	codeBuddyTaskRunsKey        = "codebuddy_task_runs"
	codeBuddyTaskRunsMaxEntries = 10
)

// AppendCodeBuddyTaskRun 追加一条任务执行记录（最新在前，环形上限 10）。
// 任何失败只告警不影响主流程（留痕是旁路）。
func AppendCodeBuddyTaskRun(ctx context.Context, repo AccountRepository, accountID int64, task, status, detail string) {
	if repo == nil || accountID <= 0 {
		return
	}
	acct, err := repo.GetByID(ctx, accountID)
	if err != nil || acct == nil {
		return
	}
	runs := make([]map[string]any, 0, codeBuddyTaskRunsMaxEntries+1)
	if raw, ok := acct.Extra[codeBuddyTaskRunsKey].([]any); ok {
		for _, it := range raw {
			if m, ok := it.(map[string]any); ok {
				runs = append(runs, m)
			}
		}
	}
	entry := map[string]any{
		"at":     time.Now().UTC().Format(time.RFC3339),
		"task":   task,
		"status": status,
	}
	if detail != "" {
		entry["detail"] = detail
	}
	runs = append([]map[string]any{entry}, runs...)
	if len(runs) > codeBuddyTaskRunsMaxEntries {
		runs = runs[:codeBuddyTaskRunsMaxEntries]
	}
	_ = repo.UpdateExtra(ctx, accountID, map[string]any{codeBuddyTaskRunsKey: runs})
}
