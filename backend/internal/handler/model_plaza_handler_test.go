//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// plazaSettingRepo 模型广场运行开关的 SettingRepository 桩（GetMultiple 逐键返回）。
type plazaSettingRepo struct {
	values map[string]string
}

func (r *plazaSettingRepo) Get(context.Context, string) (*service.Setting, error) { return nil, nil }
func (r *plazaSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	return r.values[key], nil
}
func (r *plazaSettingRepo) Set(context.Context, string, string) error { return nil }
func (r *plazaSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = r.values[k]
	}
	return out, nil
}
func (r *plazaSettingRepo) SetMultiple(context.Context, map[string]string) error { return nil }
func (r *plazaSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *plazaSettingRepo) Delete(context.Context, string) error { return nil }

func newPlazaSettingService(t *testing.T, values map[string]string) *service.SettingService {
	t.Helper()
	return service.NewSettingService(&plazaSettingRepo{values: values}, &config.Config{})
}

// stubPricingPlanRepo 模型广场测试用的 PricingPlanRepository 桩：
// 只实现公开产品读取，其余方法返回零值。
type stubPricingPlanRepo struct {
	listPublicProductsFn func(ctx context.Context) ([]service.PricingPlanProduct, error)
}

func (s *stubPricingPlanRepo) ListPlans(context.Context, bool) ([]service.PricingPlan, error) {
	return nil, nil
}
func (s *stubPricingPlanRepo) ListPublicProducts(ctx context.Context) ([]service.PricingPlanProduct, error) {
	if s.listPublicProductsFn == nil {
		return nil, nil
	}
	return s.listPublicProductsFn(ctx)
}
func (s *stubPricingPlanRepo) GetPlanByID(context.Context, int64) (*service.PricingPlan, error) {
	return nil, service.ErrPricingPlanNotFound
}
func (s *stubPricingPlanRepo) GetPlanByName(context.Context, string) (*service.PricingPlan, error) {
	return nil, service.ErrPricingPlanNotFound
}
func (s *stubPricingPlanRepo) ListModelsByPlan(context.Context, int64, bool) ([]service.PricingPlanModel, error) {
	return nil, nil
}
func (s *stubPricingPlanRepo) ListRoutesByPlan(context.Context, int64, bool) ([]service.PricingPlanRoute, error) {
	return nil, nil
}
func (s *stubPricingPlanRepo) CreatePlan(context.Context, *service.PricingPlan, []service.PricingPlanModel, []service.PricingPlanRoute) error {
	return nil
}
func (s *stubPricingPlanRepo) UpdatePlan(context.Context, *service.PricingPlan, []service.PricingPlanModel, []service.PricingPlanRoute) error {
	return nil
}
func (s *stubPricingPlanRepo) ReplaceModels(context.Context, int64, []service.PricingPlanModel) error {
	return nil
}
func (s *stubPricingPlanRepo) ReplaceRoutes(context.Context, int64, []service.PricingPlanRoute) error {
	return nil
}
func (s *stubPricingPlanRepo) DeletePlan(context.Context, int64) error { return nil }

func plazaCatalogProducts() []service.PricingPlanProduct {
	inputPrice := 2e-6
	outputPrice := 4e-6
	maxTokens := 128000
	perRequest := 0.2
	return []service.PricingPlanProduct{
		{
			Plan: service.PricingPlan{Name: "standard", Title: "Standard", Description: "Standard catalog"},
			Models: []service.PricingPlanModel{
				{
					PublicModel: "gpt-5.6",
					Protocol:    service.PricingPlanProtocolChatCompletions,
					Enabled:     true,
					Direct:      false,
					Pricing: &service.ChannelModelPricing{
						BillingMode: service.BillingModeToken,
						InputPrice:  &inputPrice,
						OutputPrice: &outputPrice,
						Intervals: []service.PricingInterval{{
							MinTokens:  1,
							MaxTokens:  &maxTokens,
							TierLabel:  "128k",
							InputPrice: &inputPrice,
						}},
					},
				},
				{
					PublicModel: "gpt-5.6",
					Protocol:    service.PricingPlanProtocolResponses,
					Enabled:     true,
					Direct:      true,
					Pricing:     nil,
				},
				{
					PublicModel: "claude-sonnet",
					Protocol:    service.PricingPlanProtocolMessages,
					Enabled:     true,
					Direct:      true,
					Pricing: &service.ChannelModelPricing{
						BillingMode:     service.BillingModeImage,
						PerRequestPrice: &perRequest,
					},
				},
			},
		},
		{
			Plan: service.PricingPlan{Name: "pro", Title: "Pro"},
			Models: []service.PricingPlanModel{
				{PublicModel: "gpt-5.6", Protocol: service.PricingPlanProtocolChatCompletions, Enabled: true},
			},
		},
	}
}

func plazaHandlerGet(t *testing.T, handler *ModelPlazaHandler) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/model-plaza", nil)
	handler.Get(c)

	raw := w.Body.Bytes()
	var resp map[string]any
	require.NoError(t, json.Unmarshal(raw, &resp))
	return w.Code, resp
}

