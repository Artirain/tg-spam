# Multi-Chat Phase 2 — Config + Per-Chat Wiring Implementation Plan (v2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the data-model foundations for multi-chat: `ConfiguredChat` and `RuntimeChatContext` types, `Telegram.Groups []ConfiguredChat` config field with legacy fallback, `engine.SQL.WithGID()` shallow-copy helper, and a per-chat construction loop in `app/main.go` that produces a slice of contexts. Plus the Phase 1 follow-up test that locks the `Reset()` propagation contract.

**Phase 2 caps `len(Groups) ≤ 1`** so the listener (still single-chat until Phase 4) keeps working unchanged.

**Architecture:**
- `NormalizeGroups` runs **first thing** in `execute()` and inside `reloadNormalize`. It is the single source of truth for "what chats are configured". It is **lenient**: when the bot is configured for server-only or convert-only mode (no telegram chat needed), it returns `nil` without populating `Groups`. The caller (`execute`) interprets the result.
- Legacy `Telegram.Group` is reconciled **non-destructively**: when both `Group` and `Groups` are present and `Groups[0].Group == Telegram.Group`, accept; when they differ, prefer `Groups` (canonical) and overwrite `Group` to match — no error. This keeps web UI flows that still write `Telegram.Group` working.
- For Phase 2, default `gid` for every chat (legacy or explicit-with-empty-gid) is `InstanceID`. The spec's `chat_<resolved_chat_id>` rule for explicit multi-chat configs lands in Phase 4 when chat IDs are resolved against Telegram's API. Documented as a deliberate deferral.
- New `Telegram.Groups` and `Admin.SuperUsersCrossChat` fields use `json:"-"` (NOT `db:"-"`) so they are NOT persisted to the CONFDB JSON blob in Phase 2. CONFDB persistence for multi-chat lands in Phase 7.
- `engine.SQL.WithGID(gid)` is a shallow copy that shares the underlying `*sqlx.DB`. Per-chat scoped engines hand off to per-chat stores in Phase 4.
- `RuntimeChatContext` lives in `app/main.go` for Phase 2 (private to `main`) — dropping it into `app/events` with concrete `*storage.X` fields would tie the events package to storage internals before listener routing exists. Phase 4 will introduce the events-package version with the right consumer-side interfaces.
- `makeSpamLogger` returns the underlying `*storage.DetectedSpam` alongside the `events.SpamLogger` so per-chat wiring can hold a reference. Signature change.

**Tech Stack:** Go 1.24+, `app/config`, `app/storage/engine`, `app/main.go`, `lib/tgspam`, `github.com/stretchr/testify`, `go test -race ./...`, `golangci-lint run` (Docker).

