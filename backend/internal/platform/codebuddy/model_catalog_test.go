package codebuddy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodeBuddyModelCatalog_Loads(t *testing.T) {
	catalog, keys, err := CodeBuddyModelCatalog()
	require.NoError(t, err)
	assert.NotEmpty(t, catalog)
	assert.True(t, sort_ascending(keys))
	entry, ok := catalog["glm-5.2"]
	require.True(t, ok, "种子应含 glm-5.2")
	assert.Positive(t, entry.ContextLength)
	assert.Positive(t, entry.MaxOutputTokens)
}

func sort_ascending(keys []string) bool {
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			return false
		}
	}
	return true
}
