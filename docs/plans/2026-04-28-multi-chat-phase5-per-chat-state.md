# Phase 5: Per-Chat Bot/Locator/SpamLogger Migration

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development.

**Goal:** Complete the multi-chat refactor by migrating listener/admin/reports handler bodies from globally-shared `l.Bot`/`l.Locator`/`l.SpamLogger` (and corresponding `a.bot`/`r.locator` etc.) to the per-chat instances carried on `*ChatContext`.

**Why this matters:** After Phase 4, routing decides WHICH chat a message belongs to, but handler bodies still call shared state. In multi-chat mode this means every chat shares the same Detector/Locator/SpamLogger — defeating per-chat isolation. Phase 5 closes that gap.

**Architecture:** `*ChatContext` already carries `Bot`/`Locator`/`SpamLogger`/`ApprovedUsers`/`DetectedSpam`/`Reports`/`Warnings` (Phase 2 wired them, Phase 4 threaded ctx into methods). Phase 5 substitutes call sites from `l.X` / `a.X` / `r.X` to `c.X` where ctx is in scope. Listener-level fields stay as legacy fallback when ctx is nil (tests).

**Tech Stack:** Go 1.24+, sqlite, tbapi.

---

## Pre-flight

- [ ] **Confirm branch and clean state**

```bash
cd /home/deploy/tg-spam
git status   # expect clean
git branch --show-current  # expect multichat/phase5-per-chat-state (create from phase4 tip)
git log --oneline -3
```

- [ ] **Branch from phase 4 tip**

```bash
git checkout -b multichat/phase5-per-chat-state multichat/phase4-listener-routing
```

---

## Task 1: Migrate listener.go to use c.X instead of l.X

**Files:**
- Modify: `app/events/listener.go`

**Sites to migrate** (all inside methods that already take `c *ChatContext`):

| Line | Current | Replacement |
|---|---|---|
| 462 | `l.Locator.AddMessage(ctx, ...)` | `c.Locator.AddMessage(ctx, ...)` |
| 474 | `l.Bot.OnMessage(*msg, false)` | `c.Bot.OnMessage(*msg, false)` |
| 492 | `l.SpamLogger.Save(msg, &resp)` | `c.SpamLogger.Save(msg, &resp)` |
| 497 | `l.Locator.AddSpam(...)` | `c.Locator.AddSpam(...)` |
| 645 (procNewChatMemberMessage) | `l.Locator.AddMessage(...)` | `c.Locator.AddMessage(...)` |
| 675 (procLeftChatMemberMessage) | `l.Locator.Message(...)` | `c.Locator.Message(...)` |
| 946 (procReaction) | `l.Bot.OnReaction(...)` | `c.Bot.OnReaction(...)` |
| 955 | `l.Locator.AddSpam(...)` | `c.Locator.AddSpam(...)` |
| 958 | `l.SpamLogger.Save(...)` | `c.SpamLogger.Save(...)` |

**Idle path** (line 399 `l.Bot.OnMessage(... idle ...)`) stays on `l.Bot` — idle is only fired in single-chat mode (per Task 4 Step 5), where `l.Bot == l.Chats[0].Bot`.

