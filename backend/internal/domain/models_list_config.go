package domain

// GroupModelsListConfig controls the optional custom /v1/models response list.
type GroupModelsListConfig struct {
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models,omitempty"`
}

// GroupModelAliasMappings maps an externally-advertised model name to the
// pool-internal real model name. Key = external unified name (what the client
// requests), value = the real model resolved before account matching.
// Lets the same external name map to different real models across pools.
type GroupModelAliasMappings map[string]string
