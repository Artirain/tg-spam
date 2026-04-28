# Multi-Chat Phase 1 — Detector Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refactor `lib/tgspam.Detector` so that the shared sample-derived model state (classifier + tokenized spam + stop words + excluded tokens) lives in a separate `SamplesModel` struct that can be **shared by reference** across multiple `Detector` instances. After this phase, behavior is unchanged for single-Detector callers; multi-Detector use is enabled but not yet wired.

**Architecture:** Introduce `SamplesModel` (mutex-guarded bundle of state mutated by `LoadSamples`/`UpdateSpam`/`UpdateHam`/`RemoveSpam`/`RemoveHam`/`Reset`). `Detector` holds `*SamplesModel` (pointer). Per-Detector state stays on `Detector`: `approvedUsers`, `duplicateDetector`, `reactionDetector`, `hamHistory`, `spamHistory`, `userStorage`, LLM clients, lua engine, meta checks. `NewDetector` continues to allocate a fresh `SamplesModel` when none is supplied — preserving today's API and tests verbatim.

**Tech Stack:** Go 1.24+, `lib/tgspam` package, `github.com/stretchr/testify`, `go test -race ./...`, `golangci-lint run`.

**Reference spec:** `docs/plans/2026-04-28-multi-chat-design.md` §SpamFilter / Option A.

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `lib/tgspam/samples_model.go` | Create | New struct `SamplesModel` bundling classifier + tokenizedSpam + stopWords + excludedTokens behind a `sync.RWMutex`. Methods: `NewSamplesModel`, `Reset` |
| `lib/tgspam/samples_model_test.go` | Create | Unit tests for `SamplesModel` construction, `Reset`, concurrent safety |
| `lib/tgspam/detector.go` | Modify | Replace four fields with `model *SamplesModel`. All read/write of these fields goes through the shared model. `NewDetector` accepts optional shared model |
| `lib/tgspam/detector_test.go` | Modify | Add tests for shared-model behavior (UpdateSpam visible across detectors), isolation behavior (approvedUsers per-detector). Existing tests left untouched |

The plan deliberately keeps the public `Detector` API stable — existing call sites in `app/main.go`, `app/bot/spam.go`, `lib/tgspam/detector_test.go`, and CLI tools continue to work without changes.

---

## Pre-flight

- [ ] **Pre-flight 1: confirm clean working tree**

```bash
git status --short
```

Expected: empty output (or only untracked spec files). Stash anything else first.

- [ ] **Pre-flight 2: baseline test run**

```bash
go test -race ./lib/tgspam/...
```

Expected: all tests pass. Capture the exact pass count for later comparison.

- [ ] **Pre-flight 3: baseline lint**

```bash
golangci-lint run ./lib/tgspam/...
```

Expected: no errors. Anything we touch later will need to clear this same bar.

---

## Task 1: Create `SamplesModel` skeleton

**Files:**
- Create: `lib/tgspam/samples_model.go`
- Create: `lib/tgspam/samples_model_test.go`

- [ ] **Step 1: Write the failing test for `NewSamplesModel`**

Create `lib/tgspam/samples_model_test.go`:

```go
package tgspam

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSamplesModel(t *testing.T) {
	m := NewSamplesModel()
	require.NotNil(t, m)
	assert.Empty(t, m.tokenizedSpam())
	assert.Empty(t, m.stopWords())
	assert.Empty(t, m.excludedTokens())
	// classifier is freshly allocated and has no learned documents yet
	assert.Equal(t, 0, m.classifierStats().AllDocs)
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test -race ./lib/tgspam/ -run TestNewSamplesModel -v
```

Expected: `undefined: NewSamplesModel` compile error.

- [ ] **Step 3: Implement `SamplesModel` skeleton**

Create `lib/tgspam/samples_model.go`:

