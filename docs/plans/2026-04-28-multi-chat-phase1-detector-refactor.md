# Multi-Chat Phase 1 — Detector Refactor Implementation Plan (v1.1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refactor `lib/tgspam.Detector` so that the shared sample-derived model state (classifier + tokenized spam + stop words + excluded tokens) lives in a separate `SamplesModel` struct that can be **shared by reference** across multiple `Detector` instances. After this phase, behavior is unchanged for single-Detector callers; multi-Detector use is enabled but not yet wired into `app/`.

**Architecture:**
- Introduce `SamplesModel` as a mutex-guarded bundle of `classifier`, `tokenizedSpam`, `stopWords`, `excludedTokens` and a few related methods (`Reset`, `tokenize`, `LoadSamples` worker).
- `Detector` holds `*SamplesModel` (pointer). Per-Detector state stays on `Detector`: `approvedUsers`, `duplicateDetector`, `reactionDetector`, `hamHistory`, `spamHistory`, `userStorage`, LLM clients, lua engine, meta checks.
- `NewDetector` continues to allocate a fresh private `SamplesModel` — preserving today's API. New `NewDetectorWithModel(cfg, model)` constructor is the entry point for sharing.
- **Lock-order invariant:** `Detector.lock` is **always acquired before** `SamplesModel.lock`. SamplesModel methods never call back into Detector. Any method that needs both takes Detector.lock first, then SamplesModel.lock — never the reverse.
- **Reset semantics warning:** calling `Detector.Reset()` on a Detector that shares its `SamplesModel` with other Detectors **wipes the shared state for all of them**. Callers in multi-chat mode (Phase 2+) must coordinate Reset with this in mind. Single-chat callers are unaffected.

**Tech Stack:** Go 1.24+, `lib/tgspam` package, `github.com/stretchr/testify`, `go test -race ./...`, `golangci-lint run`.

**Reference spec:** `docs/plans/2026-04-28-multi-chat-design.md` §SpamFilter / Option A.

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `lib/tgspam/samples_model.go` | Create | New `SamplesModel` bundling classifier + tokenizedSpam + stopWords + excludedTokens behind a `sync.RWMutex`. Methods: `NewSamplesModel`, `Reset`, `tokenize`, plus internal helpers reachable from Detector |
| `lib/tgspam/samples_model_test.go` | Create | Unit tests for `SamplesModel` construction, `Reset`, concurrent safety, `tokenize` correctness |
| `lib/tgspam/detector.go` | Modify | Replace four fields with `model *SamplesModel`. All read/write of these fields goes through the shared model. `NewDetector` preserves current signature; new `NewDetectorWithModel(cfg, model)` added |
| `lib/tgspam/detector_test.go` | Modify | Update ~24 direct field accesses (`d.classifier.*`, `d.tokenizedSpam`, `d.excludedTokens`, `d.stopWords`) to read through the model. Update `BenchmarkTokenize` if it uses `Detector{excludedTokens:...}` literal. Add new tests for shared/isolated behavior |

The plan deliberately keeps the public `Detector` API stable for `app/main.go`, `app/bot/spam.go`, and external CLI tools — they need no changes.

---

## Pre-flight

- [ ] **Pre-flight 1: confirm clean working tree**

```bash
git status --short
```

Expected: empty output (or only untracked files unrelated to this plan). Stash anything else.

- [ ] **Pre-flight 2: baseline test run**

```bash
go test -race ./lib/tgspam/... -count=1
```

Expected: all tests pass. Capture the pass count for later comparison.

- [ ] **Pre-flight 3: baseline lint**

```bash
golangci-lint run ./lib/tgspam/...
```

Expected: no errors.

- [ ] **Pre-flight 4: confirm benchmark exists for tokenize**

```bash
grep -rn "BenchmarkTokenize\|benchmark.*okenize" lib/tgspam/
```

Capture the location — Task 6 will need to update its `Detector{...}` literal if it uses the removed fields.

---

## Task 1: Create `SamplesModel` skeleton with `tokenize` method

`tokenize` lives on `SamplesModel`, not `Detector`. This avoids the fragile "caller must hold lock" comment contract — `tokenize` takes its own RLock when callers don't already hold one, and there's a `tokenizeUnlocked` variant for callers (like `LoadSamples`) that hold the write lock and would deadlock on reentry.

**Files:**
- Create: `lib/tgspam/samples_model.go`
- Create: `lib/tgspam/samples_model_test.go`

- [ ] **Step 1: Write failing test for `NewSamplesModel`**

