package service

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccount_GetUpstreamCostFactor_DefaultsToOneWhenMissing(t *testing.T) {
	require.Equal(t, 1.0, (&Account{}).GetUpstreamCostFactor())
	require.Equal(t, 1.0, (&Account{Extra: map[string]any{}}).GetUpstreamCostFactor())
}

func TestAccount_GetUpstreamCostFactor_ParsesPositiveValues(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want float64
	}{
		{name: "float64", raw: 0.75, want: 0.75},
		{name: "int", raw: 2, want: 2},
		{name: "json number", raw: json.Number("1.25"), want: 1.25},
		{name: "string", raw: " 1.5 ", want: 1.5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Extra: map[string]any{"upstream_cost_factor": tc.raw}}
			require.InDelta(t, tc.want, account.GetUpstreamCostFactor(), 1e-12)
		})
	}
}

func TestAccount_GetUpstreamCostFactor_InvalidValuesFallBackToOne(t *testing.T) {
	cases := []any{0, -1, "", "invalid", json.Number("bad"), false, math.NaN(), math.Inf(1), math.Inf(-1), "NaN", "+Inf"}
	for _, raw := range cases {
		account := &Account{Extra: map[string]any{"upstream_cost_factor": raw}}
		require.Equal(t, 1.0, account.GetUpstreamCostFactor())
	}
}
