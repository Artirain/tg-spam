# Multi-Chat Phase 4 — Listener Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace single-chat `l.chatID`/`l.adminChatID`/`l.linkedChannelID` plumbing with multi-chat routing via the `runtimeChatContext` slice built in Phase 2. Lift the Phase 2 cap on `Telegram.Groups`. Per-chat `Locator`/`ApprovedUsers`/`Detector`/`SpamFilter` instances. Embed `gid` in inline callback payloads. After this phase, the bot actually moderates multiple chats from a single instance.

**Architecture:**
- `TelegramListener` gains `byPrimary map[int64]*runtimeChatContext` and `byGID map[string]*runtimeChatContext` populated at startup. Fields `chatID`/`linkedChannelID` removed; `adminChatID` stays (shared admin chat). Existing `Group string` legacy field becomes informational.
- `isChatAllowed` → returns `(*runtimeChatContext, bool)`. Lookup in `byPrimary` + `TestingIDs` fallback.
- `isAdminChat` unchanged in shape (still checks `adminChatID`), but admin handler is now ctx-aware.
- `isLinkedChannel` now checks any chat context's `linkedChannelID`.
- `updateSupers` iterates all primary chats only when `Admin.SuperUsersCrossChat == true`. Default off: only static `SuperUsers` from config are global.
- `procEvents`, `procReaction`, `procSuperReply`, `procUserReply` accept `*runtimeChatContext`. Internal calls use ctx fields instead of `l.chatID`.
- `admin`/`userReports` handlers drop `primChatID` struct field. Every method takes `ctx *runtimeChatContext` parameter; routes ban/delete/respond via `ctx.PrimaryChatID()` (or similar accessor on the runtime type).
- `parseCallbackData` accepts both 2-part legacy and 3-part new (`gid:userID:msgID`) format. All inline keyboard builders emit the 3-part form. Legacy callbacks default to `Groups[0]` with a warn-once log.
- Phase 2 cap `len(Groups) > 1 → error` lifted in `NormalizeGroups`.
- Per-chat instantiation: each `runtimeChatContext` builds its own `Locator`, `ApprovedUsers`, `DetectedSpam`, `Reports`, `Warnings` via the scoped engine, and its own `Detector`+`SpamFilter` sharing the global classifier model (per Phase 1 Option A).

**Tech Stack:** Go 1.24+, `app/events`, `app/main.go`, `lib/tgspam`, `app/storage`, `github.com/stretchr/testify`. Tests both single-chat (regression) and multi-chat (new).

**Reference spec:** `docs/plans/2026-04-28-multi-chat-design.md` §Message routing, §SpamFilter / Option A, §Privilege model, §Callback payload format, §Single-chat code paths to update.

**Prior phases status:** Phase 1 (Detector refactor), Phase 2 (Config + per-chat wiring foundations), Phase 3 (Storage UNIQUE constraints) all merged into the multichat branch line. This plan picks up from `multichat/phase3-storage-migration`.

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `app/main.go` | Modify | Per-chat `Locator`/`ApprovedUsers`/`Detector`/`SpamFilter` instantiation in the `chatCtxs` loop. Pass `chatCtxs` into `TelegramListener`. Lift the Phase 2 cap call site is just `NormalizeGroups`; here we need to add wiring |
| `app/config/settings.go` | Modify | Remove the `len(Groups) > 1` rejection in `NormalizeGroups` |
| `app/config/settings_test.go` | Modify | Update the "Phase 2 caps at one chat" test → "any number of chats allowed". Add multi-chat test |
| `app/events/listener.go` | Modify | Replace `chatID`/`linkedChannelID` fields with maps. Update all 30+ use sites. `updateSupers` cross-chat opt-in |
| `app/events/admin.go` | Modify | Drop `primChatID` field; methods take `*runtimeChatContext` parameter. ~25 use sites |
| `app/events/reports.go` | Modify | Same treatment for `userReports`. ~15 use sites |
| `app/events/events.go` | Modify | `parseCallbackData` accepts 2 or 3 parts |
| `app/events/*_test.go` | Modify | Adapt existing single-chat tests to new API. Add multi-chat scenarios |