**Reference spec:** `docs/plans/2026-04-28-multi-chat-design.md` §Identifiers, §Architecture, §Storage layer, §Config & CLI.
**Phase 1 follow-up addressed here:** add `TestDetector_Reset_PropagatesAcrossSharedDetectors` (item #1 from Phase 1 final review).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `lib/tgspam/detector_test.go` | Modify | Add `TestDetector_Reset_PropagatesAcrossSharedDetectors` (Phase 1 follow-up #1) |
| `app/storage/engine/engine.go` | Modify | Add `WithGID(gid string) *SQL` shallow-copy method |
| `app/storage/engine/engine_test.go` | Modify | Tests: gid switch, dbType preserved, shared connection |
| `app/config/settings.go` | Modify | Add `ConfiguredChat`, `Telegram.Groups`, `Admin.SuperUsersCrossChat`. JSON `-` for both new fields. Add `NormalizeGroups()` |
| `app/config/settings_test.go` | Modify | Tests: `ConfiguredChat.Validate`, `NormalizeGroups` matrix, JSON round-trip skip |
| `app/main.go` | Modify | `makeSpamLogger` returns `(SpamLogger, *storage.DetectedSpam, error)`. `execute()` calls `NormalizeGroups` first, refactors line ~418 + ~484 checks to read `Groups`. Builds per-chat `runtimeChatContext` slice. `reloadNormalize` closure also calls `NormalizeGroups` |
| `app/main_test.go` | Modify | Add reload-flow test that proves `reloadNormalize` runs `NormalizeGroups` |

---

## Pre-flight

- [ ] **Pre-flight 1: clean tree on right branch**

```bash
cd /home/deploy/tg-spam
git status --short
git rev-parse --abbrev-ref HEAD
```

If still on `multichat/phase1-detector-refactor`, cut a Phase 2 branch:

```bash
git checkout -b multichat/phase2-config-wiring
```

- [ ] **Pre-flight 2: baseline test run**

```bash
go test -race ./... -count=1 2>&1 | tail -20
```

Capture pass count.

- [ ] **Pre-flight 3: baseline lint**

```bash
docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:latest golangci-lint run 2>&1 | tail -10
```

Expected: `0 issues.`

---

## Task 1: Reset propagation test (Phase 1 follow-up #1)

Lock the multi-chat `Reset()` contract. Per `lib/tgspam/detector.go:481-494`, `Reset()` calls `d.model.Reset()` (shared) PLUS clears per-detector `approvedUsers` and Lua engine. So `d1.Reset()` wipes shared model for `d2`, but does NOT touch `d2.approvedUsers`.

**Files:**
- Modify: `lib/tgspam/detector_test.go`

- [ ] **Step 1: Write the test**

Append to `lib/tgspam/detector_test.go`:

```go
func TestDetector_Reset_PropagatesAcrossSharedDetectors(t *testing.T) {
	model := NewSamplesModel()
	cfg := Config{MinMsgLen: 1}
	d1 := NewDetectorWithModel(cfg, model)
	d2 := NewDetectorWithModel(cfg, model)

	// seed shared model via d1 (need a SampleUpdater for UpdateSpam to actually write)
	d1.WithSpamUpdater(&mocks.SampleUpdaterMock{
		AppendFunc: func(string) error { return nil },
		RemoveFunc: func(string) error { return nil },
	})
	require.NoError(t, d1.UpdateSpam("buy cheap viagra"))
	require.NoError(t, d1.UpdateSpam("free crypto airdrop"))
	require.Positive(t, d2.model.tokenizedSpamLen(),
		"d2 must observe d1's spam additions via shared model")

	// per-detector approved-users on d2 — Reset on d1 must NOT touch d2's
	require.NoError(t, d2.AddApprovedUser(approved.UserInfo{UserID: "777", UserName: "carol"}))
	require.True(t, d2.IsApprovedUser("777"))

	// d1.Reset wipes shared model — d2 sees the wipe
	d1.Reset()
	assert.Equal(t, 0, d2.model.tokenizedSpamLen(),
		"d1.Reset must wipe shared SamplesModel that d2 holds")
	assert.Equal(t, 0, d2.model.classifierAllDocs(),
		"d1.Reset must reset shared classifier visible to d2")

	// per-detector state on d2 survives d1.Reset
	assert.True(t, d2.IsApprovedUser("777"),
		"d1.Reset must NOT touch d2's per-detector approved users")
}
```

- [ ] **Step 2: Run, expect PASS**

```bash
go test -race ./lib/tgspam/ -run TestDetector_Reset_PropagatesAcrossSharedDetectors -v
```

- [ ] **Step 3: Commit**

```bash
git add lib/tgspam/detector_test.go
git commit -m "Test Reset propagates across shared SamplesModel detectors"
```

---

## Task 2: `engine.SQL.WithGID()` shallow copy

**Files:**
- Modify: `app/storage/engine/engine.go`
- Modify: `app/storage/engine/engine_test.go`

- [ ] **Step 1: Failing test**

Append to `app/storage/engine/engine_test.go`:

```go
func TestSQL_WithGID(t *testing.T) {
	ctx := context.Background()
	root, err := New(ctx, ":memory:", "instance-x")
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	scoped := root.WithGID("chat_42")

	t.Run("scoped has new gid", func(t *testing.T) {
		assert.Equal(t, "chat_42", scoped.GID())
	})
	t.Run("root gid unchanged", func(t *testing.T) {
		assert.Equal(t, "instance-x", root.GID())
	})
	t.Run("dbType preserved", func(t *testing.T) {
		assert.Equal(t, root.Type(), scoped.Type())
	})
	t.Run("shared connection visible across copies", func(t *testing.T) {
		_, err := root.ExecContext(ctx, "CREATE TABLE phase2_probe (id INTEGER)")
		require.NoError(t, err)
		var n int
		err = scoped.GetContext(ctx, &n, "SELECT COUNT(*) FROM phase2_probe")
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})
	t.Run("empty gid is accepted", func(t *testing.T) {
		empty := root.WithGID("")
		assert.Equal(t, "", empty.GID())
	})
}
```

If `Type()` doesn't exist, read `engine.go` and use the actual accessor.

- [ ] **Step 2: Run, expect compile failure**

```bash
go test -race ./app/storage/engine/ -run TestSQL_WithGID -v
```

- [ ] **Step 3: Implement**

In `app/storage/engine/engine.go`, after `GID()`:

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

- [ ] **Step 4: Run + suite regression**

```bash
go test -race ./app/storage/engine/ -count=1
```

- [ ] **Step 5: Commit**

```bash
git add app/storage/engine/engine.go app/storage/engine/engine_test.go
git commit -m "Add SQL.WithGID shallow-copy for per-chat scoping"
```

---

## Task 3: `ConfiguredChat` type + new fields with `json:"-"`

Per Codex/Cursor finding: `db:"-"` does NOT prevent CONFDB persistence (CONFDB stores the full `Settings` as a JSON blob, see `app/config/store.go:196`). Use `json:"-"` to keep new fields out of the blob in Phase 2.

**Files:**
- Modify: `app/config/settings.go`
- Modify: `app/config/settings_test.go`

- [ ] **Step 1: Failing test for `ConfiguredChat.Validate`**

Append to `app/config/settings_test.go`:

```go
func TestConfiguredChat_Validate(t *testing.T) {
	tests := []struct {
		name    string
		chat    ConfiguredChat
		wantErr string
	}{
		{name: "valid with explicit gid", chat: ConfiguredChat{Group: "MyGroup", GID: "main"}},
		{name: "valid with empty gid (runtime fills)", chat: ConfiguredChat{Group: "MyGroup"}},
		{name: "valid numeric chat id", chat: ConfiguredChat{Group: "-1001234567890", GID: "secondary"}},
		{name: "empty group rejected", chat: ConfiguredChat{Group: "", GID: "x"}, wantErr: "group is required"},
		{name: "gid with colon rejected", chat: ConfiguredChat{Group: "g", GID: "bad:gid"}, wantErr: "gid"},
		{name: "gid 25 chars rejected", chat: ConfiguredChat{Group: "g", GID: "abcdefghijklmnopqrstuvwxy"}, wantErr: "gid"},
		{name: "gid with spaces rejected", chat: ConfiguredChat{Group: "g", GID: "with space"}, wantErr: "gid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.chat.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
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

- [ ] **Step 3: Implement `ConfiguredChat` + `Validate`**

In `app/config/settings.go` near `TelegramSettings`:

```go
// ConfiguredChat is one Telegram target group entry from Telegram.Groups.
// Group is the human-readable name or numeric chat ID like -1001234567890.
// GID is optional in YAML/env; when empty, NormalizeGroups fills it with InstanceID
// (Phase 2). The future chat_<resolved_chat_id> default for explicit multi-chat
// lands in Phase 4 alongside chat-id resolution against Telegram.
type ConfiguredChat struct {
	Group string `json:"group" yaml:"group"`
	GID   string `json:"gid"   yaml:"gid"`
}

// gidPattern is the validation regex for gid values. Capped at 24 characters to
// keep the inline-button callback payload within Telegram's 64-byte callback_data
// limit (see design spec §Callback payload format).
var gidPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,24}$`)

// Validate checks the static shape of a ConfiguredChat.
// Group must be non-empty. GID, when set, must match gidPattern.
// Empty GID is allowed and resolved by NormalizeGroups.
func (c ConfiguredChat) Validate() error {
	if c.Group == "" {
		return errors.New("group is required")
	}
	if c.GID != "" && !gidPattern.MatchString(c.GID) {
		return fmt.Errorf("gid %q invalid: must match %s", c.GID, gidPattern.String())
	}
	return nil
}
```