```go
package tgspam

import "sync"

// SamplesModel bundles the in-memory state derived from spam/ham samples and stop words.
// Multiple Detector instances may share a single *SamplesModel by reference so that
// updates from any chat (UpdateSpam/UpdateHam/RemoveSpam/RemoveHam/LoadSamples) become
// visible to all detectors immediately. The internal mutex protects all fields.
type SamplesModel struct {
	cls            classifier
	tokSpam        []map[string]int
	stops          []string
	excluded       map[string]struct{}
	lock           sync.RWMutex
}

// NewSamplesModel creates an empty model with a freshly initialised classifier.
func NewSamplesModel() *SamplesModel {
	return &SamplesModel{
		cls:      newClassifier(),
		tokSpam:  []map[string]int{},
		excluded: map[string]struct{}{},
	}
}

// classifierStats is a small read-only view used in tests.
type classifierStats struct {
	AllDocs int
}

// classifierStats returns a snapshot of classifier state for diagnostics/tests.
func (m *SamplesModel) classifierStats() classifierStats {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return classifierStats{AllDocs: m.cls.nAllDocument}
}

// tokenizedSpam returns a snapshot of tokenized spam samples (test helper).
func (m *SamplesModel) tokenizedSpam() []map[string]int { m.lock.RLock(); defer m.lock.RUnlock(); return m.tokSpam }

// stopWords returns a snapshot of stop words (test helper).
func (m *SamplesModel) stopWords() []string { m.lock.RLock(); defer m.lock.RUnlock(); return m.stops }

// excludedTokens returns a snapshot of excluded tokens (test helper).
func (m *SamplesModel) excludedTokens() map[string]struct{} { m.lock.RLock(); defer m.lock.RUnlock(); return m.excluded }
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test -race ./lib/tgspam/ -run TestNewSamplesModel -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add lib/tgspam/samples_model.go lib/tgspam/samples_model_test.go
git commit -m "Add SamplesModel skeleton for shared classifier state"
```

---

## Task 2: Add `Reset` to `SamplesModel`

**Files:**
- Modify: `lib/tgspam/samples_model.go`
- Modify: `lib/tgspam/samples_model_test.go`

- [ ] **Step 1: Write the failing test for `Reset`**

Append to `lib/tgspam/samples_model_test.go`:

```go
func TestSamplesModel_Reset(t *testing.T) {
	m := NewSamplesModel()
	// seed some state directly via internals
	m.lock.Lock()
	m.tokSpam = []map[string]int{{"buy": 1}}
	m.stops = []string{"viagra"}
	m.excluded = map[string]struct{}{"the": {}}
	m.lock.Unlock()

	m.Reset()

	assert.Empty(t, m.tokenizedSpam())
	assert.Empty(t, m.stopWords())
	assert.Empty(t, m.excludedTokens())
	assert.Equal(t, 0, m.classifierStats().AllDocs)
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test -race ./lib/tgspam/ -run TestSamplesModel_Reset -v
```

Expected: `m.Reset undefined`.

- [ ] **Step 3: Implement `Reset`**

Append to `lib/tgspam/samples_model.go`:

```go
// Reset clears all derived state (classifier, tokenized samples, stop words, excluded tokens).
// Holds the write lock for the duration of the reset.
func (m *SamplesModel) Reset() {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.cls.reset()
	m.tokSpam = []map[string]int{}
	m.stops = []string{}
	m.excluded = map[string]struct{}{}
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test -race ./lib/tgspam/ -run TestSamplesModel_Reset -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add lib/tgspam/samples_model.go lib/tgspam/samples_model_test.go
git commit -m "Add Reset method to SamplesModel"
```

---

## Task 3: Wire `SamplesModel` into `Detector` (preserving behavior)

This is the central refactor. We replace four `Detector` fields with one `*SamplesModel` pointer and route every read/write through it. The existing `Detector.lock` continues to guard per-Detector state (`approvedUsers`, history queues, etc.). The `SamplesModel` has its own mutex, so the locking discipline becomes: **acquire `Detector.lock` for per-Detector state, acquire `model.lock` for shared model state, never hold one while waiting on the other**.

**Files:**
- Modify: `lib/tgspam/detector.go`

- [ ] **Step 1: Add `model` field and remove old fields**

