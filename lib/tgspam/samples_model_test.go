package tgspam

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/lib/tgspam/mocks"
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

func TestSamplesModel_ConcurrentReadWrite(t *testing.T) {
	m := NewSamplesModel()
	cfg := Config{MinMsgLen: 1}
	d1 := NewDetectorWithModel(cfg, m)
	d2 := NewDetectorWithModel(cfg, m)

	// updater is required for UpdateSpam to actually update the shared model
	d1.WithSpamUpdater(&mocks.SampleUpdaterMock{AppendFunc: func(msg string) error { return nil }})

	// 16 goroutines hammer UpdateSpam on d1 while 16 read model state via d2.
	const N = 16
	var wg sync.WaitGroup
	wg.Add(2 * N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			_ = d1.UpdateSpam(fmt.Sprintf("spam sample %d", i))
		}(i)
		go func() {
			defer wg.Done()
			_ = d2.model.classifierReady()
			_ = d2.model.tokenizedSpamLen()
		}()
	}
	wg.Wait()

	assert.GreaterOrEqual(t, m.classifierAllDocs(), N)
}