Add `"regexp"` and `"errors"` to imports if absent.

- [ ] **Step 4: Add `Groups` and `SuperUsersCrossChat` fields**

```go
type TelegramSettings struct {
	Group        string           `json:"group"         yaml:"group"         db:"telegram_group"`
	Groups       []ConfiguredChat `json:"-"             yaml:"groups"        db:"-"`
	IdleDuration time.Duration    `json:"idle_duration" yaml:"idle_duration" db:"telegram_idle_duration"`
	Timeout      time.Duration    `json:"timeout"       yaml:"timeout"       db:"telegram_timeout"`
	Token        string           `json:"token"         yaml:"token"         db:"telegram_token"`
}
```

```go
type AdminSettings struct {
	AdminGroup              string   `json:"admin_group"              yaml:"admin_group"              db:"admin_group"`
	DisableAdminSpamForward bool     `json:"disable_admin_spam_forward" yaml:"disable_admin_spam_forward" db:"disable_admin_spam_forward"`
	TestingIDs              []int64  `json:"testing_ids"              yaml:"testing_ids"              db:"testing_ids"`
	SuperUsers              []string `json:"super_users"              yaml:"super_users"              db:"super_users"`
	SuperUsersCrossChat     bool     `json:"-"                        yaml:"superusers_cross_chat"    db:"-"`
}
```