Create `lib/tgspam/samples_model_test.go`:

```go
package tgspam

import (
	"sync"
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

var _ = sync.RWMutex{} // imports stay used after later tasks add concurrency tests
```

- [ ] **Step 2: Run, expect compile failure**

```bash
go test -race ./lib/tgspam/ -run TestNewSamplesModel -v
```

Expected: `undefined: NewSamplesModel`.

- [ ] **Step 3: Implement `SamplesModel`**

Create `lib/tgspam/samples_model.go`:

```go
package tgspam

import "sync"

// SamplesModel bundles the in-memory state derived from spam/ham samples and stop words.
// Multiple Detector instances may share a single *SamplesModel by reference so that
// updates from any chat (UpdateSpam/UpdateHam/RemoveSpam/RemoveHam/LoadSamples) become
// visible to all detectors immediately. The internal mutex protects all fields.
//
// Lock-order invariant: callers MUST acquire Detector.lock before SamplesModel.lock when
// both are needed. SamplesModel never calls back into Detector.
type SamplesModel struct {
	cls      classifier
	tokSpam  []map[string]int
	stops    []string
	excluded map[string]struct{}
	lock     sync.RWMutex
}

// NewSamplesModel creates an empty model with a freshly initialised classifier.
func NewSamplesModel() *SamplesModel {
	return &SamplesModel{
		cls:      newClassifier(),
		tokSpam:  []map[string]int{},
		excluded: map[string]struct{}{},
	}
}

// classifierAllDocs returns the classifier's total learned-document count under read lock.
func (m *SamplesModel) classifierAllDocs() int {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.cls.nAllDocument
}

// classifierReady reports whether the classifier has documents in both ham and spam classes.
func (m *SamplesModel) classifierReady() bool {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.cls.nAllDocument > 0 &&
		m.cls.nDocumentByClass["ham"] > 0 && m.cls.nDocumentByClass["spam"] > 0
}

// tokenizedSpamLen returns the number of tokenized spam samples loaded.
func (m *SamplesModel) tokenizedSpamLen() int {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return len(m.tokSpam)
}

// stopWordsLen returns the number of loaded stop words.
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
```

- [ ] **Step 4: Run, expect pass**

```bash
go test -race ./lib/tgspam/ -run TestNewSamplesModel -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add lib/tgspam/samples_model.go lib/tgspam/samples_model_test.go
git commit -m "Add SamplesModel skeleton with classifier+tokens+stops+excluded"
```

---

## Task 2: Add `Reset` and `tokenize` methods to `SamplesModel`

`tokenize` is moved off `Detector` because it reads `excluded` and we want a clean lock contract. Two flavours:

- `tokenize(s)` — public, takes its own RLock. Use from any caller.
- `tokenizeUnlocked(s)` — private, **assumes the write lock is already held**. Use only from inside `LoadSamples` / `updateSample` / `removeSample`. The "Unlocked" suffix is a convention warning future readers.

**Files:**
- Modify: `lib/tgspam/samples_model.go`
- Modify: `lib/tgspam/samples_model_test.go`

- [ ] **Step 1: Write failing test for `Reset`**

Append to `lib/tgspam/samples_model_test.go`:

```go
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
```

- [ ] **Step 2: Write failing test for `tokenize`**

Append:

```go
func TestSamplesModel_Tokenize(t *testing.T) {
	m := NewSamplesModel()
	// excluded tokens should be filtered out of the tokenize result
	m.lock.Lock()
	m.excluded = map[string]struct{}{"the": {}, "and": {}}
	m.lock.Unlock()

	got := m.tokenize("the quick brown fox and the lazy dog")
	// "the" and "and" excluded; "quick", "brown", "fox", "lazy", "dog" remain
	assert.NotContains(t, got, "the")
	assert.NotContains(t, got, "and")
	assert.Contains(t, got, "quick")
	assert.Contains(t, got, "brown")
	assert.Contains(t, got, "fox")
	assert.Contains(t, got, "lazy")
	assert.Contains(t, got, "dog")
}
```

- [ ] **Step 3: Run, expect compile/test failure**

```bash
go test -race ./lib/tgspam/ -run "TestSamplesModel_Reset|TestSamplesModel_Tokenize" -v
```

Expected: `Reset undefined`, `tokenize undefined`.

- [ ] **Step 4: Implement `Reset` and `tokenize`/`tokenizeUnlocked`**

The current `tokenize` body in `lib/tgspam/detector.go` (around line 850) is the source of truth. Read it carefully — do **not** invent a new tokenizer. Then port that exact logic into two methods on `SamplesModel`.

