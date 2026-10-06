package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stubPricingPlanRepository 内存版 PricingPlanRepository，用于管理端服务
// 与 APIKeyService 可选套餐读取的单元测试（不进数据库）。
type stubPricingPlanRepository struct {
	mu     sync.Mutex
	nextID int64
	plans  map[int64]*PricingPlan
	models map[int64][]PricingPlanModel
	routes map[int64][]PricingPlanRoute
}

func newStubPricingPlanRepository() *stubPricingPlanRepository {
	return &stubPricingPlanRepository{
		nextID: 1,
		plans:  make(map[int64]*PricingPlan),
		models: make(map[int64][]PricingPlanModel),
		routes: make(map[int64][]PricingPlanRoute),
	}
}

func (s *stubPricingPlanRepository) ListPlans(ctx context.Context, includeDisabled bool) ([]PricingPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PricingPlan, 0, len(s.plans))
	for _, p := range s.plans {
		if !includeDisabled && p.Status != PricingPlanStatusActive {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *stubPricingPlanRepository) ListPublicProducts(ctx context.Context) ([]PricingPlanProduct, error) {
	return nil, nil
}

func (s *stubPricingPlanRepository) GetPlanByID(ctx context.Context, id int64) (*PricingPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return nil, ErrPricingPlanNotFound
	}
	cp := *p
	return &cp, nil
}

func (s *stubPricingPlanRepository) GetPlanByName(ctx context.Context, name string) (*PricingPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.plans {
		if p.Name == name {
			cp := *p
			return &cp, nil
		}
	}
	return nil, ErrPricingPlanNotFound
}

func (s *stubPricingPlanRepository) ListModelsByPlan(ctx context.Context, planID int64, includeDisabled bool) ([]PricingPlanModel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PricingPlanModel, 0)
	for _, m := range s.models[planID] {
		if !includeDisabled && !m.Enabled {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *stubPricingPlanRepository) ListRoutesByPlan(ctx context.Context, planID int64, includeDisabled bool) ([]PricingPlanRoute, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PricingPlanRoute, 0)
	for _, r := range s.routes[planID] {
		if !includeDisabled && !r.Enabled {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *stubPricingPlanRepository) CreatePlan(ctx context.Context, plan *PricingPlan, models []PricingPlanModel, routes []PricingPlanRoute) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.plans {
		if p.Name == plan.Name {
			return ErrPricingPlanExists
		}
	}
	now := time.Now()
	plan.ID = s.nextID
	s.nextID++
	plan.CreatedAt = now
	plan.UpdatedAt = now
	cp := *plan
	s.plans[plan.ID] = &cp
	s.models[plan.ID] = models
	s.routes[plan.ID] = routes
	return nil
}

func (s *stubPricingPlanRepository) UpdatePlan(ctx context.Context, plan *PricingPlan, models []PricingPlanModel, routes []PricingPlanRoute) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[plan.ID]
	if !ok {
		return ErrPricingPlanNotFound
	}
	for id, other := range s.plans {
		if id != plan.ID && other.Name == plan.Name {
			return ErrPricingPlanExists
		}
	}
	plan.UpdatedAt = time.Now()
	cp := *plan
	s.plans[plan.ID] = &cp
	s.models[plan.ID] = models
	s.routes[plan.ID] = routes
	_ = p
	return nil
}

func (s *stubPricingPlanRepository) ReplaceModels(ctx context.Context, planID int64, models []PricingPlanModel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plans[planID]; !ok {
		return ErrPricingPlanNotFound
	}
	s.models[planID] = models
	return nil
}

func (s *stubPricingPlanRepository) ReplaceRoutes(ctx context.Context, planID int64, routes []PricingPlanRoute) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plans[planID]; !ok {
		return ErrPricingPlanNotFound
	}
	s.routes[planID] = routes
	return nil
}

func (s *stubPricingPlanRepository) DeletePlan(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plans[id]; !ok {
		return ErrPricingPlanNotFound
	}
	delete(s.plans, id)
	delete(s.models, id)
	delete(s.routes, id)
	return nil
}

