package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 成长链台账测试（A6 批 P2）。
//
// 台账的作用是**当日去重**：它一旦读错，后果不是"多跑一次"这么轻——
// 上游对成长链的幂等**不是全覆盖**（抽奖 draw 每次新 client_token，设计上就是
// "每次都真抽"，重复调用会真的消耗次数）。所以读路径必须钉死：
//   - 非字符串脏值不能伪装成合法日期；
//   - 键必须带前缀（Extra 是跨功能共享 map，裸键会撞名）；
//   - 写入要同时更新内存（否则同轮后续步骤读到旧值，"刚补完签又补一次"）；
//   - 写库失败要留痕（不能静默）。

// failingExtraRepo UpdateExtra 恒失败，用于验证"写库失败要留痕"。
type failingExtraRepo struct {
	AccountRepository
	account *Account
}

func (r *failingExtraRepo) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	return errors.New("boom: db is down")
}

// --- 读路径 ---

func TestCodeBuddyGrowthLedgerValueReadsStringDate(t *testing.T) {
	account := &Account{
		ID: 1,
		Extra: map[string]any{
			codeBuddyGrowthLedgerKey(ledgerTravelDepart): "2026-09-22",
		},
	}
	require.Equal(t, "2026-09-22", codeBuddyGrowthLedgerValue(account, ledgerTravelDepart))
}

func TestCodeBuddyGrowthLedgerValueMissingKeyIsEmpty(t *testing.T) {
	require.Equal(t, "", codeBuddyGrowthLedgerValue(&Account{ID: 1}, ledgerTravelDepart))
	require.Equal(t, "", codeBuddyGrowthLedgerValue(&Account{ID: 1, Extra: map[string]any{}}, ledgerTravelDepart))
	require.Equal(t, "", codeBuddyGrowthLedgerValue(nil, ledgerTravelDepart))
}

// 脏值必须当"没有记录"，**不能**用 fmt.Sprint 兜底。
//
// 反例：nil 经 fmt.Sprint 会变成字符串 "nil"；数字会被渲染成 "20260922" 之类。
// 这些值不等于任何日期，表面上"没记录"，但一旦哪天有人写了 `!= ""` 之类的前缀判断，
// 脏数据就会被当成"有记录"——而它的真实后果是当天**跳过**该动作。
func TestCodeBuddyGrowthLedgerValueRejectsNonStringValues(t *testing.T) {
	for name, raw := range map[string]any{
		"nil":    nil,
		"bool":   true,
		"number": 20260922,
		"float":  20260922.0,
		"nested": map[string]any{"date": "2026-09-22"},
		"slice":  []string{"2026-09-22"},
		"空字符串":   "",
		"只有空白":   "   ",
	} {
		t.Run(name, func(t *testing.T) {
			account := &Account{ID: 1, Extra: map[string]any{
				codeBuddyGrowthLedgerKey(ledgerTravelDepart): raw,
			}}
			require.Equal(t, "", codeBuddyGrowthLedgerValue(account, ledgerTravelDepart),
				"非字符串值必须按「没有记录」处理")
		})
	}
}

func TestCodeBuddyGrowthLedgerValueTrimsWhitespace(t *testing.T) {
	account := &Account{ID: 1, Extra: map[string]any{
		codeBuddyGrowthLedgerKey(ledgerTravelDepart): "  2026-09-22  ",
	}}
	require.Equal(t, "2026-09-22", codeBuddyGrowthLedgerValue(account, ledgerTravelDepart))
}

