//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizePricingPlanInput(t *testing.T) {
	plan := normalizePricingPlanInput(PricingPlanInput{
		Name:        "  pro  ",
		Title:       " Pro Plan ",
		Description: "  ",
		Status:      "  DISABLED ",
		IsPublic:    true,
		SortOrder:   5,
	})
	require.Equal(t, "pro", plan.Name)
	require.Equal(t, "Pro Plan", plan.Title)
	require.Equal(t, "", plan.Description)
	require.Equal(t, PricingPlanStatusDisabled, plan.Status)

	// 未知状态回落到 active
	fallback := normalizePricingPlanInput(PricingPlanInput{Name: "x", Status: "weird"})
	require.Equal(t, PricingPlanStatusActive, fallback.Status)
}

func TestNormalizePricingPlanModelInput(t *testing.T) {
	m := normalizePricingPlanModelInput(PricingPlanModelInput{
		PublicModel:                "  gpt-5 ",
		Protocol:                   " MESSAGES ",
		Direct:                     true,
		AllowCompatibilityFallback: true,
		Priority:                   10,
		Enabled:                    true,
	})
	require.Equal(t, "gpt-5", m.PublicModel)
	require.Equal(t, PricingPlanProtocolMessages, m.Protocol)
	require.True(t, m.Direct)
	require.True(t, m.AllowCompatibilityFallback)
	require.Nil(t, m.Pricing)
	// upstream_model 为空时回填为 public_model
	require.Equal(t, "gpt-5", m.UpstreamModel)

	m2 := normalizePricingPlanModelInput(PricingPlanModelInput{PublicModel: "gpt-5", Protocol: "weird"})
	require.Equal(t, "weird", m2.Protocol)
	require.False(t, isPricingPlanProtocol(m2.Protocol))

	m3 := normalizePricingPlanModelInput(PricingPlanModelInput{
		PublicModel:   "claude-sonnet-4-6",
		Protocol:      PricingPlanProtocolResponses,
		UpstreamModel: " claude-sonnet-4-6 ",
	})
	require.Equal(t, PricingPlanProtocolResponses, m3.Protocol)
	require.Equal(t, "claude-sonnet-4-6", m3.UpstreamModel)
}

func TestNormalizePricingPlanRouteInput(t *testing.T) {
	r := normalizePricingPlanRouteInput(PricingPlanRouteInput{
		GroupID:  42,
		Priority: 30,
		Enabled:  true,
	})
	require.Equal(t, int64(42), r.GroupID)
	require.Equal(t, 30, r.Priority)
	require.True(t, r.Enabled)
}
