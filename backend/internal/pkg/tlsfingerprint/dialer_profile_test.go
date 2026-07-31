package tlsfingerprint

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeCode21208CompatibilityProfileIsExplicitlyProvisional(t *testing.T) {
	first := ClaudeCode21208CompatibilityProfile()
	second := ClaudeCode21208CompatibilityProfile()

	require.Equal(t, ClaudeCode21208CompatibilityProfileName, first.Name)
	require.Contains(t, first.Name, "provisional")
	require.NotSame(t, first, second)
}
