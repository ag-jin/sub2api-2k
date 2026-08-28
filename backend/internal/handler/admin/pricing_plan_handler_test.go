package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// stubAdminPricingPlanRepo 管理端套餐 handler 测试用的 PricingPlanRepository
// 桩：内存存储，语义与仓库一致（Create 记 ID、Update 整体替换、Delete 删除）。
type stubAdminPricingPlanRepo struct {
	nextID int64
	plans  map[int64]*service.PricingPlan
	models map[int64][]service.PricingPlanModel
	routes map[int64][]service.PricingPlanRoute
}

func newStubAdminPricingPlanRepo() *stubAdminPricingPlanRepo {
	return &stubAdminPricingPlanRepo{
		nextID: 1,
		plans:  make(map[int64]*service.PricingPlan),
		models: make(map[int64][]service.PricingPlanModel),
		routes: make(map[int64][]service.PricingPlanRoute),
	}
}

func (s *stubAdminPricingPlanRepo) ListPlans(_ context.Context, includeDisabled bool) ([]service.PricingPlan, error) {
	out := make([]service.PricingPlan, 0, len(s.plans))
	for _, p := range s.plans {
		if !includeDisabled && p.Status != service.PricingPlanStatusActive {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *stubAdminPricingPlanRepo) ListPublicProducts(context.Context) ([]service.PricingPlanProduct, error) {
	return nil, nil
}

func (s *stubAdminPricingPlanRepo) GetPlanByID(_ context.Context, id int64) (*service.PricingPlan, error) {
	p, ok := s.plans[id]
	if !ok {
		return nil, service.ErrPricingPlanNotFound
	}
	cp := *p
	return &cp, nil
}

func (s *stubAdminPricingPlanRepo) GetPlanByName(_ context.Context, name string) (*service.PricingPlan, error) {
	for _, p := range s.plans {
		if p.Name == name {
			cp := *p
			return &cp, nil
		}
	}
	return nil, service.ErrPricingPlanNotFound
}

func (s *stubAdminPricingPlanRepo) ListModelsByPlan(_ context.Context, planID int64, includeDisabled bool) ([]service.PricingPlanModel, error) {
	out := make([]service.PricingPlanModel, 0)
	for _, m := range s.models[planID] {
		if !includeDisabled && !m.Enabled {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *stubAdminPricingPlanRepo) ListRoutesByPlan(_ context.Context, planID int64, includeDisabled bool) ([]service.PricingPlanRoute, error) {
	out := make([]service.PricingPlanRoute, 0)
	for _, r := range s.routes[planID] {
		if !includeDisabled && !r.Enabled {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *stubAdminPricingPlanRepo) CreatePlan(_ context.Context, plan *service.PricingPlan, models []service.PricingPlanModel, routes []service.PricingPlanRoute) error {
	for _, p := range s.plans {
		if p.Name == plan.Name {
			return service.ErrPricingPlanExists
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

func (s *stubAdminPricingPlanRepo) UpdatePlan(_ context.Context, plan *service.PricingPlan, models []service.PricingPlanModel, routes []service.PricingPlanRoute) error {
	if _, ok := s.plans[plan.ID]; !ok {
		return service.ErrPricingPlanNotFound
	}
	for id, p := range s.plans {
		if id != plan.ID && p.Name == plan.Name {
			return service.ErrPricingPlanExists
		}
	}
	plan.UpdatedAt = time.Now()
	cp := *plan
	s.plans[plan.ID] = &cp
	s.models[plan.ID] = models
	s.routes[plan.ID] = routes
	return nil
}

func (s *stubAdminPricingPlanRepo) ReplaceModels(_ context.Context, planID int64, models []service.PricingPlanModel) error {
	if _, ok := s.plans[planID]; !ok {
		return service.ErrPricingPlanNotFound
	}
	s.models[planID] = models
	return nil
}

func (s *stubAdminPricingPlanRepo) ReplaceRoutes(_ context.Context, planID int64, routes []service.PricingPlanRoute) error {
	if _, ok := s.plans[planID]; !ok {
		return service.ErrPricingPlanNotFound
	}
	s.routes[planID] = routes
	return nil
}

func (s *stubAdminPricingPlanRepo) DeletePlan(_ context.Context, id int64) error {
	if _, ok := s.plans[id]; !ok {
		return service.ErrPricingPlanNotFound
	}
	delete(s.plans, id)
	delete(s.models, id)
	delete(s.routes, id)
	return nil
}

func newTestPricingPlanHandler() (*PricingPlanHandler, *stubAdminPricingPlanRepo) {
	repo := newStubAdminPricingPlanRepo()
	return NewPricingPlanHandler(service.NewPricingPlanService(repo)), repo
}

func pricingPlanRequestJSON(t *testing.T, h *PricingPlanHandler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = []gin.Param{{Key: "id", Value: "1"}}
	switch method {
	case http.MethodPost:
		h.Create(c)
	case http.MethodPut:
		h.Update(c)
	case http.MethodDelete:
		h.Delete(c)
	case http.MethodGet:
		h.GetByID(c)
	}
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return w.Code, resp
}

func pricingPlanPlanBody(name string) UpsertPricingPlanRequest {
	return UpsertPricingPlanRequest{
		Name:        name,
		Title:       "Title " + name,
		Description: "desc",
		Status:      service.PricingPlanStatusActive,
		IsPublic:    true,
		SortOrder:   3,
		Models: []PricingPlanModelRequest{{
			PublicModel: " gpt-5 ",
			Protocol:    " RESPONSES ",
			Priority:    10,
			Enabled:     true,
			Pricing: &domain.PlanModelPricing{
				BillingMode: "token",
				InputPrice:  pricingPlanFloat64Ptr(2e-6),
				OutputPrice: pricingPlanFloat64Ptr(12e-6),
			},
		}},
		Routes: []PricingPlanRouteRequest{{
			GroupID:  42,
			Priority: 10,
			Enabled:  true,
		}},
	}
}

func TestPricingPlanHandlerCreateReturnsFullDetail(t *testing.T) {
	h, repo := newTestPricingPlanHandler()
	code, resp := pricingPlanRequestJSON(t, h, http.MethodPost, "/api/v1/admin/pricing-plans", pricingPlanPlanBody("pro"))
	require.Equal(t, http.StatusOK, code)

	data, ok := resp["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "pro", data["name"])
	require.Equal(t, true, data["is_public"])
	models, ok := data["models"].([]any)
	require.True(t, ok)
	require.Len(t, models, 1)
	model, ok := models[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "gpt-5", model["public_model"])
	require.Equal(t, service.PricingPlanProtocolResponses, model["protocol"])
	pricing, ok := model["pricing"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "token", pricing["billing_mode"])
	require.Equal(t, 2e-6, pricing["input_price"])
	// 定价响应为持久化文档形状：不含渠道/实体字段。
	for _, key := range []string{"id", "channel_id", "platform", "models", "created_at", "updated_at"} {
		_, present := pricing[key]
		require.False(t, present, "internal pricing field %q must not appear", key)
	}
	routes, ok := data["routes"].([]any)
	require.True(t, ok)
	require.Len(t, routes, 1)
	route, ok := routes[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(42), route["group_id"])

	// 已落库，可经 GetByID 回读。
	planID := int64(data["id"].(float64))
	_, ok = repo.plans[planID]
	require.True(t, ok)

	code, resp = pricingPlanRequestJSON(t, h, http.MethodGet, "/api/v1/admin/pricing-plans/1", nil)
	require.Equal(t, http.StatusOK, code)
	got := resp["data"].(map[string]any)
	require.Equal(t, "pro", got["name"])
	require.Len(t, got["models"].([]any), 1)
	require.Len(t, got["routes"].([]any), 1)
}

func TestPricingPlanHandlerCreateValidationFailures(t *testing.T) {
	h, _ := newTestPricingPlanHandler()

	t.Run("missing name", func(t *testing.T) {
		code, resp := pricingPlanRequestJSON(t, h, http.MethodPost, "/api/v1/admin/pricing-plans", UpsertPricingPlanRequest{Title: "no name"})
		require.Equal(t, http.StatusBadRequest, code)
		require.NotEmpty(t, resp["message"])
	})

	t.Run("model without public model", func(t *testing.T) {
		body := pricingPlanPlanBody("x")
		body.Models = []PricingPlanModelRequest{{Protocol: "chat_completions"}}
		code, _ := pricingPlanRequestJSON(t, h, http.MethodPost, "/api/v1/admin/pricing-plans", body)
		require.Equal(t, http.StatusBadRequest, code)
	})

	t.Run("route without group", func(t *testing.T) {
		body := pricingPlanPlanBody("x")
		body.Routes = []PricingPlanRouteRequest{{GroupID: 0, Priority: 1}}
		code, _ := pricingPlanRequestJSON(t, h, http.MethodPost, "/api/v1/admin/pricing-plans", body)
		require.Equal(t, http.StatusBadRequest, code)
	})
}

func TestPricingPlanHandlerUpdateReplacesContent(t *testing.T) {
	h, _ := newTestPricingPlanHandler()
	code, resp := pricingPlanRequestJSON(t, h, http.MethodPost, "/api/v1/admin/pricing-plans", pricingPlanPlanBody("v1"))
	require.Equal(t, http.StatusOK, code)
	planID := int64(resp["data"].(map[string]any)["id"].(float64))

	body := pricingPlanPlanBody("v2")
	body.Models[0].PublicModel = "claude-sonnet"
	body.Models[0].Protocol = service.PricingPlanProtocolMessages
	body.Routes = []PricingPlanRouteRequest{{GroupID: 7, Priority: 5, Enabled: true}}
	code, resp = pricingPlanRequestJSON(t, h, http.MethodPut, "/api/v1/admin/pricing-plans/1", body)
	require.Equal(t, http.StatusOK, code)
	data := resp["data"].(map[string]any)
	require.Equal(t, "v2", data["name"])
	require.Equal(t, planID, int64(data["id"].(float64)), "update 保持原 ID")
	require.Equal(t, "claude-sonnet", data["models"].([]any)[0].(map[string]any)["public_model"])
	require.Equal(t, float64(7), data["routes"].([]any)[0].(map[string]any)["group_id"])
}

func TestPricingPlanHandlerDelete(t *testing.T) {
	h, repo := newTestPricingPlanHandler()
	code, resp := pricingPlanRequestJSON(t, h, http.MethodPost, "/api/v1/admin/pricing-plans", pricingPlanPlanBody("doomed"))
	require.Equal(t, http.StatusOK, code)
	planID := int64(resp["data"].(map[string]any)["id"].(float64))

	code, resp = pricingPlanRequestJSON(t, h, http.MethodDelete, "/api/v1/admin/pricing-plans/1", nil)
	require.Equal(t, http.StatusOK, code)
	require.NotEmpty(t, resp["message"])
	_, ok := repo.plans[planID]
	require.False(t, ok)

	// 删除不存在的套餐 → 404。
	code, _ = pricingPlanRequestJSON(t, h, http.MethodDelete, "/api/v1/admin/pricing-plans/1", nil)
	require.Equal(t, http.StatusNotFound, code)
}

func TestPricingPlanHandlerListFiltersDisabled(t *testing.T) {
	h, _ := newTestPricingPlanHandler()
	code, _ := pricingPlanRequestJSON(t, h, http.MethodPost, "/api/v1/admin/pricing-plans", pricingPlanPlanBody("active-plan"))
	require.Equal(t, http.StatusOK, code)
	body := pricingPlanPlanBody("retired-plan")
	body.Status = service.PricingPlanStatusDisabled
	code, resp := pricingPlanRequestJSON(t, h, http.MethodPost, "/api/v1/admin/pricing-plans", body)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp["data"])

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/pricing-plans", nil)
	h.List(c)
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	items := out["data"].([]any)
	require.Len(t, items, 1, "默认只列 active")
	require.Equal(t, "active-plan", items[0].(map[string]any)["name"])

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/pricing-plans?include_disabled=true", nil)
	h.List(c2)
	var out2 map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &out2))
	require.Len(t, out2["data"].([]any), 2)
}

// pricingPlanFloat64Ptr 返回 float64 指针（命名避免与带 build tag 的
// channel_handler_test.go float64Ptr 冲突）。
func pricingPlanFloat64Ptr(v float64) *float64 { return &v }
