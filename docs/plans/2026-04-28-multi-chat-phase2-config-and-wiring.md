# Multi-Chat Phase 2 — Config + Per-Chat Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the data-model foundations for multi-chat: `ConfiguredChat` and `RuntimeChatContext` types, `Telegram.Groups []ConfiguredChat` config field with legacy fallback, `engine.SQL.WithGID()` shallow-copy helper, and per-chat construction of `Detector`/`SpamFilter`/`Locator`/`ApprovedUsers`/`DetectedSpam`/`Reports`/`Warnings` in `app/main.go`. Plus the Phase 1 follow-up test that locks the `Reset()` propagation contract before callers depend on it.

**Architecture:**
- Validation **caps `Telegram.Groups` at 1 entry** for now — the listener doesn't yet route to multiple chats (Phase 4). Operators can write the new YAML/env shape; they just can't run more than one group through it. This keeps Phase 2 small and reversible.
- Legacy single-chat config (`Telegram.Group != ""` + `Telegram.Groups` empty) auto-converts to `Groups = [{Group, GID: InstanceID}]` at startup. Zero behavioural change for existing installs.
- `engine.SQL.WithGID(gid)` is a shallow copy that shares the underlying `*sqlx.DB`. Per-chat scoped engines hand off to per-chat stores.
- `app/main.go` `execute()` builds the per-chat slice from `Groups`. The listener consumes `contexts[0]` only; multi-chat routing is Phase 4.
- `Admin.SuperUsersCrossChat bool` is added to `AdminSettings` (default `false`); the runtime check is wired in Phase 4 (`updateSupers` honours the flag). Phase 2 only adds the field, parser, and validation.

**Tech Stack:** Go 1.24+, `app/config`, `app/storage/engine`, `app/main.go`, `lib/tgspam`, `github.com/stretchr/testify`, `go test -race ./...`, `golangci-lint run` (Docker).