Append to `lib/tgspam/samples_model.go`:

```go
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
// The body is the same algorithm as the original Detector.tokenize.
func (m *SamplesModel) tokenizeUnlocked(inp string) map[string]int {
	// Port the exact body of the existing detector.tokenize here.
	// Reference: lib/tgspam/detector.go (the tokenize method) — copy line-for-line,
	// replacing `d.excludedTokens` with `m.excluded`.
	// Do not invent or simplify; preserve any rune handling, case folding, and
	// punctuation stripping exactly.
	res := map[string]int{}
	// (paste the loop body verbatim from current detector.go tokenize, with the
	//  excluded-token check using m.excluded)
	_ = inp
	return res
}
```

**Important:** the placeholder body above is INSUFFICIENT. The implementer must read `lib/tgspam/detector.go` lines around 850 and copy the real tokenize body verbatim into `tokenizeUnlocked`, swapping `d.excludedTokens` for `m.excluded`. The TestSamplesModel_Tokenize test verifies correctness; it WILL fail with the placeholder above.

- [ ] **Step 5: Run tests, iterate until both pass**

```bash
go test -race ./lib/tgspam/ -run "TestSamplesModel_Reset|TestSamplesModel_Tokenize" -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add lib/tgspam/samples_model.go lib/tgspam/samples_model_test.go
git commit -m "Add Reset and tokenize methods to SamplesModel"
```

---

## Task 3: Add `model` field to `Detector` + new `NewDetectorWithModel` constructor

This task changes the struct definition and adds the new constructor — but does **not yet** route reads/writes through it. After this task the build will fail because old field accesses still exist; that's OK — Tasks 4-7 fix them piece by piece.

**Files:**
- Modify: `lib/tgspam/detector.go`

- [ ] **Step 1: Update `Detector` struct definition**

In `lib/tgspam/detector.go` around line 37:

```go
type Detector struct {
	Config
	model             *SamplesModel
	openaiChecker     *openAIChecker
	geminiChecker     *geminiChecker
	duplicateDetector *duplicateDetector
	reactionDetector  *reactionDetector
	metaChecks        []MetaCheck
	luaChecks         []plugin.Check
	approvedUsers     map[string]approved.UserInfo
	luaEngine         LuaPluginEngine

	spamSamplesUpd SampleUpdater
	hamSamplesUpd  SampleUpdater
	userStorage    UserStorage

	hamHistory  *spamcheck.LastRequests
	spamHistory *spamcheck.LastRequests

	lock sync.RWMutex
}
```

Removed: `classifier`, `tokenizedSpam`, `stopWords`, `excludedTokens`. Added: `model`.

- [ ] **Step 2: Update `NewDetector` and add `NewDetectorWithModel`**

Replace the body of `NewDetector` (around line 187) with:

```go
// NewDetector makes a new Detector with the given config and a fresh private SamplesModel.
// To share a SamplesModel across detectors (e.g. multi-chat support), use NewDetectorWithModel.
func NewDetector(p Config) *Detector {
	return NewDetectorWithModel(p, NewSamplesModel())
}

// NewDetectorWithModel makes a new Detector that uses the provided SamplesModel.
// All sample-related operations (LoadSamples, UpdateSpam, UpdateHam, RemoveSpam,
// RemoveHam, Reset, classification, similarity, stop-word check) operate on the
// shared model. Per-Detector state (approved users, duplicate detection, reaction
// detection, history queues, LLM clients) remains private to this Detector.
//
// IMPORTANT: calling Detector.Reset() on a Detector built with a shared model wipes
// the shared state for ALL detectors holding that model. Coordinate at the caller layer.
func NewDetectorWithModel(p Config, model *SamplesModel) *Detector {
	if model == nil {
		model = NewSamplesModel()
	}
	res := &Detector{
		Config:            p,
		model:             model,
		approvedUsers:     make(map[string]approved.UserInfo),
		metaChecks:        []MetaCheck{},
		luaChecks:         []plugin.Check{},
		hamHistory:        spamcheck.NewLastRequests(p.HistorySize),
		spamHistory:       spamcheck.NewLastRequests(p.HistorySize),
		duplicateDetector: newDuplicateDetector(p.DuplicateDetection.Threshold, p.DuplicateDetection.Window),
		reactionDetector:  newReactionDetector(p.ReactionSpam.MaxReactions, p.ReactionSpam.Window),
		luaEngine:         nil,
	}
	res.LLMConsensus = res.normalizeLLMConsensusMode(p.LLMConsensus)
	if p.FirstMessagesCount > 0 {
		res.FirstMessageOnly = true
	}
	if p.FirstMessageOnly && p.FirstMessagesCount == 0 {
		res.FirstMessagesCount = 1
	}
	return res
}
```