func TestPricingPlanServiceCreateNormalizesAndPersistsContent(t *testing.T) {
	svc := NewPricingPlanService(newStubPricingPlanRepository())
	ctx := context.Background()

	detail, err := svc.Create(ctx, PricingPlanInput{
		Name:        "  pro  ",
		Title:       " Pro Plan ",
		Description: "All models",
		Status:      " DISABLED ",
		IsPublic:    true,
		SortOrder:   5,
	}, []PricingPlanModelInput{
		{PublicModel: "  gpt-5 ", Protocol: " MESSAGES ", UpstreamModel: "", Priority: 10, Enabled: true},
	}, []PricingPlanRouteInput{
		{GroupID: 42, Priority: 10, Enabled: true},
	})
	require.NoError(t, err)
	require.NotZero(t, detail.Plan.ID)
	require.Equal(t, "pro", detail.Plan.Name)
	require.Equal(t, "Pro Plan", detail.Plan.Title)
	require.Equal(t, PricingPlanStatusDisabled, detail.Plan.Status)
	require.Len(t, detail.Models, 1)
	require.Equal(t, "gpt-5", detail.Models[0].PublicModel)
	require.Equal(t, PricingPlanProtocolMessages, detail.Models[0].Protocol)
	require.Len(t, detail.Routes, 1)
	require.Equal(t, int64(42), detail.Routes[0].GroupID)

	// 创建后按 ID 回读（GetWithContent）语义一致。
	again, err := svc.GetWithContent(ctx, detail.Plan.ID)
	require.NoError(t, err)
	require.Equal(t, detail.Plan.Name, again.Plan.Name)
	require.Equal(t, detail.Models, again.Models)
	require.Equal(t, detail.Routes, again.Routes)

	// 同名冲突透传仓库错误。
	_, err = svc.Create(ctx, PricingPlanInput{Name: "pro", Title: "dup"}, nil, nil)
	require.ErrorIs(t, err, ErrPricingPlanExists)
}

func TestPricingPlanServiceCreatePricingRoundTrip(t *testing.T) {
	svc := NewPricingPlanService(newStubPricingPlanRepository())
	ctx := context.Background()

	inputPrice := 3e-6
	outputPrice := 12e-6
	maxTokens := 200000
	detail, err := svc.Create(ctx, PricingPlanInput{Name: "priced", Title: "Priced"}, []PricingPlanModelInput{
		{
			PublicModel: "claude-sonnet",
			Protocol:    PricingPlanProtocolResponses,
			Priority:    1,
			Enabled:     true,
			Pricing: &ChannelModelPricing{
				BillingMode:      BillingModeToken,
				InputPrice:       &inputPrice,
				OutputPrice:      &outputPrice,
				CacheWritePrice:  &inputPrice,
				CacheReadPrice:   &inputPrice,
				FastMultiplier:   float64Ptr(1.5),
				FlexMultiplier:   float64Ptr(0.5),
				ImageInputPrice:  &inputPrice,
				ImageOutputPrice: &outputPrice,
				PerRequestPrice:  float64Ptr(0.1),
				Intervals: []PricingInterval{{
					MinTokens: 1, MaxTokens: &maxTokens, TierLabel: "200k",
					InputPrice: &inputPrice, OutputPrice: &outputPrice,
				}},
				TimePricing: &ChannelTimePricing{
					Timezone: "UTC",
					Periods:  []ChannelTimePricingPeriod{{StartTime: "00:00", EndTime: "08:00", Multiplier: 0.8}},
				},
			},
		},
	}, nil)
	require.NoError(t, err)
	require.Len(t, detail.Models, 1)
	got := detail.Models[0].Pricing
	require.NotNil(t, got)
	require.Equal(t, BillingModeToken, got.BillingMode)
	require.Equal(t, &inputPrice, got.InputPrice)
	require.Equal(t, &outputPrice, got.OutputPrice)
	require.Equal(t, float64Ptr(1.5), got.FastMultiplier)
	require.Len(t, got.Intervals, 1)
	require.Equal(t, "200k", got.Intervals[0].TierLabel)
	require.NotNil(t, got.TimePricing)
	require.Equal(t, 0.8, got.TimePricing.Periods[0].Multiplier)
}