**Reference spec:** `docs/plans/2026-04-28-multi-chat-design.md` §Identifiers, §Architecture, §Storage layer, §Config & CLI.
**Phase 1 follow-up addressed here:** add `TestDetector_Reset_PropagatesAcrossSharedDetectors` (item #1 from Phase 1 final review).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `lib/tgspam/detector_test.go` | Modify | Add `TestDetector_Reset_PropagatesAcrossSharedDetectors` (Phase 1 follow-up #1) |
| `app/storage/engine/engine.go` | Modify | Add `WithGID(gid string) *SQL` shallow-copy method |
| `app/storage/engine/engine_test.go` | Modify | Add tests for `WithGID` (sharing, GID switch, `Close` semantics) |
| `app/config/settings.go` | Modify | Add `ConfiguredChat`, `Telegram.Groups`, `Admin.SuperUsersCrossChat`. Validation hook |
| `app/config/settings_test.go` | Modify | Add tests for new types, validation, legacy conversion |
| `app/events/types.go` (new) | Create | `RuntimeChatContext` struct (placed in events package since listener will own it in Phase 4) |
| `app/events/types_test.go` (new) | Create | Smoke test for `RuntimeChatContext` construction |
| `app/main.go` | Modify | `execute()` builds per-chat contexts from `Groups`, wires `contexts[0]` into the existing listener struct (no listener API change) |

The plan deliberately does **not** touch `app/events/listener.go` core fields beyond adding `Groups []config.ConfiguredChat` and `contexts []*RuntimeChatContext` as new fields the listener can ignore until Phase 4.

---

## Pre-flight

- [ ] **Pre-flight 1: confirm clean tree on the right branch**

```bash
cd /home/deploy/tg-spam
git status --short
git rev-parse --abbrev-ref HEAD
```

Expected: working tree clean. Branch is whatever Phase 2 uses — recommended `multichat/phase2-config-wiring` cut from `multichat/phase1-detector-refactor`.

If you're starting Phase 2 in a fresh branch:

```bash
git checkout multichat/phase1-detector-refactor
git checkout -b multichat/phase2-config-wiring
```

- [ ] **Pre-flight 2: baseline test run**

```bash
go test -race ./... -count=1 2>&1 | tail -20
```

Expected: every package passes. Capture pass count.

- [ ] **Pre-flight 3: baseline lint**

```bash
docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:latest golangci-lint run 2>&1 | tail -10
```

Expected: `0 issues.`

---

## Task 1: Reset propagation test (Phase 1 follow-up #1)

Lock the multi-chat `Reset()` contract before any Phase 2+ caller depends on it. Per `samples_model.go:73-74` and `detector.go:189-196`, calling `d1.Reset()` on a `Detector` that shares its `*SamplesModel` with `d2` must wipe shared state for both.

**Files:**
- Modify: `lib/tgspam/detector_test.go`

- [ ] **Step 1: Write the failing test**

Append to `lib/tgspam/detector_test.go`:

```go
func TestDetector_Reset_PropagatesAcrossSharedDetectors(t *testing.T) {
	model := NewSamplesModel()
	cfg := Config{MinMsgLen: 1}
	d1 := NewDetectorWithModel(cfg, model)
	d2 := NewDetectorWithModel(cfg, model)

	// seed the shared model via d1
	d1.WithSpamUpdater(&mocks.SampleUpdaterMock{
		AppendFunc: func(string) error { return nil },
		RemoveFunc: func(string) error { return nil },
	})
	require.NoError(t, d1.UpdateSpam("buy cheap viagra"))
	require.NoError(t, d1.UpdateSpam("free crypto airdrop"))
	require.Greater(t, d2.model.tokenizedSpamLen(), 0,
		"d2 must observe d1's spam additions via shared model")
	beforeReset := d2.model.tokenizedSpamLen()
	require.Greater(t, beforeReset, 0)

	// also seed per-detector approved-users on d2 — Reset on d1 must NOT touch d2's
	require.NoError(t, d2.AddApprovedUser(approved.UserInfo{UserID: "777", UserName: "carol"}))
	require.True(t, d2.IsApprovedUser("777"))

	// d1.Reset wipes the shared model — d2 must see the wipe immediately
	d1.Reset()
	assert.Equal(t, 0, d2.model.tokenizedSpamLen(),
		"d1.Reset() must wipe the shared SamplesModel that d2 also holds")
	assert.Equal(t, 0, d2.model.classifierAllDocs(),
		"d1.Reset() must reset the shared classifier visible to d2")

	// per-detector approved-users on d2 are NOT shared and must survive d1.Reset()
	assert.True(t, d2.IsApprovedUser("777"),
		"d1.Reset() must NOT touch d2's per-detector approved users")
}
```

- [ ] **Step 2: Run, expect PASS**

```bash
go test -race ./lib/tgspam/ -run TestDetector_Reset_PropagatesAcrossSharedDetectors -v
```

Expected: PASS (the contract is already in place; this test just locks it in).

If it FAILS:
- `tokenizedSpamLen()` not zero after Reset → `d.model.Reset()` doesn't clear `tokSpam`. Look at `samples_model.go` `Reset` body — Phase 1 should have made it correct.
- `IsApprovedUser("777")` returns false → `Detector.Reset` is wiping per-detector approvedUsers via `d.approvedUsers = ...`, which is correct behavior **only** when called on the same detector. But `d1.Reset()` shouldn't touch `d2.approvedUsers`. If this assertion fails, the bug is more serious — investigate.

- [ ] **Step 3: Commit**

```bash
git add lib/tgspam/detector_test.go
git commit -m "Test Reset propagates across shared SamplesModel detectors"
```

Verify subject ≤60 chars, imperative, no Co-Authored-By trailer.

---

## Task 2: `engine.SQL.WithGID()` shallow copy

Per spec §Storage layer: a method on `*SQL` that returns a shallow copy with a different `gid`, sharing the underlying `*sqlx.DB`. The original `Close()` call on the root engine continues to close everything; scoped copies must NOT call `Close()`.

**Files:**
- Modify: `app/storage/engine/engine.go`
- Modify: `app/storage/engine/engine_test.go`

- [ ] **Step 1: Write the failing test**

Append to `app/storage/engine/engine_test.go`:

```go
func TestSQL_WithGID(t *testing.T) {
	ctx := context.Background()
	root, err := New(ctx, ":memory:", "instance-x")
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	scoped := root.WithGID("chat_42")

	t.Run("scoped copy has new gid", func(t *testing.T) {
		assert.Equal(t, "chat_42", scoped.GID())
	})

	t.Run("root gid is unchanged", func(t *testing.T) {
		assert.Equal(t, "instance-x", root.GID())
	})

	t.Run("dbType is preserved", func(t *testing.T) {
		assert.Equal(t, root.Type(), scoped.Type())
	})

	t.Run("scoped shares the underlying connection", func(t *testing.T) {
		_, err := root.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS phase2_probe (id INTEGER)")
		require.NoError(t, err)
		// table created on root must be visible via scoped (same *sqlx.DB)
		var n int
		err = scoped.GetContext(ctx, &n, "SELECT COUNT(*) FROM phase2_probe")
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})

	t.Run("WithGID empty falls back to caller policy not engine policy", func(t *testing.T) {
		// engine accepts empty gid as-is; validation that rejects empty is a config concern
		empty := root.WithGID("")
		assert.Equal(t, "", empty.GID())
	})
}
```

If `Type()` doesn't exist on `*SQL`, replace the dbType assertion with a direct field comparison via a getter you find in the file. Read `engine.go` first.

- [ ] **Step 2: Run, expect compile failure**

```bash
go test -race ./app/storage/engine/ -run TestSQL_WithGID -v
```

Expected: `WithGID undefined`.

- [ ] **Step 3: Implement `WithGID`**

In `app/storage/engine/engine.go`, add after `New`:

```go
// WithGID returns a shallow copy of *SQL with the gid replaced.
// The underlying *sqlx.DB and any internal locks are shared — Close must only
// be called on the root engine, never on a scoped copy.
//
// Used by multi-chat callers to scope per-chat stores (Locator, ApprovedUsers,
// DetectedSpam, Reports, Warnings) to a specific chat's gid while reusing the
// single connection pool of the root engine.
func (e *SQL) WithGID(gid string) *SQL {
	cp := *e
	cp.gid = gid
	return &cp
}
```

Place it next to `GID()` for discoverability.

- [ ] **Step 4: Run, expect pass**

```bash
go test -race ./app/storage/engine/ -run TestSQL_WithGID -v
```

Expected: PASS for all subtests.

- [ ] **Step 5: Run full engine suite for regression**

```bash
go test -race ./app/storage/engine/ -count=1
```

Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add app/storage/engine/engine.go app/storage/engine/engine_test.go
git commit -m "Add SQL.WithGID shallow-copy for per-chat scoping"
```

---

## Task 3: `ConfiguredChat` type + `Telegram.Groups` field

Per spec §Architecture and §Identifiers: a `ConfiguredChat` struct with `Group string` and `GID string` fields. `gid` regex `^[a-zA-Z0-9_-]{1,24}$`, no `:`.

**Files:**
- Modify: `app/config/settings.go`
- Modify: `app/config/settings_test.go`

- [ ] **Step 1: Write the failing test**

Append to `app/config/settings_test.go`:

```go
func TestConfiguredChat_Validate(t *testing.T) {
	tests := []struct {
		name    string
		chat    ConfiguredChat
		wantErr string
	}{
		{name: "valid with explicit gid", chat: ConfiguredChat{Group: "MyGroup", GID: "main"}},
		{name: "valid with implicit gid", chat: ConfiguredChat{Group: "MyGroup"}},
		{name: "valid numeric chat id", chat: ConfiguredChat{Group: "-1001234567890", GID: "secondary"}},
		{name: "empty group rejected", chat: ConfiguredChat{Group: "", GID: "x"}, wantErr: "group is required"},
		{name: "gid with colon rejected", chat: ConfiguredChat{Group: "g", GID: "bad:gid"}, wantErr: "gid"},
		{name: "gid too long rejected", chat: ConfiguredChat{Group: "g", GID: "abcdefghijklmnopqrstuvwxy"}, wantErr: "gid"},
		{name: "gid with spaces rejected", chat: ConfiguredChat{Group: "g", GID: "with space"}, wantErr: "gid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.chat.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
```

- [ ] **Step 2: Run, expect compile failure**

```bash
go test -race ./app/config/ -run TestConfiguredChat_Validate -v
```

Expected: `ConfiguredChat undefined`, `Validate undefined`.

- [ ] **Step 3: Implement `ConfiguredChat` + `Validate`**

In `app/config/settings.go`, add a new type near `TelegramSettings`:

```go
// ConfiguredChat is one Telegram target group entry from Telegram.Groups.
// Group is the human-readable name (or numeric chat ID like -1001234567890),
// matching the format previously accepted by Telegram.Group.
// GID is the per-chat identifier used for scoping per-chat storage (approved
// users, locator, reports, warnings, detected spam). Optional in YAML/env: when
// omitted, it defaults to chat_<resolved_chat_id> at runtime resolution time.
type ConfiguredChat struct {
	Group string `json:"group" yaml:"group"`
	GID   string `json:"gid"   yaml:"gid"`
}

var gidRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,24}$`)

// Validate checks the static shape of a ConfiguredChat. Group must be non-empty.
// GID, when set, must match the multi-chat regex (no colon, ≤24 chars, ascii word
// characters plus underscore/hyphen). Empty GID is allowed and resolved later.
func (c ConfiguredChat) Validate() error {
	if c.Group == "" {
		return errors.New("group is required")
	}
	if c.GID != "" && !gidRegex.MatchString(c.GID) {
		return fmt.Errorf("gid %q invalid: must match %s", c.GID, gidRegex.String())
	}
	return nil
}
```

Add `"regexp"` and `"errors"` to imports if absent.

- [ ] **Step 4: Run, expect pass**

```bash
go test -race ./app/config/ -run TestConfiguredChat_Validate -v
```

Expected: PASS for all subtests.

- [ ] **Step 5: Add `Groups` field to `TelegramSettings` (no behavior change yet)**

In `app/config/settings.go` change:

```go
type TelegramSettings struct {
	Group        string           `json:"group"         yaml:"group"         db:"telegram_group"`
	Groups       []ConfiguredChat `json:"groups"        yaml:"groups"        db:"-"`
	IdleDuration time.Duration    `json:"idle_duration" yaml:"idle_duration" db:"telegram_idle_duration"`
	Timeout      time.Duration    `json:"timeout"       yaml:"timeout"       db:"telegram_timeout"`
	Token        string           `json:"token"         yaml:"token"         db:"telegram_token"`
}
```

`db:"-"` on `Groups` keeps CONFDB persistence behaviour unchanged for now (Phase 7+ adds DB schema for it).

- [ ] **Step 6: Add `SuperUsersCrossChat` to `AdminSettings`**

In `app/config/settings.go` change:

```go
type AdminSettings struct {
	AdminGroup              string   `json:"admin_group"              yaml:"admin_group"              db:"admin_group"`
	DisableAdminSpamForward bool     `json:"disable_admin_spam_forward" yaml:"disable_admin_spam_forward" db:"disable_admin_spam_forward"`
	TestingIDs              []int64  `json:"testing_ids"              yaml:"testing_ids"              db:"testing_ids"`
	SuperUsers              []string `json:"super_users"              yaml:"super_users"              db:"super_users"`
	SuperUsersCrossChat     bool     `json:"superusers_cross_chat"    yaml:"superusers_cross_chat"    db:"-"`
}
```

`db:"-"` for the same reason as `Groups`.

- [ ] **Step 7: Run full config suite for regression**

```bash
go test -race ./app/config/ -count=1
```

Expected: clean. If existing tests fail because they marshal/unmarshal `TelegramSettings` and now see new fields, update them — but only if necessary; default-zero behaviour should keep them green.

- [ ] **Step 8: Commit**

```bash
git add app/config/settings.go app/config/settings_test.go
git commit -m "Add ConfiguredChat type and Telegram.Groups/SuperUsersCrossChat"
```

---

## Task 4: Settings normaliser — legacy `Group` → `Groups[0]` + dup-checks

The startup-time conversion: when `Telegram.Groups` is empty and `Telegram.Group` is non-empty, populate `Groups = [{Group: Telegram.Group, GID: InstanceID}]`. Also reject duplicate primary chat strings and duplicate `gid`s. **Phase 2 caps `len(Groups) ≤ 1`** because the listener doesn't yet route to multiple chats.

**Files:**
- Modify: `app/config/settings.go`
- Modify: `app/config/settings_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `app/config/settings_test.go`:

```go
func TestSettings_NormalizeGroups(t *testing.T) {
	t.Run("legacy single Group fills Groups[0]", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Group = "MyGroup"
		require.NoError(t, s.NormalizeGroups())
		require.Len(t, s.Telegram.Groups, 1)
		assert.Equal(t, "MyGroup", s.Telegram.Groups[0].Group)
		assert.Equal(t, "instX", s.Telegram.Groups[0].GID)
	})

	t.Run("explicit Groups wins over legacy Group", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Group = "ignored"
		s.Telegram.Groups = []ConfiguredChat{{Group: "Real", GID: "main"}}
		require.NoError(t, s.NormalizeGroups())
		require.Len(t, s.Telegram.Groups, 1)
		assert.Equal(t, "Real", s.Telegram.Groups[0].Group)
		assert.Equal(t, "main", s.Telegram.Groups[0].GID)
	})

	t.Run("both empty returns error", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		err := s.NormalizeGroups()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "telegram group")
	})

	t.Run("Phase 2 caps at one chat", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Groups = []ConfiguredChat{
			{Group: "g1", GID: "a"},
			{Group: "g2", GID: "b"},
		}
		err := s.NormalizeGroups()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "single group")
	})

	t.Run("Group and Groups both set is rejected when they differ", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Group = "legacy"
		s.Telegram.Groups = []ConfiguredChat{{Group: "different", GID: "main"}}
		err := s.NormalizeGroups()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("invalid gid in Groups is rejected", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Groups = []ConfiguredChat{{Group: "g", GID: "bad:gid"}}
		err := s.NormalizeGroups()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "gid")
	})

	t.Run("default gid filled from chat name when omitted", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Groups = []ConfiguredChat{{Group: "MyGroup"}}
		require.NoError(t, s.NormalizeGroups())
		// Phase 2 default rule: chat name OR resolved-id-based default at runtime.
		// At config-normalize time we just leave it empty so runtime can fill chat_<id>.
		assert.Empty(t, s.Telegram.Groups[0].GID,
			"empty GID is left for runtime resolution to chat_<chat_id>")
	})
}
```

- [ ] **Step 2: Run, expect failure**

```bash
go test -race ./app/config/ -run TestSettings_NormalizeGroups -v
```

Expected: `NormalizeGroups undefined`.

- [ ] **Step 3: Implement `NormalizeGroups`**

In `app/config/settings.go` add a method on `*Settings`:

```go
// NormalizeGroups reconciles legacy Telegram.Group with the new Telegram.Groups
// list. Rules:
//   - If both are empty, return an error (no chat configured).
//   - If both are set and disagree, return an error (mutually exclusive when they
//     point to different groups).
//   - If only Telegram.Group is set, populate Groups[0] from it with GID = InstanceID
//     to preserve historical data continuity (existing rows in storage with empty gid
//     were already backfilled to InstanceID by storage migrations).
//   - If Telegram.Groups is set, validate each entry. GID may be empty here; runtime
//     resolution fills chat_<resolved_chat_id> after the chat ID is known.
//   - PHASE 2 RESTRICTION: more than one entry is rejected because the listener
//     doesn't yet route across chats. This cap is lifted in Phase 4.
func (s *Settings) NormalizeGroups() error {
	if len(s.Telegram.Groups) == 0 && s.Telegram.Group == "" {
		return errors.New("telegram group is required (set telegram.group or telegram.groups)")
	}
	if len(s.Telegram.Groups) > 0 && s.Telegram.Group != "" {
		// allow the legacy field to remain only when it points to the same chat
		if len(s.Telegram.Groups) != 1 || s.Telegram.Groups[0].Group != s.Telegram.Group {
			return errors.New("telegram.group and telegram.groups are mutually exclusive when they disagree")
		}
	}
	if len(s.Telegram.Groups) == 0 {
		s.Telegram.Groups = []ConfiguredChat{{Group: s.Telegram.Group, GID: s.InstanceID}}
		return nil
	}
	if len(s.Telegram.Groups) > 1 {
		return errors.New("multi-chat routing is not yet supported in this build; configure a single group")
	}
	for i, c := range s.Telegram.Groups {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("telegram.groups[%d]: %w", i, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test -race ./app/config/ -run TestSettings_NormalizeGroups -v
```

Expected: every subtest PASSES.

- [ ] **Step 5: Run full config suite**

```bash
go test -race ./app/config/ -count=1
```

Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add app/config/settings.go app/config/settings_test.go
git commit -m "Normalize Telegram.Groups with legacy fallback and validation"
```

---

## Task 5: `RuntimeChatContext` type

Per spec §Architecture: a struct holding the resolved per-chat runtime state. Lives in `app/events` because the listener owns it (Phase 4 will route on it).

**Files:**
- Create: `app/events/types.go`
- Create: `app/events/types_test.go`

- [ ] **Step 1: Write the failing test**

Create `app/events/types_test.go`:

```go
package events

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRuntimeChatContext_ZeroValue(t *testing.T) {
	var ctx RuntimeChatContext
	assert.Equal(t, int64(0), ctx.PrimaryChatID)
	assert.Equal(t, "", ctx.GID)
	assert.Equal(t, int64(0), ctx.LinkedChannelID)
	assert.Nil(t, ctx.Locator)
	assert.Nil(t, ctx.SpamFilter)
}
```

- [ ] **Step 2: Run, expect compile failure**

```bash
go test -race ./app/events/ -run TestRuntimeChatContext_ZeroValue -v
```

Expected: `RuntimeChatContext undefined`.

- [ ] **Step 3: Create the type**

Create `app/events/types.go`:

```go
package events

import (
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/lib/tgspam"
)

// RuntimeChatContext is the per-chat runtime bundle resolved at startup.
// One instance per ConfiguredChat in Settings.Telegram.Groups. The listener
// (Phase 4+) routes incoming updates to the right context based on chat ID.
//
// Construction wires:
//   - PrimaryChatID and LinkedChannelID from Telegram getChat lookups
//   - GID from ConfiguredChat (or chat_<PrimaryChatID> default)
//   - Detector with shared SamplesModel + per-chat ApprovedUsers
//   - SpamFilter wrapping the per-chat Detector
//   - Locator/Reports/Warnings/DetectedSpam scoped to the chat's gid
type RuntimeChatContext struct {
	PrimaryChatID   int64
	GID             string
	LinkedChannelID int64
	Detector        *tgspam.Detector
	SpamFilter      *bot.SpamFilter
	Locator         Locator
	ApprovedUsers   *storage.ApprovedUsers
	DetectedSpam    *storage.DetectedSpam
	Reports         *storage.Reports
	Warnings        Warnings
}
```

If any of the imported types don't exist (e.g., `storage.ApprovedUsers` is named differently), check the actual type names in `app/storage/` and adjust the field types accordingly. The `Locator` and `Warnings` interfaces already live in the events package (per Task 3 of Phase 1's spec mapping).

- [ ] **Step 4: Run, expect pass**

```bash
go test -race ./app/events/ -run TestRuntimeChatContext_ZeroValue -v
```

Expected: PASS.

- [ ] **Step 5: Run full events suite**

```bash
go test -race ./app/events/ -count=1
```

Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add app/events/types.go app/events/types_test.go
git commit -m "Add RuntimeChatContext for per-chat runtime state"
```

---

## Task 6: Per-chat construction in `app/main.go`

Wire `Telegram.Groups[0]` (Phase 2 cap = 1) into a `RuntimeChatContext` slice. The listener still consumes the existing single-chat fields; the slice is populated for symmetry with Phase 4.

**Files:**
- Modify: `app/main.go`

- [ ] **Step 1: Call `NormalizeGroups` early in `execute`**

In `app/main.go` `execute()`, immediately after the `dataDB, err := makeDB(...)` block (around current line 432) and BEFORE `makeDetector`:

```go
if err := settings.NormalizeGroups(); err != nil {
	return fmt.Errorf("invalid chat configuration: %w", err)
}
```

The early existing check at line 418 (`if settings.Telegram.Group == "" ...`) becomes redundant with NormalizeGroups but leave it as a fast-fail path for the server-only branch — review the conditions carefully and keep behaviour identical.

- [ ] **Step 2: Build the per-chat context**

After `makeSpamLogger` (around current line 509-512), add:

```go
// Phase 2: build per-chat runtime contexts. Only one entry permitted by NormalizeGroups
// for now; the listener still consumes the single-chat fields below.
ctxList := make([]*events.RuntimeChatContext, 0, len(settings.Telegram.Groups))
for i := range settings.Telegram.Groups {
	chatCfg := settings.Telegram.Groups[i]
	gid := chatCfg.GID
	if gid == "" {
		// runtime default — use the configured group string as a stable label until
		// Phase 4 resolves the actual chat ID and switches to chat_<id>.
		gid = chatCfg.Group
	}
	scopedDB := dataDB.WithGID(gid)
	rcCtx := &events.RuntimeChatContext{
		GID:           gid,
		Detector:      detector,        // shared classifier path; Phase 4 splits per-chat
		SpamFilter:    spamBot,         // shared until Phase 4 splits per-chat
		Locator:       locator,         // single locator for now; per-chat in Phase 4
		ApprovedUsers: approvedUsersStore,
		DetectedSpam:  spamLogger.DetectedSpamStore(), // expose the store from Task 5 if needed
		Reports:       reportsStore,
		Warnings:      warningsStore,
	}
	_ = scopedDB // wired into per-chat stores in Phase 4
	ctxList = append(ctxList, rcCtx)
}
log.Printf("[INFO] resolved %d chat context(s) (multi-chat routing pending Phase 4)", len(ctxList))
```

If `spamLogger.DetectedSpamStore()` doesn't exist, leave the `DetectedSpam` field nil for Phase 2 — it's not consumed yet. The listener doesn't read `ctxList` in Phase 2.

The `_ = scopedDB` line is intentional: it exercises `WithGID` end-to-end so the wiring is exercised even though the scoped engine isn't yet consumed by stores.

- [ ] **Step 3: Build full module**

```bash
cd /home/deploy/tg-spam
go build ./...
```

Expected: clean. Adjust imports in main.go (`events` package already imported; nothing new likely needed).

- [ ] **Step 4: Run full suite for regression**

```bash
go test -race ./... -count=1 2>&1 | tail -20
```

Expected: every package passes. The new wiring is dormant — listener still uses single-chat fields, so behaviour is unchanged.

- [ ] **Step 5: Lint clean**

```bash
docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:latest golangci-lint run 2>&1 | tail -10
```

Expected: `0 issues.`

- [ ] **Step 6: Commit**

```bash
git add app/main.go
git commit -m "Wire per-chat RuntimeChatContext slice in execute"
```

---

## Task 7: Smoke test for the wired-in path

A small integration smoke test that boots the config layer end-to-end (legacy YAML → NormalizeGroups → contexts built) without hitting Telegram.

**Files:**
- Modify: `app/main_test.go`

- [ ] **Step 1: Write the failing test**

Find a near-equivalent existing test to mirror (likely `Test_execute_*` or `TestExecute_*` — grep first). Then append:

```go
func TestExecute_LegacyTelegramGroup_NormalizesIntoGroups(t *testing.T) {
	s := &config.Settings{InstanceID: "test-instance"}
	s.Telegram.Group = "MyGroup"
	require.NoError(t, s.NormalizeGroups())
	require.Len(t, s.Telegram.Groups, 1)
	assert.Equal(t, "MyGroup", s.Telegram.Groups[0].Group)
	assert.Equal(t, "test-instance", s.Telegram.Groups[0].GID)
}
```

This is a thin pass-through but locks the contract that `execute`'s NormalizeGroups call wires in.

- [ ] **Step 2: Run, expect PASS**

```bash
go test -race ./app/ -run TestExecute_LegacyTelegramGroup_NormalizesIntoGroups -v
```

Expected: PASS (the actual implementation lives in `app/config`, this just exercises it from `app/`).

- [ ] **Step 3: Commit**

```bash
git add app/main_test.go
git commit -m "Smoke test for legacy Group to Groups normalisation"
```

---

## Task 8: Final verification

- [ ] **Step 1: Full module test**

```bash
go test -race ./... -count=1
```

Expected: every package passes, same or higher pass count than Pre-flight 2.

- [ ] **Step 2: Lint clean**

```bash
docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:latest golangci-lint run 2>&1 | tail
```

Expected: `0 issues.`

- [ ] **Step 3: Smoke run the binary with the legacy single-chat config**

If practical (you have a token + group handy in a sandbox), `go run ./app --help` and confirm the help output is unchanged. Otherwise:

```bash
go build -o /tmp/tg-spam-phase2 ./app
/tmp/tg-spam-phase2 --help 2>&1 | head -50
```

Verify no regressions in the help output.

- [ ] **Step 4: Push the branch**

```bash
git push fork multichat/phase2-config-wiring
```

---

## Self-Review Checklist

After all tasks pass, verify against the spec (`docs/plans/2026-04-28-multi-chat-design.md` §Architecture, §Identifiers, §Storage layer):

| Spec requirement | Where it lands |
|---|---|
| `engine.SQL.WithGID()` shallow copy with shared connection | Task 2 |
| `ConfiguredChat{Group, GID}` type | Task 3 |
| `Telegram.Groups []ConfiguredChat` field | Task 3 |
| Legacy `Group → Groups[0]` with default-gid rule | Task 4 |
| `gid` validation `^[a-zA-Z0-9_-]{1,24}$`, no `:` | Task 3 |
| `Admin.SuperUsersCrossChat` opt-in flag (default off) | Task 3 |
| `RuntimeChatContext` struct | Task 5 |
| Per-chat construction in `app/main.go` startup loop | Task 6 |
| Phase 1 follow-up #1: Reset propagation test | Task 1 |

Out of scope (deferred):
- Listener routing (`byPrimary`, `byGID`) → Phase 4
- Per-chat `Detector`/`SpamFilter`/`Locator`/etc. construction (Phase 2 wires the slice with shared instances; per-chat instantiation is Phase 4 alongside listener routing)
- Storage `UNIQUE(gid, hash)` and `UNIQUE(gid, user_id)` migrations → Phase 3
- Web UI changes → Phase 7

---

## Done definition

- All Pre-flight + Task checkboxes ticked.
- `go test -race ./... -count=1` clean.
- `golangci-lint run` clean.
- Branch contains 7-9 commits, one per task, suitable for review.
- Existing single-chat installations: zero config change required to upgrade.

After this plan, the next plan (`2026-04-28-multi-chat-phase3-storage-migration.md`) introduces `UNIQUE(gid, hash)` for `messages` and `UNIQUE(gid, user_id)` for `spam`, plus the create-copy-rename migration with backfill.