// 台账键必须带统一前缀：Extra 是跨功能共享 map（OpenAI 长上下文、grok 媒体资格
// 都在写），裸键迟早撞名，且撞名后表现为"某个功能莫名不生效"。
func TestCodeBuddyGrowthLedgerKeysArePrefixed(t *testing.T) {
	names := []string{
		ledgerTravelDepart, ledgerTravelClaim, ledgerAdoptTried,
		ledgerStreakClaimed, ledgerMakeupUse, ledgerNightCat,
		ledgerSchool, ledgerSchoolOffline, ledgerTrialClaimed,
	}
	for _, name := range names {
		key := codeBuddyGrowthLedgerKey(name)
		require.True(t, len(key) > len(codeBuddyGrowthLedgerPrefix) &&
			key[:len(codeBuddyGrowthLedgerPrefix)] == codeBuddyGrowthLedgerPrefix,
			"台账键 %q 缺少统一前缀 %q", key, codeBuddyGrowthLedgerPrefix)
		// 前缀本身不得为空——否则上面的判断退化成恒真。
		require.NotEmpty(t, codeBuddyGrowthLedgerPrefix)
	}

	// 台账键之间不得重名（重名会让两个通道互相覆盖去重标记）。
	seen := map[string]string{}
	for _, name := range names {
		key := codeBuddyGrowthLedgerKey(name)
		if prev, dup := seen[key]; dup {
			t.Fatalf("台账键重复：%q 与 %q 撞名", prev, name)
		}
		seen[key] = name
	}
}

func TestCodeBuddyGrowthLedgerTodayComparesExactly(t *testing.T) {
	account := &Account{ID: 1, Extra: map[string]any{
		codeBuddyGrowthLedgerKey(ledgerTravelDepart): "2026-09-22",
	}}
	require.True(t, codeBuddyGrowthLedgerToday(account, ledgerTravelDepart, "2026-09-22"))
	require.False(t, codeBuddyGrowthLedgerToday(account, ledgerTravelDepart, "2026-09-23"),
		"换成另一天就不算今天（这是次日应重新触发的前提）")
	require.False(t, codeBuddyGrowthLedgerToday(account, ledgerTravelDepart, ""),
		"空日期不得匹配（否则脏值会让去重恒真）")
}

// --- 写路径 ---

func TestMarkCodeBuddyGrowthLedgerWritesExtraAndMemory(t *testing.T) {
	account := &Account{ID: 7, Platform: PlatformCodeBuddy}
	repo := newCodebuddyAdminTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	svc.markCodeBuddyGrowthLedger(context.Background(), account, "2026-09-22",
		ledgerTravelDepart, ledgerTravelClaim)

	// 落库。
	stored := repo.accounts[7]
	require.Equal(t, "2026-09-22", codeBuddyGrowthLedgerValue(stored, ledgerTravelDepart))
	require.Equal(t, "2026-09-22", codeBuddyGrowthLedgerValue(stored, ledgerTravelClaim))

	// 内存同步更新（同轮后续步骤读的是同一个 *Account）。
	require.Equal(t, "2026-09-22", codeBuddyGrowthLedgerValue(account, ledgerTravelDepart),
		"必须同时更新内存，否则同轮后续步骤读到旧值（刚补完签又补一次）")
}

// 写库失败**只记 WARN 不回滚**：当轮动作已经真的对上游做了，撤掉内存标记会让
// 下一轮再打一次上游，反而更糟。这里断言"内存标记仍在"。
func TestMarkCodeBuddyGrowthLedgerKeepsMemoryMarkerWhenPersistFails(t *testing.T) {
	account := &Account{ID: 9, Platform: PlatformCodeBuddy}
	svc := NewCodeBuddyAdminService(nil, &failingExtraRepo{account: account}, nil)

	require.NotPanics(t, func() {
		svc.markCodeBuddyGrowthLedger(context.Background(), account, "2026-09-22", ledgerTravelDepart)
	})

	require.Equal(t, "2026-09-22", codeBuddyGrowthLedgerValue(account, ledgerTravelDepart),
		"落库失败不得回滚内存标记（回滚会让下一轮重复打上游）")
}

func TestMarkCodeBuddyGrowthLedgerToleratesNilInputs(t *testing.T) {
	svc := NewCodeBuddyAdminService(nil, newCodebuddyAdminTestRepo(), nil)

	require.NotPanics(t, func() {
		svc.markCodeBuddyGrowthLedger(context.Background(), nil, "2026-09-22", ledgerTravelDepart)
	})
	require.NotPanics(t, func() {
		svc.markCodeBuddyGrowthLedger(context.Background(), &Account{ID: 1}, "2026-09-22")
	})
}

