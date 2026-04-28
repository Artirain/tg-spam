package tgspam

import (
	"strings"
	"sync"
)

// SamplesModel holds sample-derived state (classifier, tokenized spam samples,
// stop words, excluded tokens) that is shared by reference across multiple
// Detector instances so updates from any chat propagate everywhere.
//
// The embedded RWMutex protects all fields. Lock-order invariant: Detector.lock
// is always acquired before SamplesModel.lock; SamplesModel methods never call
// back into Detector. Violating this order risks deadlock.
type SamplesModel struct {
	cls      classifier
	tokSpam  []map[string]int
	stops    []string
	excluded map[string]struct{}
	lock     sync.RWMutex
}

// NewSamplesModel returns a SamplesModel with an empty classifier, no tokenized
// spam samples, no stop words, and no excluded tokens.
func NewSamplesModel() *SamplesModel {
	return &SamplesModel{
		cls:      newClassifier(),
		tokSpam:  []map[string]int{},
		excluded: map[string]struct{}{},
	}
}

// classifierAllDocs returns the total number of documents the classifier has
// learned across all classes.
func (m *SamplesModel) classifierAllDocs() int {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.cls.nAllDocument
}

// classifierReady reports whether the classifier has at least one ham and one
// spam document and so is ready to classify.
func (m *SamplesModel) classifierReady() bool {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.cls.nAllDocument > 0 && m.cls.nDocumentByClass[ClassHam] > 0 && m.cls.nDocumentByClass[ClassSpam] > 0
}

// tokenizedSpamLen returns the number of tokenized spam samples.
func (m *SamplesModel) tokenizedSpamLen() int {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return len(m.tokSpam)
}

// stopWordsLen returns the number of configured stop words.
func (m *SamplesModel) stopWordsLen() int {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return len(m.stops)
}

// excludedTokensLen returns the number of excluded tokens.
func (m *SamplesModel) excludedTokensLen() int {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return len(m.excluded)
}

// Reset clears all derived state (classifier, tokenized samples, stop words, excluded tokens).
// Holds the write lock for the duration.
//
// In multi-chat mode where multiple Detectors share one SamplesModel by reference,
// Reset wipes state for all of them. Coordinate accordingly at the caller layer.
func (m *SamplesModel) Reset() {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.cls.reset()
	m.tokSpam = []map[string]int{}
	m.stops = []string{}
	m.excluded = map[string]struct{}{}
}

// tokenize splits a string into a token map, excluding tokens listed in the model's
// excluded set. Takes its own read lock; safe to call from any context except inside
// a write-locked section (use tokenizeUnlocked for that).
func (m *SamplesModel) tokenize(inp string) map[string]int {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.tokenizeUnlocked(inp)
}

// tokenizeUnlocked is the lock-free variant. Caller MUST already hold m.lock
// (read or write); using it from outside a critical section races with mutators.
func (m *SamplesModel) tokenizeUnlocked(inp string) map[string]int {
	isExcludedToken := func(token string) bool {
		if _, ok := m.excluded[strings.ToLower(token)]; ok {
			return true
		}
		return false
	}

	tokenFrequency := make(map[string]int)
	for token := range strings.FieldsSeq(inp) {
		if isExcludedToken(token) {
			continue
		}
		token = cleanEmoji(token)
		token = strings.Trim(token, ".,!?-:;()#")
		token = strings.ToLower(token)
		if len([]rune(token)) < 3 {
			continue
		}
		tokenFrequency[strings.ToLower(token)]++
	}
	return tokenFrequency
}