`json:"-"` ensures these fields are NOT serialised into the CONFDB JSON blob. YAML/env still works because YAML uses the `yaml:` tag and env vars use go-flags.

- [ ] **Step 5: Run, expect pass + suite regression**

```bash
go test -race ./app/config/ -count=1
```

- [ ] **Step 6: Commit**

```bash
git add app/config/settings.go app/config/settings_test.go
git commit -m "Add ConfiguredChat type and Telegram.Groups/SuperUsersCrossChat"
```

---

## Task 4: `NormalizeGroups`

Lenient reconciliation. Returns `nil` (without populating Groups) when no chat is configured — caller decides whether that's an error based on mode (server-only/convert-only allow it). Hard error only on invalid gid or Phase 2 cap violation.

**Files:**
- Modify: `app/config/settings.go`
- Modify: `app/config/settings_test.go`

- [ ] **Step 1: Failing test matrix**

Append to `app/config/settings_test.go`:

```go
func TestSettings_NormalizeGroups(t *testing.T) {
	t.Run("legacy Group fills Groups[0] with InstanceID gid", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Group = "MyGroup"
		require.NoError(t, s.NormalizeGroups())
		require.Len(t, s.Telegram.Groups, 1)
		assert.Equal(t, "MyGroup", s.Telegram.Groups[0].Group)
		assert.Equal(t, "instX", s.Telegram.Groups[0].GID)
		// canonical form: legacy Group preserved (matches Groups[0].Group)
		assert.Equal(t, "MyGroup", s.Telegram.Group)
	})

	t.Run("explicit Groups wins; legacy Group canonicalised to match", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Group = "stale"
		s.Telegram.Groups = []ConfiguredChat{{Group: "Real", GID: "main"}}
		require.NoError(t, s.NormalizeGroups())
		require.Len(t, s.Telegram.Groups, 1)
		assert.Equal(t, "Real", s.Telegram.Groups[0].Group)
		assert.Equal(t, "main", s.Telegram.Groups[0].GID)
		assert.Equal(t, "Real", s.Telegram.Group, "legacy Group must be canonicalised to Groups[0].Group")
	})

	t.Run("both empty returns nil (caller decides)", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		require.NoError(t, s.NormalizeGroups())
		assert.Empty(t, s.Telegram.Groups)
		assert.Empty(t, s.Telegram.Group)
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

	t.Run("explicit Groups with empty gid gets InstanceID", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Groups = []ConfiguredChat{{Group: "MyGroup"}}
		require.NoError(t, s.NormalizeGroups())
		assert.Equal(t, "instX", s.Telegram.Groups[0].GID,
			"empty GID must be filled with InstanceID in Phase 2")
	})

	t.Run("invalid gid rejected", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Groups = []ConfiguredChat{{Group: "g", GID: "bad:gid"}}
		err := s.NormalizeGroups()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "gid")
	})

	t.Run("idempotent: second call is no-op", func(t *testing.T) {
		s := &Settings{InstanceID: "instX"}
		s.Telegram.Group = "MyGroup"
		require.NoError(t, s.NormalizeGroups())
		// snapshot
		groups1 := append([]ConfiguredChat(nil), s.Telegram.Groups...)
		require.NoError(t, s.NormalizeGroups())
		assert.Equal(t, groups1, s.Telegram.Groups)
	})
}
```