In `lib/tgspam/detector.go`, change the `Detector` struct definition (around line 37) so the four old fields disappear and `model` is added:

```go
// Detector is a spam detector, thread-safe.
// It uses a set of checks to determine if a message is spam, and also keeps a list of approved users.
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

Removed: `classifier`, `tokenizedSpam`, `stopWords`, `excludedTokens` (now inside `model`).

- [ ] **Step 2: Update `NewDetector` to allocate a `SamplesModel`**

Replace the `NewDetector` body (around line 187) with:

```go
// NewDetector makes a new Detector with the given config and a fresh private SamplesModel.
// To share a SamplesModel across detectors, use NewDetectorWithModel.
func NewDetector(p Config) *Detector {
	return NewDetectorWithModel(p, NewSamplesModel())
}

// NewDetectorWithModel makes a new Detector that uses the provided SamplesModel.
// All sample-related operations (LoadSamples, UpdateSpam, UpdateHam, RemoveSpam,
// RemoveHam, Reset, classification, similarity, stop-word check) operate on the
// shared model. Per-Detector state (approved users, duplicate detection, reaction
// detection, history queues, LLM clients) remains private to this Detector.
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

- [ ] **Step 3: Route all reads of the shared state through `model`**

Find every read of `d.classifier`, `d.tokenizedSpam`, `d.stopWords`, `d.excludedTokens` in `lib/tgspam/detector.go` and replace as follows. Each replacement must hold `d.model.lock` only — release `d.lock` before acquiring it where needed.

Pattern: replace `d.classifier.XYZ` with a helper that takes the model's read lock.

Add these helpers to `lib/tgspam/detector.go` (place them near the bottom, before the closing of the file):

```go
// modelClassifierReady returns true when the shared classifier has trained docs in both classes.
func (d *Detector) modelClassifierReady() bool {
	d.model.lock.RLock()
	defer d.model.lock.RUnlock()
	return d.model.cls.nAllDocument > 0 &&
		d.model.cls.nDocumentByClass["ham"] > 0 && d.model.cls.nDocumentByClass["spam"] > 0
}

// modelTokenizedSpamCount returns the number of tokenized spam samples currently loaded.
func (d *Detector) modelTokenizedSpamCount() int {
	d.model.lock.RLock()
	defer d.model.lock.RUnlock()
	return len(d.model.tokSpam)
}

// modelStopWordsCount returns the number of stop words currently loaded.
func (d *Detector) modelStopWordsCount() int {
	d.model.lock.RLock()
	defer d.model.lock.RUnlock()
	return len(d.model.stops)
}
```

Then replace each callsite found via:

```bash
grep -n 'd\.classifier\|d\.tokenizedSpam\|d\.stopWords\|d\.excludedTokens' lib/tgspam/detector.go
```

For each match (the grep at top of plan listed them), apply the appropriate transformation:

- `len(d.stopWords) > 0` (line ~244) → `d.modelStopWordsCount() > 0`
- `len(d.tokenizedSpam) > 0` (line ~302) → `d.modelTokenizedSpamCount() > 0`
- `d.classifier.nAllDocument > 0 && d.classifier.nDocumentByClass["ham"] > 0 && d.classifier.nDocumentByClass["spam"] > 0` (lines ~308-309) → `d.modelClassifierReady()`
- Inside `Reset` (lines ~478-482): replace the four assignments with a single `d.model.Reset()`.
- Inside `LoadSamples` (lines ~688-722): replace direct field access with reads/writes through `d.model.lock` (see Step 4).
- Inside `LoadStopWords` (lines ~731-735): replace `d.stopWords = ...` with `d.model.lock.Lock(); d.model.stops = ...; d.model.lock.Unlock()`.
- Inside `updateSample` (lines ~775-780): operate on `d.model.cls` and `d.model.tokSpam` under `d.model.lock`.
- Inside `removeSample` (around line 787): same.
- Inside `isSpamClassified`, `isSpamSimilarityHigh`, `isStopWord`: take `d.model.lock.RLock()` for the duration of the read.

Apply each replacement individually, running the test suite between each to keep the diff small and easy to bisect if something breaks.

