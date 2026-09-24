package service

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodeBuddyWeightedPick_Empty(t *testing.T) {
	assert.Nil(t, codeBuddyWeightedPick(nil, time.Now(), rand.New(rand.NewSource(1))))
}

func TestCodeBuddyWeightedPick_SingleCandidate(t *testing.T) {
	a := &Account{ID: 1}
	got := codeBuddyWeightedPick([]*Account{a}, time.Now(), rand.New(rand.NewSource(1)))
	assert.Same(t, a, got)
}

func TestCodeBuddyWeightedPick_ShortlistCapsAt5(t *testing.T) {
	// 前两名(质量序头部)永远在短名单内；第6名及之后永不入选。
	now := time.Now()
	mk := func(id int64, idleHours float64) *Account {
		tm := now.Add(-time.Duration(idleHours * float64(time.Hour)))
		return &Account{ID: id, LastUsedAt: &tm}
	}
	cands := []*Account{mk(1, 100), mk(2, 100), mk(3, 100), mk(4, 100), mk(5, 100), mk(6, 0), mk(7, 0)}
	rnd := rand.New(rand.NewSource(42))
	seen := map[int64]bool{}
	for i := 0; i < 500; i++ {
		got := codeBuddyWeightedPick(cands, now, rnd)
		require.NotNil(t, got)
		seen[got.ID] = true
	}
	assert.NotContains(t, seen, int64(6), "短名单外的账号不得入选")
	assert.NotContains(t, seen, int64(7))
	assert.Greater(t, len(seen), 1, "短名单内应有随机性")
}

func TestCodeBuddyWeightedPick_IdleCompensationSpreadsLoad(t *testing.T) {
	// 两号质量并列：刚用过的 vs 闲置24h → 闲置的应显著更常被选中。
	now := time.Now()
	fresh := &Account{ID: 1, LastUsedAt: codebuddyWPPtrTime(now)}
	idle := &Account{ID: 2, LastUsedAt: codebuddyWPPtrTime(now.Add(-24 * time.Hour))}
	rnd := rand.New(rand.NewSource(7))
	idlePicked := 0
	for i := 0; i < 1000; i++ {
		got := codeBuddyWeightedPick([]*Account{fresh, idle}, now, rnd)
		if got.ID == 2 {
			idlePicked++
		}
	}
	assert.Greater(t, idlePicked, 700, "闲置补偿应让闲置号显著占优(权重≈24:1)")
}

func codebuddyWPPtrTime(t time.Time) *time.Time { return &t }