Note: `runtimeChatContext` is currently private to `app/main.go` (Phase 2 choice). Phase 4 must promote it to `app/events` so the listener and handlers can hold it. Or expose it via a public-facing struct in `app/events`. We'll choose the latter: a new exported `ChatContext` type in `app/events` whose fields are the listener-relevant subset, populated by `main.go` from `runtimeChatContext`. This preserves the `app/main.go` wiring as the single source of truth and avoids importing `app/storage` types into `app/events`.

---

## Pre-flight

- [ ] **Pre-flight 1: branch from phase3**

```bash
cd /home/deploy/tg-spam
git checkout multichat/phase3-storage-migration
git checkout -b multichat/phase4-listener-routing
```

- [ ] **Pre-flight 2: baseline**

```bash
go test -race ./... -count=1 2>&1 | tail -10
docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:latest golangci-lint run 2>&1 | tail -3
```

Both clean.

---

## Task 1: `events.ChatContext` exported type + listener fields

Define a public `events.ChatContext` so the listener and handlers can pass it around without importing `app/main.go`. Wire-it-up details (per-chat Locator, etc.) flow in from `main.go`.

**Files:**
- Create: `app/events/chatctx.go`
- Modify: `app/events/listener.go`

- [ ] **Step 1: Create the type**

`app/events/chatctx.go`:

```go
package events

// ChatContext is the per-chat runtime bundle that the listener routes on.
// One instance per ConfiguredChat in Settings.Telegram.Groups, populated by
// app/main.go startup wiring and passed into TelegramListener via the
// Chats field. Methods on listener/admin/reports accept *ChatContext to
// resolve the right SpamFilter/Locator/ban target without consulting
// listener struct fields.
type ChatContext struct {
	PrimaryChatID   int64
	GID             string
	LinkedChannelID int64
	Bot             Bot
	Locator         Locator
	ApprovedUsers   ApprovedUsersStore
	DetectedSpam    DetectedSpamStore
	Reports         ReportsStore
	Warnings        Warnings
}
```

`ApprovedUsersStore`, `DetectedSpamStore`, `ReportsStore` are consumer-side interfaces — declared in `app/events/` matching the methods the listener and handlers actually call. Existing interfaces (`Bot`, `Locator`, `Warnings`) already live in this package. Add the missing two as minimal interface declarations whose concrete impls live in `app/storage`.

- [ ] **Step 2: Add `Chats []*ChatContext` to `TelegramListener`**

In `app/events/listener.go`, modify the struct. Keep `chatID`/`adminChatID`/`linkedChannelID` as PRIVATE fields for now (they get populated from `Chats[0]` in legacy single-chat mode and removed in later tasks).

```go
type TelegramListener struct {
    // ... existing fields ...

    Chats []*ChatContext  // new — populated by main.go from Settings.Telegram.Groups

    // existing private fields retained for legacy compatibility during migration
    // (they will be removed after all use-sites migrate to Chats lookup):
    byPrimary map[int64]*ChatContext  // chat_id → ctx, derived from Chats at startup
    byGID     map[string]*ChatContext // gid     → ctx, derived from Chats at startup

    // single-chat legacy (removed after migration):
    chatID          int64
    linkedChannelID int64
    adminChatID     int64
}
```

- [ ] **Step 3: Build**

```bash
go build ./...
```

Expected: clean.

- [ ] **Step 4: Commit (intermediate state — non-functional change)**

```bash
git add app/events/chatctx.go app/events/listener.go
git commit -m "Add events.ChatContext type and Chats field"
```

---

## Task 2: Wire `Chats` from `app/main.go`

Convert the private `runtimeChatContext` to populate `events.ChatContext` instances and pass into `TelegramListener.Chats`. Per-chat `Locator` etc. land here too — each chat gets its own scoped instances.

**Files:**
- Modify: `app/main.go`

- [ ] **Step 1: Refactor the per-chat construction loop**

In `execute()`, replace the existing `chatCtxs := make([]*runtimeChatContext, 0, ...)` block with a slice of `*events.ChatContext` and per-chat scoped store instantiation:

```go
chats := make([]*events.ChatContext, 0, len(settings.Telegram.Groups))
for i := range settings.Telegram.Groups {
    gcfg := settings.Telegram.Groups[i]
    scopedDB := dataDB.WithGID(gcfg.GID)

    // per-chat scoped stores
    chatLocator, err := storage.NewLocator(ctx, settings.History.Duration, settings.History.MinSize, scopedDB)
    if err != nil {
        return fmt.Errorf("can't make locator for chat %s: %w", gcfg.GID, err)
    }
    chatApprovedUsers, err := storage.NewApprovedUsers(ctx, scopedDB)
    if err != nil {
        return fmt.Errorf("can't make approved users for chat %s: %w", gcfg.GID, err)
    }
    chatDetectedSpam, err := storage.NewDetectedSpam(ctx, scopedDB)
    if err != nil {
        return fmt.Errorf("can't make detected spam for chat %s: %w", gcfg.GID, err)
    }
    var chatReports *storage.Reports
    if settings.Report.Enabled {
        chatReports, err = storage.NewReports(ctx, scopedDB)
        if err != nil {
            return fmt.Errorf("can't make reports for chat %s: %w", gcfg.GID, err)
        }
    }
    var chatWarnings *storage.Warnings
    if settings.Warn.Threshold > 0 {
        chatWarnings, err = storage.NewWarnings(ctx, scopedDB)
        if err != nil {
            return fmt.Errorf("can't make warnings for chat %s: %w", gcfg.GID, err)
        }
    }

    // per-chat Detector with shared classifier (Phase 1 Option A)
    chatDetector := makeDetector(settings)
    // share the classifier from the root detector to keep samples updates in sync:
    chatDetector.WithSamplesModel(detector.SamplesModel())
    // wire per-chat approved-users storage
    if _, err := chatDetector.WithUserStorage(chatApprovedUsers); err != nil {
        return fmt.Errorf("can't load approved users for chat %s: %w", gcfg.GID, err)
    }

    chatSpamBot, err := makeSpamBot(ctx, settings, scopedDB, chatDetector)
    if err != nil {
        return fmt.Errorf("can't make spam bot for chat %s: %w", gcfg.GID, err)
    }

    chats = append(chats, &events.ChatContext{
        GID:           gcfg.GID,
        Bot:           chatSpamBot,
        Locator:       chatLocator,
        ApprovedUsers: chatApprovedUsers,
        DetectedSpam:  chatDetectedSpam,
        Reports:       chatReports,
        Warnings:      chatWarnings,
    })
}
```

Note: `PrimaryChatID` and `LinkedChannelID` are filled by the listener at `Do()` startup time when it resolves chat IDs against Telegram. Keep them zero at this point.

`SamplesModel()` accessor on `Detector` may not exist — if so, add it as a small read-only getter on `*Detector`. Alternatively, expose `model *SamplesModel` directly via a public getter:

```go
// in lib/tgspam/detector.go:
func (d *Detector) SamplesModel() *SamplesModel { return d.model }
```

And `WithSamplesModel` may not exist either — use the existing `NewDetectorWithModel` constructor instead:

```go
chatDetector := tgspam.NewDetectorWithModel(detector.Config, detector.SamplesModel())
// configure the same checkers
```

Actually simpler: refactor `makeDetector` so it accepts the shared model:

```go
func makeDetector(settings *config.Settings, model *tgspam.SamplesModel) *tgspam.Detector {
    detector := tgspam.NewDetectorWithModel(settings.toDetectorConfig(), model)
    // existing meta-check / OpenAI / Gemini / Lua / CAS wiring goes here
    return detector
}
```

And in `execute()`:

```go
sharedModel := tgspam.NewSamplesModel()
// ... ReloadSamples populates the shared model
detector := makeDetector(settings, sharedModel)
// later, per chat:
chatDetector := makeDetector(settings, sharedModel)
```

Pick the minimal-diff path. The implementation plan must NOT invent symbols that don't exist. Read `lib/tgspam/detector.go` carefully first to see what's already available.

- [ ] **Step 2: Pass `Chats` into TelegramListener**

