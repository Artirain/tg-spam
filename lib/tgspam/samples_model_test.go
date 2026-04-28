package tgspam

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSamplesModel(t *testing.T) {
	m := NewSamplesModel()
	require.NotNil(t, m)
	assert.Equal(t, 0, m.classifierAllDocs())
	assert.Equal(t, 0, m.tokenizedSpamLen())
	assert.Equal(t, 0, m.stopWordsLen())
	assert.Equal(t, 0, m.excludedTokensLen())
}