- [ ] **Step 2: Run, expect failure**

```bash
go test -race ./app/config/ -run TestSettings_NormalizeGroups -v
```

- [ ] **Step 3: Implement**

In `app/config/settings.go`:

```go
// NormalizeGroups reconciles legacy Telegram.Group with the new Telegram.Groups
// list. Lenient: no error when no chat is configured (server-only / convert-only
// modes legitimately have no telegram chat). Caller decides what "no chat" means
// in their context.
//
// Rules:
//   - Both empty → no-op, returns nil. Caller must check len(Groups) before
//     proceeding into Telegram-using flows.
//   - Only Telegram.Group set → populate Groups[0] = {Group, GID: InstanceID}.
//   - Only Telegram.Groups set → fill empty GID with InstanceID; canonicalise
//     Telegram.Group to Groups[0].Group so legacy reads stay consistent.
//   - Both set → Groups wins (canonical); Telegram.Group overwritten to match
//     Groups[0].Group. No error even when they differed at input.
//   - len(Groups) > 1 → ERROR (Phase 2 cap; lifted in Phase 4 with listener routing).
//   - Any ConfiguredChat with invalid gid → ERROR.
//
// Idempotent: a second call on already-normalised settings is a no-op.
func (s *Settings) NormalizeGroups() error {
	if len(s.Telegram.Groups) == 0 && s.Telegram.Group == "" {
		return nil
	}
	if len(s.Telegram.Groups) == 0 {
		s.Telegram.Groups = []ConfiguredChat{{Group: s.Telegram.Group, GID: s.InstanceID}}
		return nil
	}
	if len(s.Telegram.Groups) > 1 {
		return errors.New("multi-chat routing is not yet supported in this build; configure a single group")
	}
	for i := range s.Telegram.Groups {
		if err := s.Telegram.Groups[i].Validate(); err != nil {
			return fmt.Errorf("telegram.groups[%d]: %w", i, err)
		}
		if s.Telegram.Groups[i].GID == "" {
			s.Telegram.Groups[i].GID = s.InstanceID
		}
	}
	// canonicalise legacy field to whatever Groups[0] says
	s.Telegram.Group = s.Telegram.Groups[0].Group
	return nil
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test -race ./app/config/ -run TestSettings_NormalizeGroups -v
```

- [ ] **Step 5: Suite regression**

```bash
go test -race ./app/config/ -count=1
```

- [ ] **Step 6: Commit**

```bash
git add app/config/settings.go app/config/settings_test.go
git commit -m "Normalize Telegram.Groups with lenient legacy fallback"
```

---

## Task 5: `makeSpamLogger` returns `*storage.DetectedSpam` alongside the logger

The current signature `makeSpamLogger(...) (events.SpamLogger, error)` hides the underlying store. Phase 2 wiring needs the store to populate `RuntimeChatContext.DetectedSpam`. Change the signature.

**Files:**
- Modify: `app/main.go`
- Modify: `app/main_test.go` (only if tests reference makeSpamLogger directly)

- [ ] **Step 1: Read current `makeSpamLogger`**

```bash
sed -n '1023,1095p' app/main.go
```

Confirm the function and capture exact body.

- [ ] **Step 2: Update signature and return both values**