```go
tgListener := events.TelegramListener{
    // ... existing fields ...
    Chats: chats,
}
```

- [ ] **Step 3: Build**

```bash
go build ./...
```

If `SamplesModel` isn't exposed yet, add the getter and rebuild.

- [ ] **Step 4: Run full suite**

```bash
go test -race ./... -count=1 2>&1 | tail
```

Expected: green. Listener fields still single-chat (chatID/adminChatID/linkedChannelID get populated from Chats[0] in Task 3). Behavior unchanged for single-chat installs.

- [ ] **Step 5: Commit**

```bash
git add app/main.go lib/tgspam/detector.go
git commit -m "Wire per-chat stores into events.ChatContext list"
```

---

## Task 3: Populate listener maps + retain single-chat compat

In `TelegramListener.Do()`, build `byPrimary` and `byGID` from `Chats` at startup. Resolve each chat's `PrimaryChatID` and `LinkedChannelID` against Telegram. Keep the legacy `chatID`/`linkedChannelID`/`adminChatID` fields populated from `Chats[0]` so the rest of the code keeps working until Task 4 migrates each use-site.

**Files:**
- Modify: `app/events/listener.go`

- [ ] **Step 1: In `Do()`, replace the single-chat resolve block**

Find the block around `listener.go:104-128` that resolves `chatID`, `linkedChannelID`, `adminChatID`. Replace with a loop:

```go
l.byPrimary = make(map[int64]*ChatContext, len(l.Chats))
l.byGID = make(map[string]*ChatContext, len(l.Chats))
for _, c := range l.Chats {
    cid, err := l.getChatID(c.GID) // helper takes group identifier — see below
    if err != nil {
        return fmt.Errorf("can't resolve chat %s: %w", c.GID, err)
    }
    c.PrimaryChatID = cid
    if info, err := l.TbAPI.GetChat(tbapi.ChatInfoConfig{ChatConfig: tbapi.ChatConfig{ChatID: cid}}); err == nil && info.LinkedChatID != 0 {
        c.LinkedChannelID = info.LinkedChatID
    }
    l.byPrimary[cid] = c
    l.byGID[c.GID] = c
    log.Printf("[INFO] chat resolved: gid=%s primary=%d linked=%d", c.GID, c.PrimaryChatID, c.LinkedChannelID)
}

// legacy single-chat fallback — first chat populates the deprecated fields
if len(l.Chats) > 0 {
    l.chatID = l.Chats[0].PrimaryChatID
    l.linkedChannelID = l.Chats[0].LinkedChannelID
}

// admin chat resolution unchanged
if l.AdminGroup != "" {
    if l.adminChatID, err = l.getChatID(l.AdminGroup); err != nil {
        return fmt.Errorf("can't resolve admin chat: %w", err)
    }
}
```

`getChatID(c.GID)` is wrong — it should take the original group name (`l.Group` historically). We need to track that on `ChatContext` too. Add a `Group string` field if not already present. Read the existing `getChatID` to confirm the input form.

- [ ] **Step 2: Add `Group` field to `ChatContext`**

```go
type ChatContext struct {
    Group string  // human-readable name or chat ID, used for getChatID lookup at startup
    GID   string
    PrimaryChatID   int64
    LinkedChannelID int64
    Bot           Bot
    Locator       Locator
    ApprovedUsers ApprovedUsersStore
    DetectedSpam  DetectedSpamStore
    Reports       ReportsStore
    Warnings      Warnings
}
```

And in `app/main.go`, pass `Group: gcfg.Group` when constructing the ChatContext.

- [ ] **Step 3: Build + test**

```bash
go build ./...
go test -race ./app/events/... -count=1 2>&1 | tail
```

Single-chat tests should still pass — `Chats[0]` populates the legacy fields, so all existing `l.chatID` references work.

- [ ] **Step 4: Commit**

```bash
git add app/events/listener.go app/events/chatctx.go
git commit -m "Resolve and index per-chat contexts at startup"
```

---

## Task 4: Migrate listener use-sites to `byPrimary` lookup