- [ ] **Step 4: Rewrite `LoadSamples` to operate on the shared model**

Find `LoadSamples` (around line 684) and replace its body so all four shared-state mutations happen inside one critical section on `d.model.lock`. The new body holds the model's write lock around the whole operation:

```go
// LoadSamples loads spam samples, ham samples, and excluded tokens into the shared SamplesModel.
// Writes are atomic with respect to other callers of the same model.
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
		tokens := d.tokenize(token) // tokenize is pure; no lock needed
		d.model.tokSpam = append(d.model.tokSpam, tokens)
		toks := make([]string, 0, len(tokens))
		for k := range tokens {
			toks = append(toks, k)
		}
		docs = append(docs, document{class: "spam", tokens: toks})
		lr.SpamSamples++
	}
	for token := range d.readerIterator(hamReaders...) {
		tokens := d.tokenize(token)
		toks := make([]string, 0, len(tokens))
		for k := range tokens {
			toks = append(toks, k)
		}
		docs = append(docs, document{class: "ham", tokens: toks})
		lr.HamSamples++
	}

	d.model.cls.learn(docs...)
	return lr, nil
}
```

Note: `d.tokenize` reads `d.model.excluded`. To avoid deadlock, `tokenize` must NOT take `d.model.lock` itself. Audit `tokenize` (around line 850) — it currently reads `d.excludedTokens`. Update it to read directly from `d.model.excluded` *without* locking, on the assumption that callers (LoadSamples, updateSample, removeSample, isSpamSimilarityHigh) already hold `d.model.lock` (read or write). Document this contract:

```go
// tokenize splits a string into a token map. The caller MUST already hold d.model.lock
// (read or write) because tokenize reads d.model.excluded directly.
func (d *Detector) tokenize(inp string) map[string]int {
	// (existing body, but replace d.excludedTokens with d.model.excluded)
}
```

- [ ] **Step 5: Run the existing detector test suite**

```bash
go test -race ./lib/tgspam/ -count=1
```

Expected: PASS, same count as in Pre-flight 2. If anything fails, the most likely culprits are:
- A missed callsite still referencing `d.classifier` / `d.tokenizedSpam` / `d.stopWords` / `d.excludedTokens` — re-run the grep and fix.
- A double-locking deadlock in `LoadSamples`/`updateSample` interacting with `tokenize` — verify `tokenize` does not lock.

Iterate until green.

- [ ] **Step 6: Lint clean**