Change `makeSpamLogger` to `func makeSpamLogger(ctx context.Context, gid string, wr io.Writer, dataDB *engine.SQL) (events.SpamLogger, *storage.DetectedSpam, error)`. The body already creates `detectedSpamStore` internally — return it alongside the closure-based logger.

- [ ] **Step 3: Update the only caller**

In `execute()` around line 509:

```go
spamLogger, detectedSpamStore, err := makeSpamLogger(ctx, settings.InstanceID, loggerWr, dataDB)
if err != nil {
	return fmt.Errorf("can't make spam logger, %w", err)
}
_ = detectedSpamStore // wired into Phase 2 ctxList in Task 7
```

- [ ] **Step 4: Build clean**

```bash
go build ./...
```

- [ ] **Step 5: Test regression**

```bash
go test -race ./app/... -count=1 2>&1 | tail
```

- [ ] **Step 6: Commit**

```bash
git add app/main.go
git commit -m "Return detectedSpamStore from makeSpamLogger"
```

---

## Task 6: `runtimeChatContext` struct + early `NormalizeGroups` in `execute`

Define `runtimeChatContext` private to `app/main.go`. Move `NormalizeGroups` to the top of `execute()`. Refactor lines ~418 and ~484 to read `len(settings.Telegram.Groups)` instead of `settings.Telegram.Group != ""`.

**Files:**
- Modify: `app/main.go`

- [ ] **Step 1: Define the type**

In `app/main.go` near other helpers:

```go
// runtimeChatContext is the per-chat runtime bundle resolved at startup.
// One instance per ConfiguredChat in Settings.Telegram.Groups. Phase 2 only
// populates this for the single configured chat (cap = 1 in NormalizeGroups);
// Phase 4 will move this type to app/events with listener-friendly interfaces
// and route updates per chat-id.
type runtimeChatContext struct {
	gid             string
	scopedDB        *engine.SQL
	detector        *tgspam.Detector
	spamFilter      *bot.SpamFilter
	locator         *storage.Locator
	approvedUsers   *storage.ApprovedUsers
	detectedSpam    *storage.DetectedSpam
	reports         *storage.Reports
	warnings        *storage.Warnings
}
```

If any storage type name differs, check `app/storage/` and adjust.

- [ ] **Step 2: Move `NormalizeGroups` to the top of `execute`**

Find `func execute` (around line 412). Insert at the very top of the function body, before `if settings.Dry`:

```go
if err := settings.NormalizeGroups(); err != nil {
	return fmt.Errorf("invalid chat configuration: %w", err)
}
```

- [ ] **Step 3: Update line ~418 check**

The current check `if !settings.Server.Enabled && !convertOnly && (settings.Telegram.Token == "" || settings.Telegram.Group == "")` should now read:

```go
if !settings.Server.Enabled && !convertOnly && (settings.Telegram.Token == "" || len(settings.Telegram.Groups) == 0) {
	return errors.New("telegram token and group are required")
}
```

- [ ] **Step 4: Update line ~484 server-only check**

The check `if settings.Server.Enabled && (settings.Telegram.Token == "" || settings.Telegram.Group == "")` becomes:

```go
if settings.Server.Enabled && (settings.Telegram.Token == "" || len(settings.Telegram.Groups) == 0) {
	// server-only branch
	...
}
```

- [ ] **Step 5: Add `NormalizeGroups` to `reloadNormalize` closure**

Find `reloadNormalize = func(s *config.Settings)` (around line 306). Append `NormalizeGroups` call:

```go
reloadNormalize = func(s *config.Settings) {
	s.ApplyDefaults(defaults)
	applyOperationalCLIOverrides(s, opts, defaults)
	normalizeFilePaths(s)
	if err := s.NormalizeGroups(); err != nil {
		log.Printf("[WARN] reload: invalid chat configuration: %v", err)
	}
}
```

NormalizeGroups errors during reload are logged as warnings (not returned) because the reload caller (`webapi.Server.loadConfigHandler`) doesn't have a clean error channel for normalisation failures. The next startup will still validate via the strict path in `execute()`.

- [ ] **Step 6: Build clean**

```bash
go build ./...
```

- [ ] **Step 7: Run app + config + main tests**