Walk through every `l.chatID` reference in `listener.go` (per the survey: 30+ sites) and replace with ctx-lookup. Tests will keep passing because `byPrimary[l.chatID]` always returns `Chats[0]` in single-chat mode.

**Files:**
- Modify: `app/events/listener.go`

This task is large but mechanical. Group related changes into sub-commits if helpful.

- [ ] **Step 1: `isChatAllowed` returns ctx**

```go
func (l *TelegramListener) isChatAllowed(fromChat int64) (*ChatContext, bool) {
    if c, ok := l.byPrimary[fromChat]; ok {
        return c, true
    }
    if slices.Contains(l.TestingIDs, fromChat) {
        return nil, true // testing channels have no ctx; legacy single-chat use
    }
    return nil, false
}
```

Update all callers — they need to handle the ctx return.

- [ ] **Step 2: `isLinkedChannel` checks any ctx**

```go
func (l *TelegramListener) isLinkedChannel(msg *tbapi.Message) bool {
    if msg == nil || msg.SenderChat == nil || msg.SenderChat.ID == 0 {
        return false
    }
    for _, c := range l.Chats {
        if c.LinkedChannelID != 0 && c.LinkedChannelID == msg.SenderChat.ID {
            return true
        }
    }
    return false
}
```

- [ ] **Step 3: `procEvents`, `procReaction`, etc. take ctx**

Signature change:

```go
func (l *TelegramListener) procEvents(ctx context.Context, c *ChatContext, update tbapi.Update) error { ... }
func (l *TelegramListener) procReaction(ctx context.Context, c *ChatContext, r *tbapi.MessageReactionUpdated) error { ... }
```

Inside the method body, every `l.chatID` → `c.PrimaryChatID`, every `l.linkedChannelID` → `c.LinkedChannelID`, etc.

- [ ] **Step 4: Update `Do()` dispatcher**

In the main update loop, do the chat lookup once and pass ctx down:

```go
if update.Message != nil {
    if c, ok := l.isChatAllowed(update.Message.Chat.ID); ok && c != nil {
        if err := l.procEvents(ctx, c, update); err != nil { ... }
        continue
    }
}
```

- [ ] **Step 5: `StartupMsg` per multi-mode rule**

```go
if l.StartupMsg != "" && !l.TrainingMode && !l.Dry {
    if len(l.Chats) == 1 {
        // legacy single-chat behavior
        l.sendBotResponse(bot.Response{Send: true, Text: l.StartupMsg}, l.Chats[0].PrimaryChatID, NotificationSilent)
    } else {
        log.Printf("[WARN] startup message ignored in multi-chat mode")
    }
}
```

- [ ] **Step 6: `updateSupers` honour cross-chat flag**

```go
func (l *TelegramListener) updateSupers() error {
    if !l.SuperUsersCrossChat {
        // single-chat mode (or explicit opt-out): only resolve the first chat's admins
        if len(l.Chats) == 0 {
            return nil
        }
        return l.appendChatAdmins(l.Chats[0].PrimaryChatID)
    }
    // cross-chat: iterate all
    for _, c := range l.Chats {
        if err := l.appendChatAdmins(c.PrimaryChatID); err != nil {
            return fmt.Errorf("update supers for chat %d: %w", c.PrimaryChatID, err)
        }
    }
    return nil
}

func (l *TelegramListener) appendChatAdmins(chatID int64) error {
    // factor out the existing isSuper / GetChatAdministrators / append loop
    ...
}
```

Add `SuperUsersCrossChat bool` to `TelegramListener` struct. `main.go` populates from `settings.Admin.SuperUsersCrossChat`.

- [ ] **Step 7: Build + test**

```bash
go build ./...
go test -race ./app/events/... -count=1 2>&1 | tail -20
```

Existing tests should still pass — single-chat mode threads `Chats[0]` through every site, semantically identical to the old `l.chatID`.

- [ ] **Step 8: Remove the legacy `chatID`/`linkedChannelID` struct fields**

Now safe — every use-site goes through ctx.

```bash
grep -n 'l\.chatID\|l\.linkedChannelID' app/events/listener.go
```

Should be empty. If any remains, fix it.