func TestModelPlazaHandler_NilSettingServiceFailsClosed404(t *testing.T) {
	// settingService == nil → fail-closed 404，不触达仓储。
	called := false
	h := &ModelPlazaHandler{
		pricingPlanRepo: &stubPricingPlanRepo{
			listPublicProductsFn: func(context.Context) ([]service.PricingPlanProduct, error) {
				called = true
				return nil, nil
			},
		},
	}
	code, _ := plazaHandlerGet(t, h)
	require.Equal(t, http.StatusNotFound, code)
	require.False(t, called, "disabled 时不得读取公开产品")
}

func TestModelPlazaHandler_DisabledRuntimeFails404(t *testing.T) {
	// 开关关闭 → 404（opt-in 默认关闭）。
	h := &ModelPlazaHandler{
		pricingPlanRepo: &stubPricingPlanRepo{},
		settingService:  newPlazaSettingService(t, map[string]string{service.SettingKeyModelPlazaEnabled: "false"}),
	}
	code, _ := plazaHandlerGet(t, h)
	require.Equal(t, http.StatusNotFound, code)
}

func TestModelPlazaHandler_RequireAuthAnonymous401(t *testing.T) {
	// require_auth 开启且匿名 → 401；不触达仓储。
	called := false
	h := &ModelPlazaHandler{
		pricingPlanRepo: &stubPricingPlanRepo{
			listPublicProductsFn: func(context.Context) ([]service.PricingPlanProduct, error) {
				called = true
				return nil, nil
			},
		},
		settingService: newPlazaSettingService(t, map[string]string{
			service.SettingKeyModelPlazaEnabled:     "true",
			service.SettingKeyModelPlazaRequireAuth: "true",
		}),
	}
	code, _ := plazaHandlerGet(t, h)
	require.Equal(t, http.StatusUnauthorized, code)
	require.False(t, called, "401 时不得读取公开产品")
}

func TestModelPlazaHandler_GetStrictAllowlistAndGrouping(t *testing.T) {
	// 端到端：公开产品 → 目录 DTO。逐层严格白名单断言 + 按公开模型分组。
	h := &ModelPlazaHandler{
		pricingPlanRepo: &stubPricingPlanRepo{
			listPublicProductsFn: func(context.Context) ([]service.PricingPlanProduct, error) {
				return plazaCatalogProducts(), nil
			},
		},
		settingService: newPlazaSettingService(t, map[string]string{
			service.SettingKeyModelPlazaEnabled:     "true",
			service.SettingKeyModelPlazaDescription: "Prices are public.",
		}),
	}
	code, resp := plazaHandlerGet(t, h)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, map[string]any{
		"description": "Prices are public.",
		"plans": []any{
			map[string]any{
				"code": "standard", "name": "Standard", "description": "Standard catalog",
				"models": []any{
					map[string]any{
						"id": "gpt-5.6", "display_name": "gpt-5.6",
						"protocols": []any{
							map[string]any{
								"protocol": "chat_completions", "direct": false, "billing_mode": "token",
								"pricing": map[string]any{
									"input_price":        2e-6,
									"output_price":       4e-6,
									"cache_write_price":  nil,
									"cache_read_price":   nil,
									"image_input_price":  nil,
									"image_output_price": nil,
									"per_request_price":  nil,
									"intervals": []any{map[string]any{
										"min_tokens": float64(1), "max_tokens": float64(128000), "tier_label": "128k",
										"input_price": 2e-6, "output_price": nil,
										"cache_write_price": nil, "cache_read_price": nil,
										"per_request_price": nil,
									}},
								},
							},
							map[string]any{
								"protocol": "responses", "direct": true, "billing_mode": "token",
								"pricing": nil,
							},
						},
					},
					map[string]any{
						"id": "claude-sonnet", "display_name": "claude-sonnet",
						"protocols": []any{map[string]any{
							"protocol": "messages", "direct": true, "billing_mode": "image",
							"pricing": map[string]any{
								"input_price": nil, "output_price": nil,
								"cache_write_price": nil, "cache_read_price": nil,
								"image_input_price": nil, "image_output_price": nil,
								"per_request_price": 0.2, "intervals": []any{},
							},
						}},
					},
				},
			},
			map[string]any{
				"code": "pro", "name": "Pro", "description": "",
				"models": []any{map[string]any{
					"id": "gpt-5.6", "display_name": "gpt-5.6",
					"protocols": []any{map[string]any{
						"protocol": "chat_completions", "direct": false, "billing_mode": "token",
						"pricing": nil,
					}},
				}},
			},
		},
	}, resp["data"])
}