**Legacy fallback**: when `c == nil` (tests that don't populate Chats), fall back to `l.X`. Pattern:

```go
locator := c.Locator
if locator == nil {
    locator = l.Locator
}
```

Or simpler: keep a `(l *TelegramListener) ctxOrLegacy(c *ChatContext) *ChatContext` helper that returns `legacyChatContext()` (already exists from Task 4) when c is nil, so all per-method use becomes `c.X` after a single guard at top of method. **Use the helper** for cleanliness.

**Steps:**

- [ ] **Step 1**: At top of each migrated method (procEvents, procReaction, procNewChatMemberMessage, procLeftChatMemberMessage), if c is nil, set `c = l.legacyChatContext()` immediately after the existing nil-c handling. Verify `legacyChatContext` already populates Bot/Locator/SpamLogger/etc. If it doesn't, extend it to pull from `l.Bot`/`l.Locator`/`l.SpamLogger` fields. **Critical**: `legacyChatContext` currently only sets Group/GID/PrimaryChatID/LinkedChannelID — extend it.

- [ ] **Step 2**: Substitute the 9 sites above with `c.X`.

- [ ] **Step 3**: Build + test.

```bash
go build ./...
go test -race ./app/events/... -count=1
```

- [ ] **Step 4**: Commit.

```bash
git commit -m "Use per-chat Bot/Locator/SpamLogger in listener"
```

---

## Task 2: Migrate admin.go to use c.X instead of a.X

**Files:**
- Modify: `app/events/admin.go`

**Sites** (all inside methods that already take `c *ChatContext`):

`a.bot.X` → `c.Bot.X` for: OnMessage, AddApprovedUser, RemoveApprovedUser, UpdateSpam, UpdateHam (17 occurrences per grep).

`a.locator.X` → `c.Locator.X` for: Message, Spam, GetUserMessageIDs, UserNameByID (5 occurrences).

`a.spamLogger.X` if any → `c.SpamLogger.X`.

`a.warnings` (Warnings interface) → `c.Warnings` where method takes ctx.

`a.approvedUsers` if used → `c.ApprovedUsers`.

**Pattern**: same legacy fallback — at top of method, if c is nil set c to a synthetic ChatContext built from `a.bot`, `a.locator`, etc.

```go
// admin-side legacy fallback for tests
func (a *admin) legacyChatContext() *ChatContext {
    if len(a.chats) > 0 {
        return a.chats[0]
    }
    return &ChatContext{
        Bot:           a.bot,
        Locator:       a.locator,
        Warnings:      a.warnings,
        // ... etc
    }
}
```

But: `a.bot`/`a.locator` are still parameters in admin struct construction. **Keep them** for the legacy fallback path. Phase 4's adminChats synthesis at listener.go:184-189 already populates a ChatContext, but it doesn't carry Bot/Locator/etc — extend it to do so.

**Cleanest approach**: extend the synthesis in `listener.go:184-189` to also fill `Bot`, `Locator`, `SpamLogger`, `ApprovedUsers`, `DetectedSpam`, `Reports`, `Warnings` from `l.X`. Then admin.go methods can ALWAYS rely on `c.X` and tests that pass nil ctx fall through to the synthesized ctx via `a.defaultChat()` → which is `a.chats[0]` → which is now the populated synthetic ctx.

**Step-by-step:**

- [ ] **Step 1**: Extend `adminChats` synthesis in listener.go:184-189 to populate Bot/Locator/SpamLogger/ApprovedUsers/DetectedSpam/Reports/Warnings from `l.X`. Same for reports synthesis (Task 6 reused adminChats).

- [ ] **Step 2**: In admin.go, substitute `a.bot.X` → `c.Bot.X` and `a.locator.X` → `c.Locator.X` etc. throughout migrated methods.

- [ ] **Step 3**: Build + test + lint.

```bash
go build ./...
go test -race ./app/events/... -count=1
docker run --rm -v $(pwd):/app -w /app golangci/golangci-lint:latest golangci-lint run ./app/events/...
```

- [ ] **Step 4**: Commit.

```bash
git commit -m "Use per-chat Bot/Locator in admin handler"
```

---

## Task 3: Migrate reports.go to use c.X instead of r.X

**Files:**
- Modify: `app/events/reports.go`

Same pattern as Task 2 (22 occurrences per grep).

- [ ] **Step 1**: Substitute call sites.
- [ ] **Step 2**: Build + test + lint.
- [ ] **Step 3**: Commit.

```bash
git commit -m "Use per-chat Bot/Locator in reports handler"
```

---

## Task 4: Final verification

- [ ] `go test -race ./... -count=1 -timeout 10m`
- [ ] `docker run --rm -v $(pwd):/app -w /app golangci/golangci-lint:latest golangci-lint run`
- [ ] `go build -o /tmp/tg-spam-p5 ./app && /tmp/tg-spam-p5 --help | head -10`
- [ ] `git push fork multichat/phase5-per-chat-state`

---

## Out of scope

- Phase 6: `[gid=X]` log prefix everywhere (separate)
- Phase 7: Web UI multi-chat selector
- Removing `l.chatID`/`l.linkedChannelID` legacy fields (defer; doing so requires re-auditing the synthesis paths)
- Removing handler-level `a.bot`/`a.locator`/etc fields (defer; they're still needed for the legacy fallback synthesis)