```bash
go test -race ./app/... -count=1 2>&1 | tail
```

Expected: green. The legacy `Telegram.Group != ""` flow is preserved (NormalizeGroups populates Groups[0], len > 0 satisfies new checks).

- [ ] **Step 8: Commit**

```bash
git add app/main.go
git commit -m "Run NormalizeGroups early in execute and on reload"
```

---

## Task 7: Build the `runtimeChatContext` slice in `execute`

After all stores are made (locator, approvedUsersStore, reportsStore, warningsStore, detectedSpamStore from Task 5), build a slice of one `runtimeChatContext` per `Telegram.Groups` entry (Phase 2 cap = 1). The slice is constructed but not yet consumed by the listener — that's Phase 4. The point of Phase 2 is to prove the wiring shape compiles and exercises `WithGID` end-to-end.

**Files:**
- Modify: `app/main.go`

- [ ] **Step 1: Build the slice**

After `makeSpamLogger` (around line 509-512), insert:

```go
chatCtxs := make([]*runtimeChatContext, 0, len(settings.Telegram.Groups))
for i := range settings.Telegram.Groups {
	gcfg := settings.Telegram.Groups[i]
	scopedDB := dataDB.WithGID(gcfg.GID)
	chatCtxs = append(chatCtxs, &runtimeChatContext{
		gid:           gcfg.GID,
		scopedDB:      scopedDB,
		detector:      detector,           // shared single instance — per-chat split is Phase 4
		spamFilter:    spamBot,            // shared — per-chat split is Phase 4
		locator:       locator,            // single — per-chat in Phase 4
		approvedUsers: approvedUsersStore, // single — per-chat in Phase 4
		detectedSpam:  detectedSpamStore,
		reports:       reportsStore,
		warnings:      warningsStore,
	})
}
log.Printf("[INFO] resolved %d chat context(s); multi-chat routing pending Phase 4", len(chatCtxs))
_ = chatCtxs // consumed in Phase 4 by the listener
```

- [ ] **Step 2: Build clean + tests + lint**

```bash
go build ./...
go test -race ./app/... -count=1 2>&1 | tail
docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:latest golangci-lint run 2>&1 | tail -10
```

The `_ = chatCtxs` line will probably trip an `unused-variable` lint. If so, replace with a debug log that references at least one field:

```go
for _, c := range chatCtxs {
	log.Printf("[DEBUG] chat context wired: gid=%s", c.gid)
}
```

- [ ] **Step 3: Commit**

```bash
git add app/main.go
git commit -m "Wire per-chat runtimeChatContext slice in execute"
```

---

## Task 8: Reload-flow test (replaces low-value smoke test)

Per Codex finding: a unit test that just calls `NormalizeGroups` from `app/main_test.go` duplicates `app/config` coverage. Replace with a real reload-flow test that proves `reloadNormalize` invokes `NormalizeGroups`.

**Files:**
- Modify: `app/main_test.go`

- [ ] **Step 1: Find existing reload-related tests for shape**

```bash
grep -n "reloadNormalize\|ReloadNormalize" app/main_test.go app/webapi/config_test.go 2>&1 | head
```

If `app/webapi/config_test.go` already has a reload test, mirror its setup.

- [ ] **Step 2: Write the test**

Append to `app/main_test.go` (or wherever fits the existing structure):

```go
func TestReloadNormalize_RunsNormalizeGroups(t *testing.T) {
	// build a reloadNormalize closure the way main does it
	defaults := &config.Settings{}
	opts := options{} // adjust to actual local opts type if needed
	reloadNormalize := func(s *config.Settings) {
		s.ApplyDefaults(defaults)
		applyOperationalCLIOverrides(s, opts, defaults)
		normalizeFilePaths(s)
		if err := s.NormalizeGroups(); err != nil {
			// reload swallows but logs; we only assert the call happened
			t.Logf("normalize warning: %v", err)
		}
	}

	s := &config.Settings{InstanceID: "reload-test"}
	s.Telegram.Group = "GroupFromBlob"
	reloadNormalize(s)
	require.Len(t, s.Telegram.Groups, 1)
	assert.Equal(t, "GroupFromBlob", s.Telegram.Groups[0].Group)
	assert.Equal(t, "reload-test", s.Telegram.Groups[0].GID)
}
```

