package service

import (
	"math/rand"
	"time"
)

// A2/A3/C3（吸收自 workbuddy2api）v1：codebuddy 选号的**闲置补偿 + Top-5 加权随机**。
//
// 上游的三因子（credits×10 + 快过期×8 + 闲置补偿）中，前两个需要每账号积分数据；
// 在选号热路径上实时查上游不可接受（WAF/延迟），故 v1 只用本地信号 last_used_at
// 做「闲置补偿」：越久没用过的号权重越高，把流量从"刚用过的号"摊开——配合粘性
// 与冷却，达成"三号配额同步消耗"。积分因子待 A5 账本（deferred）落地后接入，
// 届时权重函数在此处扩展，选号结构不变。
//
// 输入 eligible 已按既有质量序（compact tier / rate / isBetterAccount）排好，
// 本函数只在**前 K 名短名单内**做概率倾斜（K=5，对齐上游 Top-5 短名单+抽签），
// 硬排序语义（compact tier 等）不被破坏。
const codeBuddyWeightedPickShortlist = 5

// codeBuddyWeightedPick 从质量序候选的短名单内加权随机取一个。
// rnd 注入以便测试确定性；权重 = 1h 基础 + 每闲置 1h 加 1（封顶 24h）。
func codeBuddyWeightedPick(eligible []*Account, now time.Time, rnd *rand.Rand) *Account {
	if len(eligible) == 0 {
		return nil
	}
	k := len(eligible)
	if k > codeBuddyWeightedPickShortlist {
		k = codeBuddyWeightedPickShortlist
	}
	shortlist := eligible[:k]

	weights := make([]float64, k)
	total := 0.0
	for i, acct := range shortlist {
		idle := 24.0
		if acct.LastUsedAt != nil {
			idle = now.Sub(*acct.LastUsedAt).Hours()
			if idle < 0 {
				idle = 0
			}
			if idle > 24 {
				idle = 24
			}
		}
		weights[i] = 1 + idle // 1h 基础，闲置补偿封顶 24
		total += weights[i]
	}
	if total <= 0 || rnd == nil {
		return shortlist[0]
	}
	x := rnd.Float64() * total
	for i, w := range weights {
		x -= w
		if x <= 0 {
			return shortlist[i]
		}
	}
	return shortlist[k-1]
}