- [ ] **Step 3: Verify the build now fails (expected)**

```bash
go build ./lib/tgspam/...
```

Expected: many errors of the form `d.classifier undefined`, `d.tokenizedSpam undefined`, etc. — one per remaining old-field reference. Capture the full list with:

```bash
go build ./lib/tgspam/... 2>&1 | grep -E 'd\.(classifier|tokenizedSpam|stopWords|excludedTokens)' | head -40
```

This list drives Tasks 4 and 5.

- [ ] **Step 4: Do NOT commit yet** — leave the broken state for Task 4 to fix immediately. Tasks 3-6 share an in-progress diff and only commit at the end of Task 6 once tests are green again.

This deviates from "commit per task" but is necessary because the refactor straddles many methods. Phase 1's Done definition still requires green tests at the end.

---

## Task 4: Migrate read paths to `d.model`

Replace every `d.classifier.*`, `d.tokenizedSpam`, `d.stopWords`, `d.excludedTokens` *read* in `detector.go` with the model equivalent.

**Files:**
- Modify: `lib/tgspam/detector.go`

- [ ] **Step 1: Migrate `Check` method (around line 215)**

In the existing `Check`:

- `len(d.stopWords) > 0` → `d.model.stopWordsLen() > 0`
- `len(d.tokenizedSpam) > 0` → `d.model.tokenizedSpamLen() > 0`
- The `classifierReady := d.classifier.nAllDocument > 0 && ...` block → `classifierReady := d.model.classifierReady()`

- [ ] **Step 2: Migrate `isSpamClassified` (around line 1007)**

The method calls `d.classifier.classify(tokens)` (or similar). Wrap that whole operation under `d.model.lock.RLock()` so the classify call sees a consistent classifier snapshot:

```go
func (d *Detector) isSpamClassified(msg string) spamcheck.Response {
	d.model.lock.RLock()
	defer d.model.lock.RUnlock()
	tokens := d.model.tokenizeUnlocked(msg)
	// ... existing classification logic, but reading d.model.cls instead of d.classifier ...
}
```

Read the original method to see exactly what to port. Don't invent.

- [ ] **Step 3: Migrate `isSpamSimilarityHigh` (around line 875)**

Same pattern: take `d.model.lock.RLock()` for the duration so `tokenize` + `tokSpam` reads are atomic:

```go
func (d *Detector) isSpamSimilarityHigh(msg string) spamcheck.Response {
	d.model.lock.RLock()
	defer d.model.lock.RUnlock()
	msgTokenized := d.model.tokenizeUnlocked(msg)
	// ... existing similarity logic, replacing d.tokenizedSpam with d.model.tokSpam ...
}
```

- [ ] **Step 4: Migrate `isStopWord` (around line 1029)**

Take `d.model.lock.RLock()` and read `d.model.stops` instead of `d.stopWords`:

```go
func (d *Detector) isStopWord(msg string, req spamcheck.Request) spamcheck.Response {
	d.model.lock.RLock()
	defer d.model.lock.RUnlock()
	// ... existing logic with d.stopWords replaced by d.model.stops ...
}
```

- [ ] **Step 5: Build, expect remaining failures only on write paths**

```bash
go build ./lib/tgspam/... 2>&1 | grep -E 'd\.(classifier|tokenizedSpam|stopWords|excludedTokens)' | head
```

Expected: errors only inside `LoadSamples`, `LoadStopWords`, `Reset`, `updateSample`, `removeSample`, `buildDocs`. If anything else still references old fields, fix it now.

---

## Task 5: Migrate write paths to `d.model`

**Files:**
- Modify: `lib/tgspam/detector.go`

- [ ] **Step 1: Rewrite `LoadSamples` (around line 684)**

Hold the model write lock for the whole method body. Use `d.model.tokenizeUnlocked` (caller already holds the lock, public `tokenize` would deadlock):