func TestPricingPlanServiceUpdateReplacesContent(t *testing.T) {
	svc := NewPricingPlanService(newStubPricingPlanRepository())
	ctx := context.Background()
	created, err := svc.Create(ctx, PricingPlanInput{Name: "v1", Title: "V1", Status: PricingPlanStatusActive},
		[]PricingPlanModelInput{{PublicModel: "gpt-5", Protocol: PricingPlanProtocolChatCompletions, Enabled: true}},
		[]PricingPlanRouteInput{{GroupID: 10, Priority: 10, Enabled: true}})
	require.NoError(t, err)

	updated, err := svc.Update(ctx, created.Plan.ID,
		PricingPlanInput{Name: "v2", Title: "V2", IsPublic: true},
		[]PricingPlanModelInput{{PublicModel: "gpt-5-mini", Protocol: PricingPlanProtocolResponses, Enabled: true}},
		[]PricingPlanRouteInput{{GroupID: 20, Priority: 5, Enabled: true}},
	)
	require.NoError(t, err)
	require.Equal(t, "v2", updated.Plan.Name)
	require.Len(t, updated.Models, 1)
	require.Equal(t, "gpt-5-mini", updated.Models[0].PublicModel)
	require.Equal(t, PricingPlanProtocolResponses, updated.Models[0].Protocol)
	require.Len(t, updated.Routes, 1)
	require.Equal(t, int64(20), updated.Routes[0].GroupID)

	// 不存在 → ErrPricingPlanNotFound。
	_, err = svc.Update(ctx, 99999, PricingPlanInput{Name: "x"}, nil, nil)
	require.ErrorIs(t, err, ErrPricingPlanNotFound)
}

func TestPricingPlanServiceDeleteAndList(t *testing.T) {
	svc := NewPricingPlanService(newStubPricingPlanRepository())
	ctx := context.Background()
	a, err := svc.Create(ctx, PricingPlanInput{Name: "a", Status: PricingPlanStatusActive, SortOrder: 2}, nil, nil)
	require.NoError(t, err)
	b, err := svc.Create(ctx, PricingPlanInput{Name: "b", Status: PricingPlanStatusDisabled, SortOrder: 1}, nil, nil)
	require.NoError(t, err)

	active, err := svc.List(ctx, false)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "a", active[0].Name)

	all, err := svc.List(ctx, true)
	require.NoError(t, err)
	require.Len(t, all, 2)

	require.NoError(t, svc.Delete(ctx, a.Plan.ID))
	_, err = svc.GetWithContent(ctx, a.Plan.ID)
	require.ErrorIs(t, err, ErrPricingPlanNotFound)
	require.NoError(t, svc.Delete(ctx, b.Plan.ID))
	require.ErrorIs(t, svc.Delete(ctx, b.Plan.ID), ErrPricingPlanNotFound)
	left, err := svc.List(ctx, true)
	require.NoError(t, err)
	require.Empty(t, left)
}

func TestPricingPlanServiceValidation(t *testing.T) {
	svc := NewPricingPlanService(newStubPricingPlanRepository())
	ctx := context.Background()

	t.Run("missing plan name", func(t *testing.T) {
		_, err := svc.Create(ctx, PricingPlanInput{Title: "no name"}, nil, nil)
		require.ErrorIs(t, err, ErrPricingPlanInvalidInput)
	})

	t.Run("model without public model", func(t *testing.T) {
		_, err := svc.Create(ctx, PricingPlanInput{Name: "x"},
			[]PricingPlanModelInput{{PublicModel: "  ", Protocol: PricingPlanProtocolChatCompletions}}, nil)
		require.ErrorIs(t, err, ErrPricingPlanInvalidInput)
	})

	t.Run("unknown model protocol", func(t *testing.T) {
		_, err := svc.Create(ctx, PricingPlanInput{Name: "x"},
			[]PricingPlanModelInput{{PublicModel: "gpt-5", Protocol: "unknown", Direct: true, Enabled: true}}, nil)
		require.ErrorIs(t, err, ErrPricingPlanInvalidInput)
	})

	t.Run("compatibility fallback only applies to non-direct chat", func(t *testing.T) {
		_, err := svc.Create(ctx, PricingPlanInput{Name: "x"},
			[]PricingPlanModelInput{{
				PublicModel: "gpt-5", Protocol: PricingPlanProtocolResponses,
				AllowCompatibilityFallback: true, Enabled: true,
			}}, nil)
		require.ErrorIs(t, err, ErrPricingPlanInvalidInput)
	})

	t.Run("duplicate public model protocol is rejected", func(t *testing.T) {
		_, err := svc.Create(ctx, PricingPlanInput{Name: "x"}, []PricingPlanModelInput{
			{PublicModel: "GPT-5", Protocol: PricingPlanProtocolChatCompletions, Enabled: true},
			{PublicModel: "gpt-5", Protocol: PricingPlanProtocolChatCompletions, Enabled: true},
		}, nil)
		require.ErrorIs(t, err, ErrPricingPlanInvalidInput)
	})

	t.Run("invalid pricing document is rejected", func(t *testing.T) {
		negative := -1.0
		_, err := svc.Create(ctx, PricingPlanInput{Name: "x"}, []PricingPlanModelInput{{
			PublicModel: "gpt-5", Protocol: PricingPlanProtocolChatCompletions, Enabled: true,
			Pricing: &ChannelModelPricing{BillingMode: BillingMode("invalid"), InputPrice: &negative},
		}}, nil)
		require.ErrorIs(t, err, ErrPricingPlanInvalidInput)
	})

	t.Run("per request pricing requires a price or tier", func(t *testing.T) {
		_, err := svc.Create(ctx, PricingPlanInput{Name: "x"}, []PricingPlanModelInput{{
			PublicModel: "image-model", Protocol: PricingPlanProtocolChatCompletions, Enabled: true,
			Pricing: &ChannelModelPricing{BillingMode: BillingModeImage},
		}}, nil)
		require.ErrorIs(t, err, ErrPricingPlanInvalidInput)
	})

	t.Run("overlapping pricing intervals are rejected", func(t *testing.T) {
		firstMax, secondMax := 100, 200
		price := 0.1
		_, err := svc.Create(ctx, PricingPlanInput{Name: "x"}, []PricingPlanModelInput{{
			PublicModel: "gpt-5", Protocol: PricingPlanProtocolChatCompletions, Enabled: true,
			Pricing: &ChannelModelPricing{BillingMode: BillingModeToken, Intervals: []PricingInterval{
				{MinTokens: 0, MaxTokens: &firstMax, InputPrice: &price},
				{MinTokens: 50, MaxTokens: &secondMax, InputPrice: &price},
			}},
		}}, nil)
		require.ErrorIs(t, err, ErrPricingPlanInvalidInput)
	})
	t.Run("duplicate route group or priority is rejected", func(t *testing.T) {
		_, err := svc.Create(ctx, PricingPlanInput{Name: "x"}, nil, []PricingPlanRouteInput{
			{GroupID: 1, Priority: 10, Enabled: true},
			{GroupID: 1, Priority: 20, Enabled: true},
		})
		require.ErrorIs(t, err, ErrPricingPlanInvalidInput)

		_, err = svc.Create(ctx, PricingPlanInput{Name: "y"}, nil, []PricingPlanRouteInput{
			{GroupID: 1, Priority: 10, Enabled: true},
			{GroupID: 2, Priority: 10, Enabled: true},
		})
		require.ErrorIs(t, err, ErrPricingPlanInvalidInput)
	})
}