Update the struct definition to remove the two fields.

- [ ] **Step 9: Final listener test run**

```bash
go test -race ./app/events/... -count=1
```

- [ ] **Step 10: Commit**

```bash
git add app/events/listener.go app/events/chatctx.go
git commit -m "Migrate listener use-sites to ChatContext lookup"
```

---

## Task 5: Refactor `admin` handler (drop `primChatID` field, take ctx)

Mirror Task 4 for `app/events/admin.go`. ~25 use-sites.

**Files:**
- Modify: `app/events/admin.go`

- [ ] **Step 1: Change admin handler methods to take `*ChatContext`**

Every method that currently uses `a.primChatID` gains a `c *ChatContext` parameter. Method signatures change. Callers in `listener.go` pass the ctx threaded down from the dispatcher.

- [ ] **Step 2: Update callback handler to resolve ctx from payload**

`InlineCallbackHandler` (admin.go) parses callback data. After Task 7 updates `parseCallbackData`, the parser returns `(gid, userID, msgID, err)`. Look up `c := l.byGID[gid]` and pass into the unban/info flow.

Caveat: the callback handler is currently on `admin`, not `TelegramListener` — it needs access to `byGID`. Either:
- Pass `byGID` map into `admin` constructor (preferred — keeps consumer-side state in the handler)
- Or expose a `*TelegramListener` getter for the chat (worse — circular dependency)

Pick the first option. Add `byGID map[string]*ChatContext` field to `admin` struct.

- [ ] **Step 3: Drop `primChatID` field from `admin`**

After all use-sites migrated, remove `primChatID` from the struct definition.

- [ ] **Step 4: Build + test**

```bash
go build ./...
go test -race ./app/events/... -count=1 2>&1 | tail
```

- [ ] **Step 5: Commit**

```bash
git add app/events/admin.go app/events/listener.go
git commit -m "Migrate admin handler to ChatContext parameter"
```

---

## Task 6: Refactor `userReports` handler (drop `primChatID`, take ctx)

Same pattern as Task 5 but for `app/events/reports.go`. ~15 use-sites.

**Files:**
- Modify: `app/events/reports.go`

Steps mirror Task 5 — method signatures gain `c *ChatContext`, callback handler uses `byGID` lookup, `primChatID` field removed.

- [ ] **Build + test + commit**

```bash
git add app/events/reports.go app/events/listener.go
git commit -m "Migrate reports handler to ChatContext parameter"
```

---

## Task 7: Callback payload format with gid

Extend `parseCallbackData` to accept 2-part legacy and 3-part new format. Update all 6 inline-keyboard builders to emit `gid:userID:msgID` form.

**Files:**
- Modify: `app/events/events.go`
- Modify: `app/events/admin.go`
- Modify: `app/events/reports.go`
- Modify: `app/events/events_test.go`

- [ ] **Step 1: Extend `parseCallbackData`**

```go
// parseCallbackData parses callback data. Supports two formats:
//   legacy 2-part: [prefix]userID:msgID                  → returns gid=""
//   new 3-part:    [prefix]gid:userID:msgID              → returns explicit gid
//
// Callers receiving gid="" must default to Groups[0] and (in multi-chat mode) log
// a one-time warning that a legacy callback was processed.
func parseCallbackData(data string) (gid string, userID int64, msgID int, err error) {
    if len(data) < 3 {
        return "", 0, 0, fmt.Errorf("unexpected callback data, too short %q", data)
    }
    // strip known prefixes
    if data[:1] == "R" {
        data = data[2:] // two-char report prefix
    } else if data[:1] == "?" || data[:1] == "+" || data[:1] == "!" {
        data = data[1:]
    }
    parts := strings.Split(data, ":")
    switch len(parts) {
    case 2:
        // legacy form
    case 3:
        gid = parts[0]
        parts = parts[1:]
    default:
        return "", 0, 0, fmt.Errorf("unexpected callback data, want 2 or 3 parts, got %d: %q", len(parts), data)
    }
    if userID, err = strconv.ParseInt(parts[0], 10, 64); err != nil {
        return "", 0, 0, fmt.Errorf("failed to parse userID %q: %w", parts[0], err)
    }
    if msgID, err = strconv.Atoi(parts[1]); err != nil {
        return "", 0, 0, fmt.Errorf("failed to parse msgID %q: %w", parts[1], err)
    }
    return gid, userID, msgID, nil
}
```