```go
func (d *Detector) LoadSamples(exclReader io.Reader, spamReaders, hamReaders []io.Reader) (LoadResult, error) {
	d.model.lock.Lock()
	defer d.model.lock.Unlock()

	d.model.tokSpam = []map[string]int{}
	d.model.excluded = map[string]struct{}{}
	d.model.cls.reset()

	for t := range d.readerIterator(exclReader) {
		d.model.excluded[strings.ToLower(t)] = struct{}{}
	}

	lr := LoadResult{ExcludedTokens: len(d.model.excluded)}

	docs := []document{}
	for token := range d.readerIterator(spamReaders...) {
		tokens := d.model.tokenizeUnlocked(token)
		d.model.tokSpam = append(d.model.tokSpam, tokens)
		toks := make([]string, 0, len(tokens))
		for k := range tokens {
			toks = append(toks, k)
		}
		docs = append(docs, document{spamClass: ClassSpam, tokens: toks})
		lr.SpamSamples++
	}
	for token := range d.readerIterator(hamReaders...) {
		tokens := d.model.tokenizeUnlocked(token)
		toks := make([]string, 0, len(tokens))
		for k := range tokens {
			toks = append(toks, k)
		}
		docs = append(docs, document{spamClass: ClassHam, tokens: toks})
		lr.HamSamples++
	}

	d.model.cls.learn(docs...)
	return lr, nil
}
```

Note: `document` literal uses `spamClass: ClassSpam`/`ClassHam` per the actual definition in `lib/tgspam/classifier.go:21` and `lib/tgspam/detector.go:718`. **Verify this is still the form** by reading the existing code before pasting.

- [ ] **Step 2: Rewrite `LoadStopWords` (around line 727)**

```go
func (d *Detector) LoadStopWords(readers ...io.Reader) (LoadResult, error) {
	d.model.lock.Lock()
	defer d.model.lock.Unlock()
	d.model.stops = []string{}
	for t := range d.readerIterator(readers...) {
		d.model.stops = append(d.model.stops, strings.ToLower(t))
	}
	return LoadResult{StopWords: len(d.model.stops)}, nil
}
```

- [ ] **Step 3: Rewrite `Reset` (around line 474)**

`Detector.Reset` currently clears both shared state and per-Detector state. Preserve that: delegate shared-state clearing to `d.model.Reset()`, keep per-Detector clearing inline. Lock-order invariant: take `d.lock` first, then `d.model.lock` (via `Reset`):

```go
func (d *Detector) Reset() {
	d.lock.Lock()
	defer d.lock.Unlock()
	d.model.Reset()              // shared state — wipes for all detectors holding this model
	d.approvedUsers = map[string]approved.UserInfo{}
	// reset any other per-detector state the original method touched (LLM history, etc.)
}
```

Read the original `Reset` body to see what per-detector state it cleared (e.g., LLM history, samples updaters references) and preserve those clears.

- [ ] **Step 4: Rewrite `updateSample` and `removeSample` (around lines 760-810)**

Both methods mutate `d.classifier` and `d.tokenizedSpam`. Move those mutations inside `d.model.lock.Lock()` block:

```go
func (d *Detector) updateSample(msg string, upd SampleUpdater, sc spamClass) error {
	if upd == nil {
		return errors.New("sample updater is not set")
	}
	if err := upd.Append(msg); err != nil {
		return fmt.Errorf("can't update samples: %w", err)
	}
	d.model.lock.Lock()
	defer d.model.lock.Unlock()
	tokens := d.model.tokenizeUnlocked(msg)
	if sc == ClassSpam {
		d.model.tokSpam = append(d.model.tokSpam, tokens)
	}
	toks := make([]string, 0, len(tokens))
	for k := range tokens {
		toks = append(toks, k)
	}
	d.model.cls.learn(document{spamClass: sc, tokens: toks})
	return nil
}
```

`removeSample` — symmetric. Read the original to preserve exact semantics.

- [ ] **Step 5: Update `buildDocs` if it references old fields**

`buildDocs` at line 811 reads `d.excludedTokens` indirectly via `d.tokenize`. After moving `tokenize` to the model, `buildDocs` either:

- becomes a thin wrapper that calls `d.model.tokenize(msg)` (public, takes its own RLock), OR
- gets inlined where it's used.

Pick whichever keeps the diff smallest. Verify caller contract — if `buildDocs` is called from inside `updateSample` (which holds the write lock), use `tokenizeUnlocked`; otherwise use `tokenize`.

- [ ] **Step 6: Build clean**

```bash
go build ./lib/tgspam/...
```

Expected: success. If any errors remain, run:

```bash
grep -n 'd\.classifier\|d\.tokenizedSpam\|d\.stopWords\|d\.excludedTokens' lib/tgspam/detector.go
```

Expected: empty. Fix any matches and rebuild.