```bash
golangci-lint run ./lib/tgspam/...
```

Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add lib/tgspam/detector.go
git commit -m "Move classifier/tokenized/stops/excluded into SamplesModel"
```

---

## Task 4: Test that two `Detector`s sharing a model see updates from each other

This is the headline test for Phase 1: it proves the multi-chat foundation actually works.

**Files:**
- Modify: `lib/tgspam/detector_test.go`

- [ ] **Step 1: Write the failing test**

Append to `lib/tgspam/detector_test.go`:

```go
func TestDetector_SharedSamplesModel_UpdatesPropagate(t *testing.T) {
	model := NewSamplesModel()

	// two detectors, same shared model, distinct configs are fine
	cfg := Config{MinMsgLen: 1, MinSpamProbability: 0.5}
	d1 := NewDetectorWithModel(cfg, model)
	d2 := NewDetectorWithModel(cfg, model)

	// seed both classes with some baseline so the classifier is "ready" enough.
	// LoadSamples on d1 alone should populate the shared model that d2 also sees.
	spam := bytes.NewBufferString("buy cheap viagra now\nclick here to win cash\n")
	ham := bytes.NewBufferString("the weather is lovely today\nlet's discuss the project plan\n")
	excl := bytes.NewBufferString("")
	_, err := d1.LoadSamples(excl, []io.Reader{spam}, []io.Reader{ham})
	require.NoError(t, err)

	// d2 immediately sees the loaded model
	require.True(t, d2.modelClassifierReady(), "d2 should observe shared classifier readiness after d1 loads samples")
	require.Greater(t, d2.modelTokenizedSpamCount(), 0, "d2 should see tokenized spam samples loaded by d1")

	// adding a new spam sample on d1 must be visible to d2's classifier
	spamSamples := d2.modelTokenizedSpamCount()
	require.NoError(t, d1.UpdateSpam("free crypto airdrop dm me"))
	assert.Equal(t, spamSamples+1, d2.modelTokenizedSpamCount(),
		"UpdateSpam on d1 must propagate to d2 via shared model")
}
```

You'll need to add `"bytes"` and `"io"` to the imports of `detector_test.go` if not already present.

- [ ] **Step 2: Run the test**

```bash
go test -race ./lib/tgspam/ -run TestDetector_SharedSamplesModel_UpdatesPropagate -v
```

Expected: PASS (the model is already shared by reference because Task 3 made every read/write go through `d.model`). If FAIL, there is a missed callsite still using a per-Detector field — fix and re-run.

- [ ] **Step 3: Commit**

```bash
git add lib/tgspam/detector_test.go
git commit -m "Test SamplesModel updates propagate across detectors"
```

---

## Task 5: Test that per-Detector state stays isolated

**Files:**
- Modify: `lib/tgspam/detector_test.go`

- [ ] **Step 1: Write the failing test**

Append to `lib/tgspam/detector_test.go`:

```go
func TestDetector_SharedSamplesModel_PerDetectorStateIsolated(t *testing.T) {
	model := NewSamplesModel()
	cfg := Config{
		MinMsgLen:          1,
		FirstMessageOnly:   true,
		FirstMessagesCount: 1,
	}
	d1 := NewDetectorWithModel(cfg, model)
	d2 := NewDetectorWithModel(cfg, model)

	// approve user on d1; d2 must NOT see them as approved
	require.NoError(t, d1.AddApprovedUser(approved.UserInfo{UserID: "1001", UserName: "alice"}))
	assert.True(t, d1.IsApprovedUser("1001"), "d1 should know its approved user")
	assert.False(t, d2.IsApprovedUser("1001"), "d2 must not see approvals added to d1")

	// duplicate detection state is also per-detector: trigger it on d1, verify d2 unaffected.
	// (only meaningful when DuplicateDetection.Threshold > 0; default 0 means disabled, which
	// still proves isolation: per-detector instances exist and don't share state.)
	assert.NotSame(t, d1.duplicateDetector, d2.duplicateDetector,
		"each detector must own its own duplicateDetector instance")
	assert.NotSame(t, d1.reactionDetector, d2.reactionDetector,
		"each detector must own its own reactionDetector instance")
}
```

- [ ] **Step 2: Run the test**

```bash
go test -race ./lib/tgspam/ -run TestDetector_SharedSamplesModel_PerDetectorStateIsolated -v
```

Expected: PASS. If FAIL on the `IsApprovedUser` assertion, then Task 3 accidentally moved `approvedUsers` into the shared model — back it out.

- [ ] **Step 3: Commit**

```bash
git add lib/tgspam/detector_test.go
git commit -m "Test per-detector state isolation alongside shared model"
```

---

## Task 6: Test that the legacy `NewDetector` constructor stays single-instance

**Files:**
- Modify: `lib/tgspam/detector_test.go`

- [ ] **Step 1: Write the test**

Append to `lib/tgspam/detector_test.go`:

```go
func TestDetector_LegacyConstructor_PrivateModel(t *testing.T) {
	cfg := Config{MinMsgLen: 1}
	d1 := NewDetector(cfg)
	d2 := NewDetector(cfg)

	// each detector built via the legacy constructor must own a distinct SamplesModel.
	assert.NotSame(t, d1.model, d2.model,
		"NewDetector should allocate a fresh SamplesModel each time, preserving prior single-tenant behavior")
}
```

- [ ] **Step 2: Run the test**

```bash
go test -race ./lib/tgspam/ -run TestDetector_LegacyConstructor_PrivateModel -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add lib/tgspam/detector_test.go
git commit -m "Test NewDetector still allocates a private SamplesModel"
```

---

## Task 7: Concurrent-safety smoke test for shared model

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

	// 16 goroutines hammer UpdateSpam/UpdateHam on d1 while 16 read modelClassifierReady on d2.
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
			_ = d2.modelClassifierReady()
			_ = d2.modelTokenizedSpamCount()
		}()
	}
	wg.Wait()

	// classifier should have learned at least N docs (each UpdateSpam appends one)
	assert.GreaterOrEqual(t, m.classifierStats().AllDocs, N)
}
```