func TestPricingPlanServiceNilRepoFailsClosed(t *testing.T) {
	svc := NewPricingPlanService(nil)
	ctx := context.Background()
	_, err := svc.List(ctx, false)
	require.ErrorIs(t, err, ErrPricingPlanUnavailable)
	_, err = svc.GetWithContent(ctx, 1)
	require.ErrorIs(t, err, ErrPricingPlanUnavailable)
	_, err = svc.Create(ctx, PricingPlanInput{Name: "x"}, nil, nil)
	require.ErrorIs(t, err, ErrPricingPlanUnavailable)
	_, err = svc.Update(ctx, 1, PricingPlanInput{Name: "x"}, nil, nil)
	require.ErrorIs(t, err, ErrPricingPlanUnavailable)
	require.ErrorIs(t, svc.Delete(ctx, 1), ErrPricingPlanUnavailable)
}

func TestAPIKeyServiceGetSelectablePricingPlans(t *testing.T) {
	ctx := context.Background()

	t.Run("nil repo returns empty list", func(t *testing.T) {
		svc := NewAPIKeyService(nil, nil, nil, nil, nil, nil, nil)
		plans, err := svc.GetSelectablePricingPlans(ctx)
		require.NoError(t, err)
		require.Empty(t, plans)
	})

	t.Run("only active public plans", func(t *testing.T) {
		repo := newStubPricingPlanRepository()
		require.NoError(t, repo.CreatePlan(ctx, &PricingPlan{Name: "public", Title: "Public", Status: PricingPlanStatusActive, IsPublic: true}, nil, nil))
		require.NoError(t, repo.CreatePlan(ctx, &PricingPlan{Name: "private", Title: "Private", Status: PricingPlanStatusActive, IsPublic: false}, nil, nil))
		require.NoError(t, repo.CreatePlan(ctx, &PricingPlan{Name: "retired", Title: "Retired", Status: PricingPlanStatusDisabled, IsPublic: true}, nil, nil))

		svc := NewAPIKeyService(nil, nil, nil, nil, nil, nil, nil)
		svc.SetPricingPlanRepository(repo)

		plans, err := svc.GetSelectablePricingPlans(ctx)
		require.NoError(t, err)
		require.Len(t, plans, 1)
		require.Equal(t, "public", plans[0].Name)
	})
}