- [ ] **Step 7: Run tests — many will fail because detector_test.go still uses old fields**

```bash
go test -race ./lib/tgspam/ -count=1
```

Expected: compile errors in `detector_test.go`. That's fine — Task 6 fixes them.

---

## Task 6: Migrate `detector_test.go` field accesses + benchmark

24+ existing test sites read `d.classifier.nAllDocument`, `d.tokenizedSpam`, `d.classifier.learningResults`, etc. These tests are correct in intent — they verify the model state — but need to address the new field path. `BenchmarkTokenize` (and any other benchmark that constructs `Detector{...}` literal) needs the same treatment.

**Files:**
- Modify: `lib/tgspam/detector_test.go`

- [ ] **Step 1: List every site that needs updating**

```bash
grep -n 'd\.classifier\|d\.tokenizedSpam\|d\.stopWords\|d\.excludedTokens\|excludedTokens:\|classifier:' lib/tgspam/detector_test.go
```

Expected: 24+ matches. Capture the list.

- [ ] **Step 2: Replace each access mechanically**

For every match:

| Old | New |
|---|---|
| `d.classifier.reset()` | `d.model.cls.reset()` (test takes model lock if needed) |
| `d.classifier.nAllDocument` | `d.model.cls.nAllDocument` |
| `d.classifier.nDocumentByClass[...]` | `d.model.cls.nDocumentByClass[...]` |
| `d.classifier.learningResults` | `d.model.cls.learningResults` |
| `d.tokenizedSpam` | `d.model.tokSpam` |
| `d.tokenizedSpam = nil` | `d.model.tokSpam = nil` |
| `d.stopWords` | `d.model.stops` |
| `d.excludedTokens` | `d.model.excluded` |
| `&Detector{excludedTokens: map[string]struct{}{...}}` | `&Detector{model: &SamplesModel{excluded: map[string]struct{}{...}}}` |

For tests that construct a `Detector` via composite literal AND populate model fields (e.g., `TestDetector_buildDocs`), the right rewrite is:

```go
m := NewSamplesModel()
m.lock.Lock()
m.excluded = map[string]struct{}{"the": {}, "and": {}}
m.lock.Unlock()
d := NewDetectorWithModel(Config{}, m)
```

Use `NewDetectorWithModel` over composite literals where the test wants a fully wired Detector.

For tests that read state immediately after a mutation (no concurrent goroutine), no extra locking is needed — Go's memory model guarantees the read sees the write within the same goroutine.

- [ ] **Step 3: Find and update the benchmark**

```bash
grep -n 'BenchmarkTokenize\|Detector{.*excludedTokens' lib/tgspam/
```

If the benchmark constructs `Detector{excludedTokens: ...}`, rewrite it the same way as the tests above (use `NewDetectorWithModel` or directly seed the model).

- [ ] **Step 4: Build + run all detector tests**

```bash
go build ./lib/tgspam/...
go test -race ./lib/tgspam/ -count=1
```

Expected: all tests pass — including the original 100% of the suite from Pre-flight 2.

If any test fails:
1. If it's an assertion mismatch — the refactor accidentally changed behavior. Revert the failing piece, investigate.
2. If it's a deadlock or race — the locking discipline was violated. Re-check Task 4/5 changes, especially nested locks.

- [ ] **Step 5: Lint clean**

```bash
golangci-lint run ./lib/tgspam/...
```

Expected: clean.

- [ ] **Step 6: Commit (covers Tasks 3-6 — first checkpoint of the refactor)**

```bash
git add lib/tgspam/detector.go lib/tgspam/detector_test.go
git commit -m "Move classifier/tokens/stops/excluded into SamplesModel"
```

---

## Task 7: New test — shared `SamplesModel` propagates updates

This is the headline test for Phase 1: it proves the multi-chat foundation works.

**Files:**
- Modify: `lib/tgspam/detector_test.go`

- [ ] **Step 1: Write failing test**

Append to `lib/tgspam/detector_test.go`:

```go
func TestDetector_SharedSamplesModel_UpdatesPropagate(t *testing.T) {
	model := NewSamplesModel()
	cfg := Config{MinMsgLen: 1, MinSpamProbability: 0.5}
	d1 := NewDetectorWithModel(cfg, model)
	d2 := NewDetectorWithModel(cfg, model)

	spam := bytes.NewBufferString("buy cheap viagra now\nclick here to win cash\n")
	ham := bytes.NewBufferString("the weather is lovely today\nlet's discuss the project plan\n")
	excl := bytes.NewBufferString("")
	_, err := d1.LoadSamples(excl, []io.Reader{spam}, []io.Reader{ham})
	require.NoError(t, err)

	require.True(t, model.classifierReady(), "shared classifier ready after d1 loads samples")
	require.Greater(t, model.tokenizedSpamLen(), 0, "shared model has tokenized spam after d1 loads samples")

	before := model.tokenizedSpamLen()
	require.NoError(t, d1.UpdateSpam("free crypto airdrop dm me"))
	assert.Equal(t, before+1, model.tokenizedSpamLen(),
		"UpdateSpam on d1 must propagate to the shared model that d2 also sees")

	// d2's view of the model is identical because they share the pointer
	assert.Equal(t, model.tokenizedSpamLen(), d2.model.tokenizedSpamLen())
	assert.Equal(t, model.classifierReady(), d2.model.classifierReady())
}
```

- [ ] **Step 2: Run, expect PASS (state already shared via Task 6)**

```bash
go test -race ./lib/tgspam/ -run TestDetector_SharedSamplesModel_UpdatesPropagate -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add lib/tgspam/detector_test.go
git commit -m "Test shared SamplesModel updates propagate across detectors"
```

---

## Task 8: New test — per-Detector state stays isolated (with non-zero thresholds)

The previous draft tested isolation with default-config Detectors, where `duplicateDetector`/`reactionDetector` are `nil` (the constructors return nil when threshold == 0). `assert.NotSame(nil, nil)` would silently pass-or-fail unpredictably. Use non-zero thresholds so the constructors return real instances.

**Files:**
- Modify: `lib/tgspam/detector_test.go`

- [ ] **Step 1: Write failing test**

Append:

```go
func TestDetector_SharedSamplesModel_PerDetectorStateIsolated(t *testing.T) {
	model := NewSamplesModel()
	cfg := Config{
		MinMsgLen:          1,
		FirstMessageOnly:   true,
		FirstMessagesCount: 1,
		// non-zero so newDuplicateDetector/newReactionDetector return real instances:
		DuplicateDetection: struct {
			Threshold int
			Window    time.Duration
		}{Threshold: 3, Window: time.Minute},
		ReactionSpam: struct {
			MaxReactions int
			Window       time.Duration
		}{MaxReactions: 5, Window: time.Minute},
	}
	d1 := NewDetectorWithModel(cfg, model)
	d2 := NewDetectorWithModel(cfg, model)

	// approved-users isolation
	require.NoError(t, d1.AddApprovedUser(approved.UserInfo{UserID: "1001", UserName: "alice"}))
	assert.True(t, d1.IsApprovedUser("1001"), "d1 should know its approved user")
	assert.False(t, d2.IsApprovedUser("1001"), "d2 must not see approvals added to d1")

	// duplicate / reaction detectors are non-nil (config has non-zero thresholds) and distinct
	require.NotNil(t, d1.duplicateDetector)
	require.NotNil(t, d2.duplicateDetector)
	assert.NotSame(t, d1.duplicateDetector, d2.duplicateDetector,
		"each detector must own its own duplicateDetector instance")

	require.NotNil(t, d1.reactionDetector)
	require.NotNil(t, d2.reactionDetector)
	assert.NotSame(t, d1.reactionDetector, d2.reactionDetector,
		"each detector must own its own reactionDetector instance")
}
```

The struct-literal anonymous types for `DuplicateDetection`/`ReactionSpam` must match the actual type definitions in `detector.go` Config. Verify by reading lines 136-144 of `detector.go` first.

- [ ] **Step 2: Run, expect PASS**

```bash
go test -race ./lib/tgspam/ -run TestDetector_SharedSamplesModel_PerDetectorStateIsolated -v
```

Expected: PASS.

- [ ] **Step 3: Add legacy-constructor isolation test**

Append:

```go
func TestDetector_LegacyConstructor_PrivateModel(t *testing.T) {
	cfg := Config{MinMsgLen: 1}
	d1 := NewDetector(cfg)
	d2 := NewDetector(cfg)
	assert.NotSame(t, d1.model, d2.model,
		"NewDetector should allocate a fresh SamplesModel each time, preserving prior single-tenant behavior")
}
```

- [ ] **Step 4: Run**

```bash
go test -race ./lib/tgspam/ -run "TestDetector_SharedSamplesModel_PerDetectorStateIsolated|TestDetector_LegacyConstructor_PrivateModel" -v
```

Expected: both PASS.

- [ ] **Step 5: Commit**