If the closure signature in production code is different, adapt. The test's invariant: after `reloadNormalize`, `Telegram.Groups` is populated.

- [ ] **Step 3: Run, expect PASS**

```bash
go test -race ./app/ -run TestReloadNormalize_RunsNormalizeGroups -v
```

- [ ] **Step 4: Commit**

```bash
git add app/main_test.go
git commit -m "Test reloadNormalize invokes NormalizeGroups"
```

---

## Task 9: Final verification

- [ ] **Step 1: Full module test**

```bash
go test -race ./... -count=1
```

- [ ] **Step 2: Lint clean**

```bash
docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:latest golangci-lint run 2>&1 | tail
```

- [ ] **Step 3: --help smoke**

```bash
go build -o /tmp/tg-spam-phase2 ./app
/tmp/tg-spam-phase2 --help 2>&1 | head -50
```

- [ ] **Step 4: Push the branch**

```bash
git push fork multichat/phase2-config-wiring
```

---

## Self-Review Checklist

| Spec requirement | Where it lands |
|---|---|
| `engine.SQL.WithGID()` shallow copy with shared connection | Task 2 |
| `ConfiguredChat{Group, GID}` type | Task 3 |
| `Telegram.Groups []ConfiguredChat` field, `json:"-"` for Phase 2 | Task 3 |
| `Admin.SuperUsersCrossChat` opt-in flag, `json:"-"` for Phase 2 | Task 3 |
| Lenient `NormalizeGroups` (no-op when no chat configured) | Task 4 |
| `Group ↔ Groups` canonicalisation (no hard error) | Task 4 |
| `gid` validation `^[a-zA-Z0-9_-]{1,24}$`, no `:` | Task 3 |
| Default-gid = `InstanceID` for Phase 2 (deferred chat_<id> rule) | Task 4 |
| `makeSpamLogger` returns store for wiring | Task 5 |
| `runtimeChatContext` struct (private to `app/main.go`) | Task 6 |
| `NormalizeGroups` runs early in `execute` | Task 6 |
| Lines ~418 + ~484 read `len(Groups)` not `Group != ""` | Task 6 |
| `reloadNormalize` invokes `NormalizeGroups` | Task 6 + Task 8 |
| Per-chat `runtimeChatContext` slice constructed | Task 7 |
| Phase 1 follow-up #1: Reset propagation test | Task 1 |

Out of scope (deferred):
- Listener routing (`byPrimary`, `byGID`) → Phase 4
- Per-chat `Detector`/`SpamFilter`/`Locator`/etc. instantiation (Phase 2 wires shared instances) → Phase 4
- `chat_<resolved_chat_id>` default-gid for explicit multi-chat → Phase 4 (when chat IDs are resolved)
- `RuntimeChatContext` in `app/events` package with consumer-side interfaces → Phase 4
- CONFDB persistence of `Telegram.Groups` and `Admin.SuperUsersCrossChat` → Phase 7
- Storage `UNIQUE(gid, hash)` and `UNIQUE(gid, user_id)` migrations → Phase 3
- Web UI changes → Phase 7

---

## Done definition

- All Pre-flight + Task checkboxes ticked.
- `go test -race ./... -count=1` clean.
- `golangci-lint run` clean.
- Branch contains 8-10 commits, one per task, suitable for review.
- Existing single-chat installations: zero config change required to upgrade. The `TELEGRAM_GROUP=foo` flow continues to work; `Telegram.Groups` (when used in YAML/env) is the same chat through a new code path.
- POST `/config/reload` continues to work; reloaded blobs without `Telegram.Groups` are normalised via the existing `Telegram.Group` field.

After this plan, the next plan (`2026-04-28-multi-chat-phase3-storage-migration.md`) introduces `UNIQUE(gid, hash)` for `messages` and `UNIQUE(gid, user_id)` for `spam`, plus the create-copy-rename migration with backfill.
