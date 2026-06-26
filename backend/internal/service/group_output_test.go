package service

import "testing"

func TestGroup_EnforceModelsListActive(t *testing.T) {
	cases := []struct {
		name    string
		enforce bool
		models  []string
		want    bool
	}{
		{"off empty", false, nil, false},
		{"off with models", false, []string{"claude-sonnet-4"}, false},
		{"on empty -> not active (avoid locking all)", true, nil, false},
		{"on with models", true, []string{"claude-sonnet-4"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := &Group{EnforceModelsList: c.enforce, ModelsListConfig: GroupModelsListConfig{Models: c.models}}
			if got := g.EnforceModelsListActive(); got != c.want {
				t.Errorf("EnforceModelsListActive()=%v want %v", got, c.want)
			}
		})
	}
	var nilG *Group
	if nilG.EnforceModelsListActive() {
		t.Error("nil group should be false")
	}
}

func TestGroup_AllowsOutputModel(t *testing.T) {
	g := &Group{ModelsListConfig: GroupModelsListConfig{Models: []string{"claude-sonnet-4", "claude-opus-*", " gpt-4o "}}}
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-sonnet-4", true},          // exact
		{"claude-opus-4-20250514", true},   // wildcard prefix
		{"claude-opus-", true},             // wildcard boundary
		{"claude-haiku-4", false},          // not listed
		{"gpt-4o", true},                   // pattern trimmed
		{" claude-sonnet-4 ", true},        // input trimmed
		{"claude-sonnet", false},           // partial, no wildcard
		{"", false},
	}
	for _, c := range cases {
		if got := g.AllowsOutputModel(c.model); got != c.want {
			t.Errorf("AllowsOutputModel(%q)=%v want %v", c.model, got, c.want)
		}
	}
	var nilG *Group
	if nilG.AllowsOutputModel("x") {
		t.Error("nil group should not allow")
	}
}

func TestGroup_ResolveModelAlias(t *testing.T) {
	g := &Group{ModelAliasMappings: GroupModelAliasMappings{
		"claude-sonnet-4": "real-sonnet-in-pool",
		"gpt-4o":          "",
	}}
	cases := []struct {
		in   string
		want string
	}{
		{"claude-sonnet-4", "real-sonnet-in-pool"}, // mapped
		{" claude-sonnet-4 ", "real-sonnet-in-pool"}, // trimmed key match
		{"gpt-4o", "gpt-4o"},                        // empty value -> original
		{"unmapped-model", "unmapped-model"},        // miss -> original
	}
	for _, c := range cases {
		if got := g.ResolveModelAlias(c.in); got != c.want {
			t.Errorf("ResolveModelAlias(%q)=%q want %q", c.in, got, c.want)
		}
	}
	// nil / empty mappings -> passthrough
	empty := &Group{}
	if got := empty.ResolveModelAlias("x"); got != "x" {
		t.Errorf("empty mappings should passthrough, got %q", got)
	}
	var nilG *Group
	if got := nilG.ResolveModelAlias("x"); got != "x" {
		t.Errorf("nil group should passthrough, got %q", got)
	}
}

func TestNormalizeGroupModelAliasMappings(t *testing.T) {
	in := GroupModelAliasMappings{
		" claude-sonnet-4 ": " real-sonnet ",
		"empty-val":         "  ",
		"  ":                "orphan",
		"gpt-4o":            "gpt-4o-2024",
	}
	out := normalizeGroupModelAliasMappings(in)
	if len(out) != 2 {
		t.Fatalf("want 2 valid entries, got %d: %#v", len(out), out)
	}
	if out["claude-sonnet-4"] != "real-sonnet" {
		t.Errorf("key/value not trimmed: %#v", out)
	}
	if out["gpt-4o"] != "gpt-4o-2024" {
		t.Errorf("missing gpt-4o entry: %#v", out)
	}
	if _, ok := out["empty-val"]; ok {
		t.Error("empty value should be dropped")
	}
	// empty input -> empty (non-nil) map
	if got := normalizeGroupModelAliasMappings(nil); got == nil || len(got) != 0 {
		t.Errorf("nil input should return empty non-nil map, got %#v", got)
	}
}