func TestMarkCodeBuddyGrowthLedgerInitializesNilExtra(t *testing.T) {
	account := &Account{ID: 3, Platform: PlatformCodeBuddy} // Extra 为 nil
	repo := newCodebuddyAdminTestRepo(account)
	svc := NewCodeBuddyAdminService(nil, repo, nil)

	svc.markCodeBuddyGrowthLedger(context.Background(), account, "2026-09-23", ledgerMakeupUse)

	require.Equal(t, "2026-09-23", codeBuddyGrowthLedgerValue(account, ledgerMakeupUse))
}

// --- 日期口径（A4/A5 一致：UTC+8 当地日）---

// UTC+8 的 00:00–08:00 区间是**最容易算错的地方**：北京时间已是新的一天，
// 而 UTC 还在前一天。按 UTC 取日期会让"今天已经跑过"被误判成"还没跑"。
func TestCodeBuddyGrowthLocalDayUsesUTC8(t *testing.T) {
	cases := []struct {
		name string
		utc  time.Time
		want string
	}{
		{
			// UTC 2026-09-21 16:30 == 北京时间 2026-09-22 00:30（跨日临界）。
			name: "UTC 前一日下午 = 北京次日凌晨",
			utc:  time.Date(2026, 9, 21, 16, 30, 0, 0, time.UTC),
			want: "2026-09-22",
		},
		{
			// UTC 2026-09-21 15:59 == 北京时间 2026-09-21 23:59（仍是前一日）。
			name: "UTC 前一日 15:59 = 北京当日 23:59",
			utc:  time.Date(2026, 9, 21, 15, 59, 0, 0, time.UTC),
			want: "2026-09-21",
		},
		{
			// UTC 2026-09-21 16:00 == 北京时间 2026-09-22 00:00（整点跨日）。
			name: "UTC 前一日 16:00 = 北京次日 00:00",
			utc:  time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC),
			want: "2026-09-22",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, codeBuddyGrowthLocalDay(tc.utc))
		})
	}
}

func TestCodeBuddyGrowthYesterdayUsesUTC8(t *testing.T) {
	cases := []struct {
		name string
		utc  time.Time
		want string
	}{
		{
			// 北京时间 2026-09-22 00:30 → 昨日应为 09-21（而不是 09-20）。
			name: "北京凌晨的昨日",
			utc:  time.Date(2026, 9, 21, 16, 30, 0, 0, time.UTC),
			want: "2026-09-21",
		},
		{
			// 北京时间 2026-09-22 20:00 → 昨日 09-21。
			name: "北京白天的昨日",
			utc:  time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
			want: "2026-09-21",
		},
		{
			// 跨月：北京时间 2026-10-01 00:30 → 昨日 09-30。
			name: "跨月的昨日",
			utc:  time.Date(2026, 9, 30, 16, 30, 0, 0, time.UTC),
			want: "2026-09-30",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, codeBuddyGrowthYesterday(tc.utc))
		})
	}
}

// 台账口径必须与 A4 的 PlatformFeatureLocalDate 同源——两处若各算各的，
// "签到已跑"与"成长链已跑"会在跨日临界上错开一天。
func TestCodeBuddyGrowthLocalDayMatchesPlatformFeatureLocalDate(t *testing.T) {
	moments := []time.Time{
		time.Date(2026, 9, 21, 15, 59, 59, 0, time.UTC),
		time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 31, 16, 30, 0, 0, time.UTC), // 跨年
	}
	for _, m := range moments {
		require.Equal(t,
			PlatformFeatureLocalDate(m, codeBuddyTimeZone).Format(time.DateOnly),
			codeBuddyGrowthLocalDay(m),
			"台账与 A4 的当日口径必须一致（时刻 %s）", m.Format(time.RFC3339))
	}
}