func TestModelPlazaHandler_GetNoInternalFieldLeak(t *testing.T) {
	// 深扫整个响应：任何内部字段名（分组/ID/路由/账号/成本/倍率/健康/平台/官方价）
	// 都不得作为键出现。
	h := &ModelPlazaHandler{
		pricingPlanRepo: &stubPricingPlanRepo{
			listPublicProductsFn: func(context.Context) ([]service.PricingPlanProduct, error) {
				return plazaCatalogProducts(), nil
			},
		},
		settingService: newPlazaSettingService(t, map[string]string{service.SettingKeyModelPlazaEnabled: "true"}),
	}
	_, resp := plazaHandlerGet(t, h)

	forbidden := []string{
		"groups", "group_id", "plan_id", "routes", "accounts", "platform",
		"rate_multiplier", "user_rate_multiplier", "subscription_type", "is_exclusive",
		"upstream_model", "upstream_cost", "official_pricing", "health", "base_url",
		"direct_pricing", "fast_multiplier", "flex_multiplier", "time_pricing",
		"sort_order", "created_at", "updated_at", "status", "enabled",
	}
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			for k, child := range node {
				for _, f := range forbidden {
					require.NotEqualf(t, f, k, "internal field %q leaked into model plaza response", k)
				}
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(resp)
}

func TestModelPlazaHandler_RepoErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	h := &ModelPlazaHandler{
		pricingPlanRepo: &stubPricingPlanRepo{
			listPublicProductsFn: func(context.Context) ([]service.PricingPlanProduct, error) {
				return nil, sentinel
			},
		},
		settingService: newPlazaSettingService(t, map[string]string{service.SettingKeyModelPlazaEnabled: "true"}),
	}
	code, _ := plazaHandlerGet(t, h)
	require.Equal(t, http.StatusInternalServerError, code)
}

func TestModelPlazaHandler_NilPricingPlanRepoFailsClosed404(t *testing.T) {
	// 仓储未注入 → fail-closed 404，与 settingService nil 语义一致。
	h := &ModelPlazaHandler{
		settingService: newPlazaSettingService(t, map[string]string{service.SettingKeyModelPlazaEnabled: "true"}),
	}
	code, _ := plazaHandlerGet(t, h)
	require.Equal(t, http.StatusNotFound, code)
}

func TestModelPlazaHandler_EmptyCatalogReturnsEmptyPlans(t *testing.T) {
	// 无公开产品时返回空 plans（不是 null），description 沿用运行开关配置。
	h := &ModelPlazaHandler{
		pricingPlanRepo: &stubPricingPlanRepo{},
		settingService:  newPlazaSettingService(t, map[string]string{service.SettingKeyModelPlazaEnabled: "true"}),
	}
	code, resp := plazaHandlerGet(t, h)
	require.Equal(t, http.StatusOK, code)
	data := resp["data"].(map[string]any)
	require.Equal(t, "", data["description"])
	require.Empty(t, data["plans"])
}

func TestToModelPlazaProtocol_BillingModeAndDirect(t *testing.T) {
	// 计费模式：定价缺失回落 token；带定价按定价的 BillingMode；Direct 透传。
	protocol := toModelPlazaProtocol(&service.PricingPlanModel{
		PublicModel: "x", Protocol: service.PricingPlanProtocolChatCompletions, Direct: true,
	})
	require.Equal(t, "chat_completions", protocol.Protocol)
	require.True(t, protocol.Direct)
	require.Equal(t, "token", protocol.BillingMode)
	require.Nil(t, protocol.Pricing)

	priced := toModelPlazaProtocol(&service.PricingPlanModel{
		Protocol: service.PricingPlanProtocolMessages,
		Pricing:  &service.ChannelModelPricing{BillingMode: service.BillingModeImage},
	})
	require.Equal(t, "image", priced.BillingMode)

	explicit := toModelPlazaProtocol(&service.PricingPlanModel{
		Protocol: service.PricingPlanProtocolResponses,
		Pricing:  &service.ChannelModelPricing{BillingMode: service.BillingModeToken},
	})
	require.Equal(t, "token", explicit.BillingMode)
}

func TestGroupModelPlazaProtocols_GroupsByPublicModelSkipsDisabled(t *testing.T) {
	rows := []service.PricingPlanModel{
		{PublicModel: "a", Protocol: service.PricingPlanProtocolChatCompletions, Enabled: true},
		{PublicModel: "b", Protocol: service.PricingPlanProtocolMessages, Enabled: false},
		{PublicModel: "a", Protocol: service.PricingPlanProtocolResponses, Enabled: true},
	}
	models := groupModelPlazaProtocols(rows)
	require.Len(t, models, 1)
	require.Equal(t, "a", models[0].ID)
	require.Equal(t, "a", models[0].DisplayName)
	require.Len(t, models[0].Protocols, 2, "同模型多协议行合并，保持仓库顺序")
	require.Equal(t, service.PricingPlanProtocolChatCompletions, models[0].Protocols[0].Protocol)
	require.Equal(t, service.PricingPlanProtocolResponses, models[0].Protocols[1].Protocol)
}

func TestToModelPlazaPricing_NilPassthrough(t *testing.T) {
	require.Nil(t, toModelPlazaPricing(nil))
}
