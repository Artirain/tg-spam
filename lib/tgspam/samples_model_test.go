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

func TestSamplesModel_Reset(t *testing.T) {
	m := NewSamplesModel()
	m.lock.Lock()
	m.tokSpam = []map[string]int{{"buy": 1}}
	m.stops = []string{"viagra"}
	m.excluded = map[string]struct{}{"the": {}}
	m.lock.Unlock()

	m.Reset()

	assert.Equal(t, 0, m.tokenizedSpamLen())
	assert.Equal(t, 0, m.stopWordsLen())
	assert.Equal(t, 0, m.excludedTokensLen())
	assert.Equal(t, 0, m.classifierAllDocs())
}

func TestSamplesModel_Tokenize(t *testing.T) {
	m := NewSamplesModel()
	m.lock.Lock()
	m.excluded = map[string]struct{}{"the": {}, "and": {}}
	m.lock.Unlock()

	got := m.tokenize("the quick brown fox and the lazy dog")
	assert.NotContains(t, got, "the")
	assert.NotContains(t, got, "and")
	assert.Contains(t, got, "quick")
	assert.Contains(t, got, "brown")
	assert.Contains(t, got, "fox")
	assert.Contains(t, got, "lazy")
	assert.Contains(t, got, "dog")
}