```bash
git add lib/tgspam/detector_test.go
git commit -m "Test per-detector isolation and legacy constructor private model"
```

---

## Task 9: Concurrent-safety smoke test

**Files:**
- Modify: `lib/tgspam/samples_model_test.go`

- [ ] **Step 1: Write the test**

Append to `lib/tgspam/samples_model_test.go`:

```go
func TestSamplesModel_ConcurrentReadWrite(t *testing.T) {
	m := NewSamplesModel()
	cfg := Config{MinMsgLen: 1}
	d1 := NewDetectorWithModel(cfg, m)
	d2 := NewDetectorWithModel(cfg, m)

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
```

Add `"fmt"` and `"sync"` imports if missing.

- [ ] **Step 2: Run under the race detector, multiple iterations**

```bash
go test -race ./lib/tgspam/ -run TestSamplesModel_ConcurrentReadWrite -v -count=10
```

Expected: PASS for all 10 iterations, no race warnings.

- [ ] **Step 3: Commit**

```bash
git add lib/tgspam/samples_model_test.go
git commit -m "Add concurrent read/write race test for SamplesModel"
```

---

## Task 10: Final verification — grep, build, tests, lint, normalise

- [ ] **Step 1: Grep for stale field references in the package**

```bash
grep -rn 'd\.classifier\|d\.tokenizedSpam\|d\.stopWords\|d\.excludedTokens' lib/tgspam/
```

Expected: empty output. Any match indicates a missed migration — fix it.

- [ ] **Step 2: Full module build**

```bash
go build ./...
```

Expected: clean. If any non-`lib/tgspam` package fails, it touches the four removed fields directly (very unlikely for non-test code).

- [ ] **Step 3: Full module test suite**

```bash
go test -race ./... -count=1
```

Expected: every package passes with the same or higher pass count than Pre-flight 2.

- [ ] **Step 4: Lint clean**

```bash
golangci-lint run
```

Expected: clean.

- [ ] **Step 5: Normalise comments**

```bash
command -v unfuck-ai-comments >/dev/null || go install github.com/umputun/unfuck-ai-comments@latest
unfuck-ai-comments run --fmt --skip=mocks ./lib/tgspam/...
```

Re-run tests + lint to confirm nothing was broken:

```bash
go test -race ./lib/tgspam/... -count=1 && golangci-lint run ./lib/tgspam/...
```

- [ ] **Step 6: Commit any normalisation changes**

```bash
git status --short
```

If anything was modified:

```bash
git add -A lib/tgspam/
git commit -m "Normalise comments after detector refactor"
```

Otherwise skip.

---

## Self-Review Checklist

After all tasks pass, verify against the spec (`docs/plans/2026-04-28-multi-chat-design.md` §SpamFilter / Option A):

| Spec requirement | Where it lands |
|---|---|
| Shared classifier (model) pointer held by all per-chat Detectors | Task 3 (`Detector.model *SamplesModel`) + Task 4-5 (state migrated) |
| `UpdateSpam`/`UpdateHam`/`ReloadSamples` propagate via shared state | Task 7 (`TestDetector_SharedSamplesModel_UpdatesPropagate`) |
| Per-chat `approvedUsers`/`duplicateDetector`/`reactionDetector` isolated | Task 8 |
| `NewDetector` preserves single-chat behavior for tests/CLI | Tasks 3, 6, 8 |
| All existing tests pass | Tasks 6, 10 |
| Lock-order invariant documented | Architecture section + `SamplesModel` doc comment |
| `Detector.Reset` semantics for shared model documented | `NewDetectorWithModel` doc comment + `SamplesModel.Reset` doc comment |

Out of scope (deferred to Phase 2):
- Wiring `RuntimeChatContext.Detector` in `app/main.go`.
- Per-chat `SpamFilter` construction in `app/`.
- Anything in `app/events`, `app/webapi`, `app/config`.

---

## Done definition

- All Pre-flight + Task checkboxes ticked.
- `grep -rn 'd\.classifier\|d\.tokenizedSpam\|d\.stopWords\|d\.excludedTokens' lib/tgspam/` returns empty.
- `go test -race ./...` passes with the same or higher count than Pre-flight 2.
- `golangci-lint run` clean.
- Branch contains 7-10 commits, one per task or grouped checkpoint, suitable for review.

After this plan is complete, the next plan (`2026-04-28-multi-chat-phase2-config-and-wiring.md`) introduces `ConfiguredChat`, `RuntimeChatContext`, `engine.WithGID`, and per-chat construction in `app/main.go`.
