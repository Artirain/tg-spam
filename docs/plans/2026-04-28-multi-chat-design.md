# Multi-Chat Support — Design Spec (v2.2)

**Status:** Draft v2.2 (post-review round 3)
**Date:** 2026-04-28
**Issue:** [#100](https://github.com/umputun/tg-spam/issues/100) (closed; fresh PR planned)

## Goal

Single `tg-spam` instance monitors and moderates **multiple Telegram target groups** simultaneously, sharing **one admin chat** (current model, extended). Single-group behavior unchanged.

## Non-goals (v1)

- Per-chat overrides of detection thresholds, training mode, ban duration.
- Per-chat samples / dictionary / super-users.
- Cross-chat ban propagation (ban only in source chat).
- Multiple admin chats.
- **Manual forward of spam to admin chat across groups** (see Forward Routing section — out of scope, replaced by callback-only path).

## Tenancy matrix

| Data / setting | Storage scope | Runtime scope |
|---|---|---|
| Detection thresholds, OpenAI/Gemini config, model files | Global config | Global |
| Classifier in-memory model (token tables loaded from samples) | n/a (built from `samples` table) | **Single shared instance**, pointer held by every per-chat `Detector` |
| `samples` table | Schema has `gid` but used as **global** via canonical `rootGID = InstanceID` | Single shared store |
| `dictionary` table | Same as samples — global via `rootGID` | Single shared store |
| `SuperUsers` (static list from config/env) | Global | Global |
| Chat admins auto-promoted into SuperUsers | **Disabled by default in multi-chat mode** (see Privilege section) | n/a |
| `approved_users` table | **Per-chat (`gid`)** | **Per-chat `Detector` instance** owns the `approvedUsers` map and per-chat `userStorage` |
| `Detector.approvedUsers`, `duplicateDetector`, `reactionDetector`, `hamHistory`, `spamHistory` | n/a (in-memory) | **Per-chat** (one `Detector` per `RuntimeChatContext`) |
| `messages` table (locator) | **Per-chat (`gid`)** | **Per-chat scoped Locator** |
| `spam` table (locator) | **Per-chat (`gid`)** | **Per-chat scoped Locator** |
| `reports` table | **Per-chat (`gid`)** | **Per-chat scoped Reports store** |
| `warnings` table | **Per-chat (`gid`)** | **Per-chat scoped Warnings store** |
| `detected_spam` table | **Per-chat (`gid`)** | **SpamLogger writes with the chat's `gid`** |
| Linked channel ID | Per-chat | Per-chat |
| Admin chat | Single | Single |
| `dmUsers` (recent DM senders) | n/a (in-memory) | Global (DMs are bot-token-level, not chat-level) |

**Storage tenancy ≠ runtime tenancy.** Both must be addressed. The rest of this spec covers runtime wiring explicitly.

## Identifiers

- `InstanceID` — instance-level id from `settings.InstanceID`. Continues to identify the instance (used for `rootGID` of global tables, full-DB backup name, etc.).
- `gid` — per-chat group id (string, `^[a-zA-Z0-9_-]{1,24}$`, no `:`). Each `ConfiguredChat` has one.
- **Default-gid rule** (single source of truth):
  - Legacy single-chat upgrade (`Telegram.Group != ""` and `Telegram.Groups` empty) → the implicit `Groups[0].GID = InstanceID`. This preserves existing data continuity (`gid=''` rows backfilled to `InstanceID` by today's `locator.go:186-194` migration).
  - Explicit multi-chat config (operator writes `Telegram.Groups`) — if `gid` field omitted on a chat, default to `chat_<resolved_chat_id>` (a string, base10, with `chat_` prefix to keep regex compliance and human readability).

The 24-char cap is a hard limit driven by callback payload sizing (see Callback Format section); validator rejects anything longer.

`gid` lifetime is **stable across restarts**. Changing a chat's `gid` in config orphans existing per-chat rows for that chat — documented as a hard rename procedure.

## Architecture

### Two layers

```
ConfiguredChat   (config.TelegramSettings.Groups[N])
    Group string   // human-readable name OR -100... ID, same as TELEGRAM_GROUP today
    GID   string   // optional; defaults to chat_<resolved_chat_id>

RuntimeChatContext  (resolved at startup, one per ConfiguredChat)
    PrimaryChatID    int64
    GID              string
    LinkedChannelID  int64
    ScopedEngine     *engine.SQL                // engine.WithGID(gid) shallow copy
    Locator          *storage.Locator           // bound to ScopedEngine
    ApprovedUsers    *storage.ApprovedUsersStore
    DetectedSpam     *storage.DetectedSpam
    Reports          *storage.Reports
    Warnings         *storage.Warnings
```

**Shared (single instance, root engine `gid = InstanceID`):** samples store, dictionary store, classifier model state (the in-memory token tables loaded from samples).
**Per-chat (one per `RuntimeChatContext`):** `Detector` struct holding `approvedUsers`, `duplicateDetector`, `reactionDetector`, plus a **pointer/reference** to the shared classifier and to the global samples/dictionary stores.

This split avoids both data leakage (per-chat `approvedUsers`/duplicate state isolated) and staleness (samples updates from any chat propagate to all detectors via the shared classifier). See §SpamFilter / Option A for the constructor pattern.

### TelegramListener

```go
type TelegramListener struct {
    // existing non-chat-specific fields ...
    Groups []ConfiguredChat   // new
    Group  string             // legacy compat; converted to Groups[0] if Groups empty

    contexts    []*RuntimeChatContext
    byPrimary   map[int64]*RuntimeChatContext
    byGID       map[string]*RuntimeChatContext   // for callback routing (stable gid in payload)
    adminChatID int64                            // single shared admin chat
}
```

Removed: hardcoded `chatID`, `linkedChannelID` struct fields.

### SpamFilter and per-chat runtime wiring

`bot.SpamFilter.IsApprovedUser` / `AddApprovedUser` / `RemoveApprovedUser` are thin proxies to `Detector` (`lib/tgspam/detector.go`). Per-chat `SpamFilter` instances alone do **not** isolate approved users — the underlying `Detector` holds:

- `approvedUsers map[string]approved.UserInfo` — keyed solely by `userID`. A second chat calling `Detector.WithUserStorage(ctx.ApprovedUsers)` will replace the map and lose the first chat's loaded entries.
- `duplicateDetector` in `lib/tgspam/duplicate.go` — `cache.Cache[int64, userHistory]` keyed by `userID` (`int64`) only.
- `reactionDetector` in `lib/tgspam/reaction.go` (analogous per-user state).
- `hamHistory` / `spamHistory` queues on the `Detector` struct — message-level state shared globally if the detector is shared.

This **must** be addressed at the `Detector` layer, not at `SpamFilter` wiring. Two acceptable options (implementation plan picks one):

**Option A — per-chat `Detector` instances with shared classifier (preferred for umputun-style minimalism):**

The classifier (in-memory token model loaded from samples) becomes a **shared pointer** held by all per-chat `Detector` instances. The classifier exposes its own internal mutex (already does — `classifier.lock`). All `UpdateSpam` / `UpdateHam` / `ReloadSamples` calls on any chat's `Detector` mutate this single shared state, so all detectors see updates immediately. No fan-out logic needed.

The per-chat `Detector` instance owns its own `approvedUsers`, `duplicateDetector`, `reactionDetector`, `hamHistory`, `spamHistory`, and the per-chat `userStorage`.

```go
// once, globally:
sharedClassifier := tgspam.NewClassifier()       // loaded by ReloadSamples below
sharedSamples    := samplesStore                  // global
sharedDictionary := dictionaryStore               // global

// per chat in startup loop:
det := tgspam.NewDetector(tgspam.Config{
    Classifier: sharedClassifier,                 // shared pointer
    Samples:    sharedSamples,                    // shared
    Dictionary: sharedDictionary,                 // shared
    // per-chat detection thresholds inherit from settings (no per-chat overrides in v1)
})
det.WithUserStorage(ctx.ApprovedUsers)            // per-chat scoped store
ctx.Detector = det
ctx.SpamFilter = bot.NewSpamFilter(bot.SpamConfig{
    Detector: det,
    GroupID:  ctx.GID,
})

// once at end of startup, after all detectors created:
sharedClassifier.LoadSamples(...)                 // single pass, populates the shared model
```

**Cost of N detectors:** small per-chat structs + each chat's `approvedUsers` map + its `duplicateDetector` LRU cache + its `reactionDetector` state + history queues. Approved-users typically <10k per chat. The expensive classifier model loaded from samples is shared, so memory growth is sub-linear in N.

**`Detector` refactor required to enable this:**
- `classifier` field becomes `*classifier` (pointer) instead of value-embedded.
- `NewDetector` accepts a shared classifier in its config; if not provided, allocates its own (preserves current single-chat constructor behavior for tests/CLI tools).
- `ReloadSamples`, `UpdateSpam`, `UpdateHam`, `RemoveSpam`, `RemoveHam` already operate on `d.classifier.*`; with a shared pointer they automatically affect all detectors.

**Option B — `Detector` methods take `gid` parameter, internal state is `map[gid]map[userID]UserInfo`:**

More invasive (signature change across call sites) but keeps a single `Detector` instance. Rejected for v1 because it propagates `gid` through the entire detector API including non-chat-aware paths (CLI tools, tests).

**Decision: Option A.** Spec assumes per-chat `Detector` from this point on.

`makeSpamBot` becomes:

```go
// in startup loop, per ConfiguredChat
ctx.SpamFilter, err = makeSpamBot(ctx, settings, ctx.ScopedEngine, makeDetector(ctx.ScopedEngine, ctx.GID))
```

`makeSpamLogger` similarly per-chat: writes `detected_spam` rows with `ctx.GID` via `ctx.DetectedSpam`. Single global logger receiving an event with the originating `chat_id` is also acceptable provided it routes via `byPrimary[chat_id].DetectedSpam` — implementation plan picks the simpler path.

**In-memory cache audit** (starts in Phase 1 with the Detector refactor; verified complete in Phase 3 alongside the storage migration):

- `lib/tgspam/detector.go` — `approvedUsers`, `hamHistory`, `spamHistory` move to per-chat instance; `classifier` becomes shared pointer
- `lib/tgspam/duplicate.go` — `duplicateDetector` per-chat (its `cache.Cache[int64, userHistory]` is per-chat state)
- `lib/tgspam/reaction.go` — `reactionDetector` per-chat
- `app/bot/spam.go` — methods unchanged (proxy to per-chat Detector via `RuntimeChatContext.SpamFilter`)
- `app/events/listener.go` — `dmUsers` stays global (DMs are bot-token-level, not chat-level)

### Startup flow

1. Load config; if `Telegram.Groups` empty and `Telegram.Group != ""` → `Groups = [{Group: Telegram.Group, GID: InstanceID}]`.
2. Build root engine + global stores (samples, dictionary) with `rootGID = InstanceID`.
3. Build the shared classifier (pointer) — to be filled by `ReloadSamples` once all per-chat detectors are constructed.
4. For each `ConfiguredChat`:
   - Resolve `PrimaryChatID` via `getChatID(Group)`.
   - Resolve `LinkedChannelID` via `GetChat(PrimaryChatID).LinkedChatID`.
   - `ScopedEngine = rootEngine.WithGID(gid)`.
   - Build per-chat `Locator`, `ApprovedUsers`, `DetectedSpam`, `Reports`, `Warnings` from `ScopedEngine`.
   - Build per-chat `Detector` (holds the shared classifier pointer + per-chat approvedUsers/duplicate/reaction state) and per-chat `SpamFilter` wrapping it.
   - Append to `contexts`; populate `byPrimary` and `byGID`.
5. Resolve single `adminChatID` from `AdminGroup` (unchanged).
6. **SuperUsers**: load static list from config/env. If `Admin.SuperUsersCrossChat == true` (new opt-in flag, default `false`), iterate all primary chats and append admins; otherwise skip auto-promotion. Log clearly which mode is active.
7. **Validation:** reject duplicate primary chat IDs; reject duplicate `gid`s; require `gid` matches regex; reject `gid` containing `:`.
8. **StartupMsg:** `len(Groups) == 1` → send to that chat (legacy behavior); `len(Groups) >= 2` → log warning `"startup message ignored in multi-chat mode"`, do not send.

### Privilege model

- Static SuperUsers from config remain global (unchanged for both single and multi-chat modes).
- **Chat-admin auto-promotion**: in single-chat mode, `updateSupers` continues to run for the one chat (current behavior). In multi-chat mode, this is **opt-in** via `Admin.SuperUsersCrossChat`. Default off prevents unintended privilege escalation across groups.
- Flag lives in `AdminSettings` to align with existing `Admin.SuperUsers` / `Admin.AdminGroup` neighbours; CLI/env follow the standard `--admin.superusers-cross-chat` / `ADMIN_SUPERUSERS_CROSS_CHAT` pattern.
- README security section documents the trade-off.

### Message routing

```
chat_id := update.Message.Chat.ID

if ctx, ok := byPrimary[chat_id]; ok {
    process via ctx (uses ctx.SpamFilter, ctx.Locator, ctx.PrimaryChatID for ban target, etc.)
} else if chat_id == adminChatID && SuperUsers.IsSuper(from) {
    handle admin command (target ctx resolved from callback payload — gid embedded)
} else if slices.Contains(TestingIDs, chat_id) {
    legacy testing path
} else {
    skip
}
```

All bot operations (`banUserOrChannel`, `DeleteMessage`, `sendBotResponse` to a chat) take chat ID from `ctx.PrimaryChatID`.

### Storage layer

`engine.SQL` already holds `gid`. Add:

```go
// WithGID returns a shallow copy of *SQL with a different gid value.
// The underlying *sqlx.DB and RWLocker are shared. Caller must not call Close
// on the returned scoped copy — only on the root engine.
func (e *SQL) WithGID(gid string) *SQL {
    cp := *e
    cp.gid = gid
    return &cp
}
```

In-memory cache audit pass: `lib/tgspam/detector.go` (approvedUsers, hamHistory, spamHistory), `lib/tgspam/duplicate.go` (cache by `int64` userID), `lib/tgspam/reaction.go`, `app/bot/spam.go`. Any state keyed solely by `user_id` must move to a per-chat owner (the per-chat `Detector`); findings logged in the implementation plan.

### Schema changes — UNIQUE constraints, not PK rebuild

Two tables currently allow cross-chat overwrites:

- `messages`: PK `hash` only; same-text rows in different chats overwrite each other via `INSERT OR REPLACE` / `ON CONFLICT (hash) DO UPDATE`.
- `spam`: PK `user_id` only; same user across chats overwrites.

**Migration approach** (uniform create-copy-rename for SQLite and Postgres, easier to test than divergent paths):

1. Create `messages_new` and `spam_new` with the new schema. Both use a surrogate `id INTEGER PRIMARY KEY AUTOINCREMENT` (same pattern as `samples`, `approved_users`, `detected_spam`), plus:
   - `messages_new`: `UNIQUE(gid, hash)`. All existing columns preserved.
   - `spam_new`: `UNIQUE(gid, user_id)`. All existing columns preserved.
2. Copy existing rows; for any row with `gid = ''`, set `gid = legacy_gid` (= `Groups[0].GID` for single-chat upgrades, which itself defaults to `InstanceID` for legacy compat). Conflict on `(gid, hash)` / `(gid, user_id)` during copy: keep the most recent row (by `time`).
3. Drop old tables; rename `*_new` → original name. Recreate indexes (`idx_messages_gid_user_id_time` etc.).
4. Update all `INSERT OR REPLACE` / `ON CONFLICT (...)` clauses in `app/storage/locator.go` (`CmdAddLocatorMessage`, `CmdAddLocatorSpam`) to reference the new constraint columns: `ON CONFLICT (gid, hash)` and `ON CONFLICT (gid, user_id)` for Postgres; SQLite uses `INSERT ... ON CONFLICT(gid, hash) DO UPDATE` (replacing the current `INSERT OR REPLACE`).
5. The whole migration runs inside a single transaction with rollback on any error; row counts pre/post compared and logged.

Pre-migration full-DB backup is required (logged at startup, recommended in README; failure to back up is on the operator).

### Backup / restore

- `engine.Backup` today filters by single `gid` — produces a per-instance JSON dump.
- In multi-chat mode the operator typically wants a backup covering **all chats + global samples/dictionary**. Two implementations needed:
  - **SQLite:** new `BackupFull` mode streams the whole `*sqlx.DB` via the SQLite backup API (or file copy with WAL checkpoint); served as a single `.db` file from `/backup/full`.
  - **Postgres:** in-process full dump via `pg_dump` is out of scope (would require shelling out to a binary and authentication). The `/backup/full` endpoint for Postgres returns a JSON dump that iterates over **all configured `gid`s plus the root global stores**, concatenated into one file. README documents that for production Postgres, operators should use external `pg_dump` instead.
- Existing per-`gid` backup endpoint stays available for single-chat installs (legacy path) and for "export this group's data" use cases.

### Forward routing — out of scope for v1

The current admin-side flow where an admin **manually forwards** a spam message into the admin chat for ban relies on hashing the message text and looking it up in the locator. In multi-chat with shared admin:

- Hash collisions across chats are common (spam templates, short messages, copied promos).
- An "any-chat" lookup risks banning in the **wrong** chat.

**v1 decision:** the admin-chat forward path is **disabled when `len(Groups) >= 2`**. Admin sees a one-time reply: `"manual spam forward is not supported in multi-chat mode. Use the inline buttons on the bot's notification, or send /spam in the source chat as a reply to the spammer."`

Single-chat mode keeps current behavior.

Supported moderation paths in multi-chat:
- `/spam` reply inside the source chat (gid known from `update.Message.Chat.ID`).
- Inline callback buttons on the bot's own admin-chat notifications (gid embedded in payload).

If a future PR wants to restore manual forward, it needs a stable origin signal — out of scope here.

### Callback payload format — stable `gid` in payload

Telegram limit: `callback_data` is 1–64 bytes ([API docs](https://core.telegram.org/bots/api)).

Hard cap: `gid` ≤ 24 chars (enforced by the regex in Identifiers section). Worst case: `R+` (2) + `gid` (≤24) + `:` (1) + `userID` (≤19 for int64) + `:` (1) + `msgID` (≤10) = **≤57 bytes**, ≤64 with 7 bytes of safety margin. Typical case is much smaller — `gid` is usually ≤8 chars and msg IDs ≤7 digits → 25–35 bytes.

`gid` length validation lives in config validation (Phase 2) and is the single invariant supporting this bound.

| Today | New |
|---|---|
| `R+userID:msgID` | `R+gid:userID:msgID` |
| `R-userID:msgID` | `R-gid:userID:msgID` |
| `?userID:msgID` | `?gid:userID:msgID` |
| `!userID:msgID` | `!gid:userID:msgID` |
| `userID:msgID` | `gid:userID:msgID` |

`parseCallbackData` accepts both 2-part (legacy) and 3-part (new) formats. Legacy 2-part defaults to `Groups[0]` so old in-flight callbacks from before upgrade still work in single-chat installs and degrade gracefully in multi-chat (admin-visible warning suggested on first such callback).

Unknown `gid` in callback → reject with admin-chat error `"unknown chat in callback"`. No silent fallback.

`R`-prefix routing in `listener.go:208` is unaffected: `gid` matches `^[a-zA-Z0-9_-]{1,24}$`, so `R+R...` is fine — the leading `R+` is the prefix; the `R...` is the gid; only the first `R` is the routing key.

### Single-chat code paths to update

Audit checklist for the implementation plan. Not exhaustive — implementation plan must grep all uses.

| File | Symbol / context | Change |
|---|---|---|
| `listener.go` | `chatID`, `linkedChannelID` struct fields | Remove; replace with `byPrimary` map and per-ctx fields |
| `listener.go` | startup message send | Per the StartupMsg rule |
| `listener.go` | response loop / `sendBotResponse` to bot replies | Use `ctx.PrimaryChatID` from event source |
| `listener.go` | orphan `/report` delete | Use `update.Message.Chat.ID` |
| `listener.go` | reactions handler `r.Chat.ID != l.chatID` | `byPrimary[r.Chat.ID]` |
| `listener.go` | linked-channel check | `byPrimary[msg.SenderChat...].LinkedChannelID` |
| `listener.go` | `banUserOrChannel` calls | `ctx.PrimaryChatID` |
| `listener.go` | admin notification recipient | Unchanged (`adminChatID`) |
| `listener.go` | `updateSupers` | Iterate all `byPrimary` if cross-chat opt-in; otherwise no-op for chat admins |
| `admin.go` | `MsgHandler` (manual forward) | Multi-chat mode → respond with the "not supported" message; single-chat → unchanged |
| `admin.go` | `InlineCallbackHandler` | Resolve ctx via parsed `gid` from callback |
| `admin.go` | `DirectWarnReport` | Use ctx from incoming reply |
| `admin.go` | `msgHandlerFallback` | Use caller ctx |
| `reports.go` | `HandleReportCallback` | Resolve ctx via parsed `gid` |
| `reports.go` | `sendReportNotification` callback markup | Embed `gid` in payload |
| `events.go` | `parseCallbackData` | Accept 2 or 3 parts |
| `bot/spam.go` | `IsApprovedUser` / `AddApprovedUser` / `RemoveApprovedUser` | Per-chat instance (Option A); methods unchanged |
| `lib/tgspam/detector.go` | `approvedUsers` map, `WithUserStorage` | Per-chat Detector instance; one per RuntimeChatContext |
| `lib/tgspam/duplicate.go` | `duplicateDetector` cache by userID | Per-chat instance, owned by the per-chat Detector |
| `lib/tgspam/reaction.go` | `reactionDetector` per-user state | Per-chat instance, owned by the per-chat Detector |
| `main.go` | `makeSpamBot`, `makeSpamLogger`, `makeDetector` | Per-chat construction in startup loop; shared classifier/LLM clients passed in |

### Config & CLI

```yaml
# legacy (works unchanged):
telegram:
  group: "MyGroupName"
admin:
  admin_group: "MyAdminGroup"

# new multi-chat:
telegram:
  groups:
    - group: "MyGroupName"
      gid: "main"
    - group: "-1001234567890"
      gid: "secondary"
admin:
  admin_group: "MyAdminGroup"
  superusers_cross_chat: false   # default; set true to auto-promote chat admins across all groups
```

CLI/env: `TELEGRAM_GROUP`, `ADMIN_GROUP` unchanged. New `TELEGRAM_GROUPS` env (JSON array). Mutually exclusive with `TELEGRAM_GROUP`; validation error if both set. New flag: `--admin.superusers-cross-chat` / `ADMIN_SUPERUSERS_CROSS_CHAT`.

**CONFDB note:** `app/config/settings.go` persists `Telegram.Group` to a DB column (`db:"telegram_group"`). Multi-chat config persistence requires schema extension:

- Phase-1 decision: **multi-chat configurable from YAML/env only**. CONFDB-loaded multi-chat is deferred. Loading an old single-chat CONFDB row works as before; storing back a multi-chat config from CONFDB is rejected with a clear error.
- Phase-2 (deferred): add `telegram_groups TEXT` column carrying JSON, with migration from `telegram_group` on first read.

### Web UI

Phase-1 minimal:

- Header **group selector** dropdown populated from `Telegram.Groups`. Default = first.
- Per-chat endpoints (`/approved`, `/detected_spam`, `/reports`, `/warnings`, `/backup` per-gid) carry `?gid=` (or path `/groups/{gid}/...`). Server validates the `gid` exists in `Telegram.Groups`; reject otherwise.
- Global views (`/samples`, `/dictionary`) ignore the selector.
- Full-DB backup endpoint added: `/backup/full`, available regardless of selector.
- No per-user gid ACL in Phase 7 (deferred to Phase 8+).

### Observability

Every chat-specific log line carries `[gid=X chat_id=Y]` prefix. Format consistent across listener, handlers, storage layer.

## Risks

1. **Schema migration is destructive on failure.** Pre-migration backup required; migration runs in single transaction with rollback; post-migration row-count validation against pre-migration snapshot.
2. **Telegram rate limits — 30 msg/s per token.** With many chats under simultaneous attack, outbound queue saturates and delays cross-chat. Out of scope for v1; documented as known limitation. Future: per-chat queue + global rate limiter.
3. **In-memory state leakage and update staleness.** The `Detector` carries `approvedUsers`, `duplicateDetector`, `reactionDetector`, `hamHistory`, `spamHistory` — all keyed by `userID` alone or globally shared. v2.2 mandates per-chat `Detector` instances **with a shared classifier pointer** (Option A): isolation for per-user/per-chat state, shared classifier so that `UpdateSpam`/`UpdateHam`/`ReloadSamples` from any chat propagate to all detectors automatically (no fan-out logic). Audit covers `lib/tgspam/{detector,duplicate,reaction}.go`, `app/bot/spam.go`.
4. **Privilege model change.** Cross-chat admin auto-promotion is now opt-in. Single-chat installs see no change. Multi-chat installs that want today's "chat admins act as super" behavior must enable `Admin.SuperUsersCrossChat`.
5. **Callback compatibility window.** Pre-upgrade in-flight callbacks use 2-part format. `parseCallbackData` accepts both; legacy 2-part defaults to `Groups[0]` post-upgrade with admin-visible warning on first hit.
6. **Callback `gid` cap of 24 chars** is a hard validation rule to stay within 64-byte payload bound. Documented in config validator error messages.
7. **CONFDB persistence** is deferred for multi-chat. Operators using DB-backed config get a clear error if they try to store a multi-chat config.
8. **Test surface explosion.** Every existing test in `app/events/`, `app/storage/`, `app/webapi/` needs a multi-chat variant. Budget ~40% of total effort.

## Phased delivery

1. **Phase 1 — Detector refactor (precondition for per-chat wiring)**
   - `lib/tgspam/detector.go`: change `classifier` field from value-embedded to `*classifier` pointer. `NewDetector` config accepts a shared classifier; if not provided, allocates its own (preserves single-chat behavior for tests/CLI tools).
   - `duplicateDetector`, `reactionDetector` move to per-Detector instances (already are, but verify ownership semantics).
   - All existing tests pass with one Detector + one classifier (default behavior unchanged).
2. **Phase 2 — Config + types + per-chat wiring**
   - `ConfiguredChat`, `RuntimeChatContext`, `Telegram.Groups` in `app/config/settings.go`.
   - Legacy compat conversion `Group → Groups[0]` with default-gid rule.
   - Validation (no dup chat IDs, gid format `^[a-zA-Z0-9_-]{1,24}$`, no `:`).
   - `engine.WithGID()` shallow copy in `app/storage/engine/engine.go`.
   - Per-chat construction in `app/main.go`: shared classifier first, then per-chat `Detector`, `SpamFilter`, `Locator`, `ApprovedUsers`, `DetectedSpam`, `Reports`, `Warnings`.
   - `Admin.SuperUsersCrossChat` opt-in flag (default off).
3. **Phase 3 — Storage UNIQUE constraints**
   - `messages` `UNIQUE(gid, hash)`, `spam` `UNIQUE(gid, user_id)`.
   - Create-copy-rename migration with backfill coordinated with existing `locator.go:186-194` migration.
   - All `INSERT OR REPLACE` / `ON CONFLICT (...)` updates in `locator.go`.
   - In-memory cache audit complete (verify Phase 1+2 left no `user_id`-keyed shared state).
4. **Phase 4 — Listener routing**
   - `byPrimary`, `byGID` maps in `TelegramListener`.
   - `procEvents`, `procReaction`, `procSuperReply`, `procUserReply` accept ctx.
   - `StartupMsg` per single/multi rule.
   - `linkedChannelID` per ctx.
   - `updateSupers` honours `Admin.SuperUsersCrossChat` flag.
   - All `l.chatID` use-sites migrated (full grep, exhaustive).
5. **Phase 5 — Handlers + callback format**
   - `admin`/`userReports` lose `primChatID` field, take `chatCtx` param.
   - `parseCallbackData` accepts 2 or 3 parts.
   - All inline keyboards in `admin.go`/`reports.go` emit `gid:` prefix.
   - Multi-chat mode disables manual forward in `admin.MsgHandler` with the documented reply.
   - `BackupFull` engine mode (SQLite + Postgres).
6. **Phase 6 — Observability**
   - `[gid=X chat_id=Y]` log prefix in every chat-scoped path.
7. **Phase 7 — Web UI**
   - Header selector + scoped endpoints.
   - Server-side gid validation.
   - `/backup/full` endpoint wired to engine.
8. **Phase 8+ (deferred)** — per-chat overrides, multiple admin chats, cross-chat ban propagation, per-chat super-users, multi-chat CONFDB persistence, restoration of manual forward with reliable origin signal.

## Success criteria

- Existing single-chat installations: **zero config change**, behaviour identical.
- New multi-chat install with 3 groups + 1 admin chat:
  - Spam in group A → ban only in A.
  - Admin notifications labelled with originating chat.
  - Inline callback buttons act on the correct chat after the bot has been restarted (stable `gid`).
  - `/spam` reply inside source chat works.
  - Manual forward into admin chat is rejected with the documented message in multi-chat mode.
  - Web UI lets admin switch between groups; global views unaffected.
  - Full-DB backup downloads cover all chats and global data.
- All existing tests pass; new multi-chat tests cover routing, callback parsing with legacy and new formats, per-chat SpamFilter, schema migration, manual-forward rejection.