Add `"fmt"` and `"sync"` imports to the test file if missing.

- [ ] **Step 2: Run the test under the race detector**

```bash
go test -race ./lib/tgspam/ -run TestSamplesModel_ConcurrentReadWrite -v -count=10
```

Expected: PASS, no race detector warnings, across 10 iterations. If `-race` reports a data race, the most likely place is `tokenize` reading `d.model.excluded` without holding the lock when a caller forgot to hold it — re-audit per-callsite locking from Task 3.

- [ ] **Step 3: Commit**

```bash
git add lib/tgspam/samples_model_test.go
git commit -m "Add concurrent read/write race test for SamplesModel"
```

---

## Task 8: Verify the rest of the codebase still builds and passes

The Detector refactor touches a lot of internal state. Other packages (`app/bot/spam.go`, `app/main.go`, `app/events/*`, `lib/tgspam/llm.go`, etc.) call into `Detector`. They don't reference the four moved fields directly, so they should compile unchanged — but verify.

- [ ] **Step 1: Build the whole module**

```bash
go build ./...
```

Expected: clean. If anything fails to compile, it is touching the four removed fields directly (very unlikely for non-test code, but possible in tests). Patch up by routing through Detector methods.

- [ ] **Step 2: Run the full test suite**

```bash
go test -race ./... -count=1
```

Expected: every package passes. Capture the count, compare to Pre-flight 2.

- [ ] **Step 3: Lint clean**

```bash
golangci-lint run
```

Expected: clean.

- [ ] **Step 4: Normalise comments**

```bash
command -v unfuck-ai-comments >/dev/null || go install github.com/umputun/unfuck-ai-comments@latest
unfuck-ai-comments run --fmt --skip=mocks ./lib/tgspam/...
```

Re-run tests + lint:

```bash
go test -race ./lib/tgspam/... && golangci-lint run ./lib/tgspam/...
```

Expected: clean.

- [ ] **Step 5: Commit any normalisation changes**

```bash
git status --short
```

If anything was modified by `unfuck-ai-comments`:

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
| Shared classifier pointer held by all per-chat Detectors | Task 3 (`Detector.model *SamplesModel`) |
| `UpdateSpam`/`UpdateHam`/`ReloadSamples` propagate via shared state | Task 4 (`TestDetector_SharedSamplesModel_UpdatesPropagate`) |
| Per-chat `approvedUsers`/`duplicateDetector`/`reactionDetector` isolated | Task 5 |
| `NewDetector` preserves single-chat behavior for tests/CLI | Task 6 |
| All existing tests pass | Task 8 |

Out of scope for Phase 1, deferred to Phase 2 (per-chat wiring) or Phase 3 (storage migration):
- Wiring `RuntimeChatContext.Detector` in `app/main.go`.
- Per-chat `SpamFilter` construction.
- Storage `UNIQUE` constraints.
- Anything in `app/events`, `app/webapi`, `app/config`.

If Task 8 reveals a regression in another package, surface it here and stop — do not paper over it. Phase 1 must leave the suite green for downstream phases to make sense.

---

## Done definition

- All seven Pre-flight + Task checklist boxes ticked.
- `go test -race ./...` passes with the same or higher pass count than Pre-flight 2.
- `golangci-lint run` clean.
- The branch contains 7-8 small commits, one per task or sub-step, suitable for review.

After this plan is complete, the next plan (`2026-04-28-multi-chat-phase2-config-and-wiring.md`) introduces `ConfiguredChat`, `RuntimeChatContext`, `engine.WithGID`, and per-chat construction in `app/main.go`.