- [ ] **Step 2: Update all 6 inline-keyboard sites**

Per the survey:
- `admin.go:798` (callbackBanConfirm)
- `admin.go:1132-1134` (sendWithUnbanMarkup)
- `reports.go:479-481` (notifyNewReport)
- `reports.go:556-558` (updateNotification)
- `reports.go:855-857` (validateReportRejection)
- `reports.go:885-887` (validateBanReporter)

Each currently emits `fmt.Sprintf("%d:%d", userID, msgID)` or `fmt.Sprintf("R+%d:%d", ...)` etc. Update to `fmt.Sprintf("%s:%d:%d", c.GID, userID, msgID)` / `fmt.Sprintf("R+%s:%d:%d", c.GID, ...)`. Each site has the `c` ctx available because the handler method now takes it.

- [ ] **Step 3: Update all consumers of `parseCallbackData`**

Callers now get 4 return values. Where gid is empty (legacy callback), default to `Groups[0]`:

```go
gid, userID, msgID, err := parseCallbackData(data)
if err != nil { ... }
var c *ChatContext
if gid == "" {
    if len(a.Chats) > 0 {
        c = a.Chats[0]  // legacy fallback
        if len(a.Chats) > 1 {
            log.Printf("[WARN] legacy callback without gid, defaulting to chat %s", c.GID)
        }
    }
} else {
    c = a.byGID[gid]
    if c == nil {
        return fmt.Errorf("unknown gid in callback: %q", gid)
    }
}
```

- [ ] **Step 4: Update `parseCallbackData` tests**

In `events_test.go`, existing tests check 2-part format. Add subtests for 3-part. Existing assertions on 2-part still work — function preserves legacy compat.

- [ ] **Step 5: Build + test**

```bash
go build ./...
go test -race ./app/events/... -count=1 2>&1 | tail
```

- [ ] **Step 6: Commit**

```bash
git add app/events/events.go app/events/admin.go app/events/reports.go app/events/events_test.go
git commit -m "Embed gid in inline callback payloads"
```

---

## Task 8: Lift Phase 2 cap + multi-chat tests

Remove the `len(Groups) > 1` rejection. Add multi-chat tests that exercise actual routing.

**Files:**
- Modify: `app/config/settings.go`
- Modify: `app/config/settings_test.go`
- Modify: `app/events/listener_test.go` (or a new file)

- [ ] **Step 1: Lift cap in `NormalizeGroups`**

In `app/config/settings.go`, remove the block:

```go
if len(s.Telegram.Groups) > 1 {
    return errors.New("multi-chat routing is not yet supported in this build; configure a single group")
}
```

- [ ] **Step 2: Update `TestSettings_NormalizeGroups`**

The "Phase 2 caps at one chat" subtest must flip to "multi-chat permitted":

```go
t.Run("multi-chat permitted", func(t *testing.T) {
    s := &Settings{InstanceID: "instX"}
    s.Telegram.Groups = []ConfiguredChat{
        {Group: "g1", GID: "a"},
        {Group: "g2", GID: "b"},
    }
    require.NoError(t, s.NormalizeGroups())
    assert.Len(t, s.Telegram.Groups, 2)
})
```

Plus a new test for invariant: all entries must have unique gids:

```go
t.Run("duplicate gid rejected", func(t *testing.T) {
    s := &Settings{InstanceID: "instX"}
    s.Telegram.Groups = []ConfiguredChat{
        {Group: "g1", GID: "same"},
        {Group: "g2", GID: "same"},
    }
    err := s.NormalizeGroups()
    require.Error(t, err)
    assert.Contains(t, err.Error(), "duplicate gid")
})
```

Implement the duplicate check in `NormalizeGroups`:

```go
seen := make(map[string]bool, len(s.Telegram.Groups))
for i := range s.Telegram.Groups {
    if err := s.Telegram.Groups[i].Validate(); err != nil { ... }
    if s.Telegram.Groups[i].GID == "" {
        s.Telegram.Groups[i].GID = s.InstanceID
    }
    if seen[s.Telegram.Groups[i].GID] {
        return fmt.Errorf("duplicate gid %q in telegram.groups", s.Telegram.Groups[i].GID)
    }
    seen[s.Telegram.Groups[i].GID] = true
}
```

Also reject duplicate Group strings.

- [ ] **Step 3: Add listener routing test**

Add to `app/events/listener_test.go` (or new file `listener_multichat_test.go`):

```go
func TestTelegramListener_RoutesToMatchingChatContext(t *testing.T) {
    // build two chat contexts with distinct primary chat IDs
    c1 := &ChatContext{Group: "g1", GID: "chat1", PrimaryChatID: 101, ...}
    c2 := &ChatContext{Group: "g2", GID: "chat2", PrimaryChatID: 202, ...}
    l := TelegramListener{Chats: []*ChatContext{c1, c2}, ...}
    // simulate startup byPrimary build (or call l.Do briefly via test scaffold)
    l.byPrimary = map[int64]*ChatContext{101: c1, 202: c2}

    // message to chat 101 → c1; message to 202 → c2; message to 999 → nil/skip
    c, ok := l.isChatAllowed(101)
    require.True(t, ok)
    assert.Equal(t, c1, c)

    c, ok = l.isChatAllowed(202)
    require.True(t, ok)
    assert.Equal(t, c2, c)

    _, ok = l.isChatAllowed(999)
    assert.False(t, ok)
}
```

- [ ] **Step 4: Build + test**

```bash
go build ./...
go test -race ./... -count=1 2>&1 | tail
```

- [ ] **Step 5: Commit**

```bash
git add app/config/settings.go app/config/settings_test.go app/events/listener_test.go
git commit -m "Lift Phase 2 cap and add multi-chat routing tests"
```

---

## Task 9: Final verification

- [ ] **Step 1: Full module test**

```bash
go test -race ./... -count=1
```

- [ ] **Step 2: Lint**

```bash
docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:latest golangci-lint run 2>&1 | tail
```

- [ ] **Step 3: --help smoke**

```bash
go build -o /tmp/tg-spam-p4 ./app && /tmp/tg-spam-p4 --help 2>&1 | head -40
```

- [ ] **Step 4: Push branch**

```bash
git push fork multichat/phase4-listener-routing
```

---

## Self-Review Checklist

| Spec requirement | Where it lands |
|---|---|
| `byPrimary map[int64]*ChatContext` | Task 1 + Task 3 |
| `byGID map[string]*ChatContext` for callback routing | Task 1 + Task 3 |
| Per-chat `Locator`/`ApprovedUsers`/`Detector`/`SpamFilter` | Task 2 |
| `procEvents`/`procReaction`/etc. accept ctx | Task 4 |
| `StartupMsg` per single/multi mode | Task 4 Step 5 |
| `updateSupers` honour cross-chat opt-in | Task 4 Step 6 |
| `admin`/`userReports` drop `primChatID`, take ctx | Tasks 5, 6 |
| `parseCallbackData` accepts 2 or 3 parts | Task 7 |
| Inline keyboards embed gid in payload | Task 7 |
| Lift Phase 2 cap | Task 8 |
| Multi-chat routing test | Task 8 |

Out of scope:
- Observability (`[gid=X]` log prefix) → Phase 5 (or could land here as polish — see comment in spec)
- Web UI selector → Phase 6
- Manual forward disable in multi-mode → Phase 5 (handler-level)

---

## Done definition

- All Pre-flight + Task checkboxes ticked.
- `go test -race ./... -count=1` clean.
- `golangci-lint run` clean.
- Branch contains 8-10 commits.
- Single-chat installs upgrade transparently — `TELEGRAM_GROUP=foo` continues to work.
- Multi-chat install (2+ groups) actually moderates each chat independently with shared admin chat.

After this plan, Phases 5 (handlers polish: manual-forward disable, logging), 6 (observability), and 7 (Web UI) remain.
