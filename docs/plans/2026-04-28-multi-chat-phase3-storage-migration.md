# Multi-Chat Phase 3 — Storage UNIQUE Constraints Migration Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the cross-chat-overwriting PKs on `messages` and `spam` with composite `UNIQUE(gid, hash)` and `UNIQUE(gid, user_id)` constraints. After this phase, multiple chats can store the same hash / same user_id without collision — the storage layer is fully multi-chat correct. Listener still single-chat (Phase 2 cap not yet lifted; that's Phase 4).

**Architecture:**
- New schema: surrogate auto-increment `id` PK + composite `UNIQUE(gid, hash)` for `messages`, `UNIQUE(gid, user_id)` for `spam`. Matches the existing `samples`/`approved_users`/`detected_spam`/`warnings` pattern.
- Create-copy-rename migration (uniform for SQLite and Postgres). Idempotent: detects old schema by inspecting PK and runs only when needed.
- Backfill: existing `gid = ''` rows already get the instance gid via the existing `migrate()` step (`locator.go:186-194`). Phase 3 migration runs AFTER that backfill in the same `migrate()` transaction, so by the time the new tables are created all rows already have correct gid.
- Dedup during copy: when the old schema's hash collision spans two chats (which the bug allowed), keep the most recent row per `(gid, hash)` by `time`. Same for spam by `(gid, user_id)`.
- Update `CmdAddLocatorMessage` and `CmdAddLocatorSpam` queries to use the new conflict columns: SQLite `ON CONFLICT(gid, hash) DO UPDATE`, Postgres `ON CONFLICT (gid, hash) DO UPDATE`. Same shape for spam with `(gid, user_id)`.
- Whole migration runs in a single transaction with rollback on any error. Pre-migration row count captured and compared post-migration as a sanity check (logged as warning if it dropped due to conflict dedup, NOT fatal).

**Tech Stack:** Go 1.24+, `app/storage/locator.go`, `app/storage/engine`, sqlx, modernc.org/sqlite, Postgres via pgx. Tests use both in-memory SQLite and the existing containerized Postgres harness in `app/storage/`.

**Reference spec:** `docs/plans/2026-04-28-multi-chat-design.md` §Schema changes — UNIQUE constraints, not PK rebuild.

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `app/storage/locator.go` | Modify | New Cmd constants + queries. New migration step. Updated INSERT queries. Updated schema for fresh installs |
| `app/storage/locator_test.go` | Modify | Tests for: migration on legacy schema, idempotency, cross-chat insert, dedup |

---

## Pre-flight

- [ ] **Pre-flight 1: clean tree on phase3 branch**

```bash
cd /home/deploy/tg-spam
git status --short
git checkout -b multichat/phase3-storage-migration multichat/phase2-config-wiring
git rev-parse --abbrev-ref HEAD
```

- [ ] **Pre-flight 2: baseline tests + lint**

```bash
go test -race ./app/storage/... -count=1 2>&1 | tail
docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:latest golangci-lint run 2>&1 | tail
```

Both must be clean.

- [ ] **Pre-flight 3: snapshot of current `migrate()` body for diff reference**

```bash
sed -n '120,210p' app/storage/locator.go
```

Capture the current migration flow — Phase 3 inserts AFTER the gid backfill (lines 186-194) and BEFORE `log.Printf("[DEBUG] locator tables migrated")` at the end.

---

## Task 1: Update fresh-install schema (Cmd constants + queries)

Update `CmdCreateLocatorTables` so new installs land directly on the new schema. Migration of existing installs happens in Task 3.

**Files:**
- Modify: `app/storage/locator.go`

- [ ] **Step 1: Update `CmdCreateLocatorTables`**

In `app/storage/locator.go` around line 32-62, change the schema:

```go
Add(CmdCreateLocatorTables, engine.Query{
    Sqlite: `CREATE TABLE IF NOT EXISTS messages (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        hash TEXT NOT NULL,
        gid TEXT NOT NULL DEFAULT '',
        time TIMESTAMP,
        chat_id INTEGER,
        user_id INTEGER,
        user_name TEXT,
        msg_id INTEGER,
        UNIQUE(gid, hash)
    );
    CREATE TABLE IF NOT EXISTS spam (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        user_id INTEGER NOT NULL,
        gid TEXT NOT NULL DEFAULT '',
        time TIMESTAMP,
        checks TEXT,
        UNIQUE(gid, user_id)
    )`,
    Postgres: `CREATE TABLE IF NOT EXISTS messages (
        id BIGSERIAL PRIMARY KEY,
        hash TEXT NOT NULL,
        gid TEXT NOT NULL DEFAULT '',
        time TIMESTAMP,
        chat_id BIGINT,
        user_id BIGINT,
        user_name TEXT,
        msg_id INTEGER,
        UNIQUE(gid, hash)
    );
    CREATE TABLE IF NOT EXISTS spam (
        id BIGSERIAL PRIMARY KEY,
        user_id BIGINT NOT NULL,
        gid TEXT NOT NULL DEFAULT '',
        time TIMESTAMP,
        checks TEXT,
        UNIQUE(gid, user_id)
    )`,
}).
```

- [ ] **Step 2: Update `CmdAddLocatorMessage`**

Around line 87-99:

```go
Add(CmdAddLocatorMessage, engine.Query{
    Sqlite: `INSERT INTO messages (hash, gid, time, chat_id, user_id, user_name, msg_id) 
        VALUES (:hash, :gid, :time, :chat_id, :user_id, :user_name, :msg_id)
        ON CONFLICT(gid, hash) DO UPDATE SET 
        time = excluded.time, 
        chat_id = excluded.chat_id, 
        user_id = excluded.user_id, 
        user_name = excluded.user_name, 
        msg_id = excluded.msg_id`,
    Postgres: `INSERT INTO messages (hash, gid, time, chat_id, user_id, user_name, msg_id) 
        VALUES (:hash, :gid, :time, :chat_id, :user_id, :user_name, :msg_id)
        ON CONFLICT (gid, hash) DO UPDATE SET 
        time = EXCLUDED.time, 
        chat_id = EXCLUDED.chat_id, 
        user_id = EXCLUDED.user_id, 
        user_name = EXCLUDED.user_name, 
        msg_id = EXCLUDED.msg_id`,
}).
```

Note: SQLite `INSERT ... ON CONFLICT(columns) DO UPDATE` replaces `INSERT OR REPLACE` because the new conflict target is composite.

- [ ] **Step 3: Update `CmdAddLocatorSpam`**

Around line 100-111:

```go
Add(CmdAddLocatorSpam, engine.Query{
    Sqlite: `INSERT INTO spam (user_id, gid, time, checks) 
        VALUES (:user_id, :gid, :time, :checks)
        ON CONFLICT(gid, user_id) DO UPDATE SET 
        time = excluded.time, 
        checks = excluded.checks`,
    Postgres: `INSERT INTO spam (user_id, gid, time, checks) 
        VALUES (:user_id, :gid, :time, :checks)
        ON CONFLICT (gid, user_id) DO UPDATE SET 
        time = EXCLUDED.time, 
        checks = EXCLUDED.checks`,
}).
```

- [ ] **Step 4: Build**

```bash
go build ./...
```

Expected: clean.

- [ ] **Step 5: Run locator tests — many will fail because tests still expect old schema**

```bash
go test -race ./app/storage/ -run TestLocator -v 2>&1 | tail -30
```

Capture failures. Most likely:
- `TestLocator_AddMessage` asserting duplicate-hash behavior — needs update OR may still pass if it only uses one gid
- Migration tests asserting old PK shape

Do NOT fix tests yet — that's Task 4. Task 3 also adds new migration logic that may resolve some.

---

## Task 2: Add migration Cmd constants and SQL

Add commands needed for the migration: detect old PK, create new tables with suffix, copy with dedup, drop old, rename.

**Files:**
- Modify: `app/storage/locator.go`

- [ ] **Step 1: Add new Cmd constants**

Find the const block around line 20-28 and extend:

```go
const (
    CmdCreateLocatorTables engine.DBCmd = iota + 400
    CmdCreateLocatorIndexes
    CmdAddGIDColumnMessages
    CmdAddGIDColumnSpam
    CmdAddLocatorMessage
    CmdAddLocatorSpam
    CmdMessagesNeedsMigration
    CmdMigrateMessagesCreate
    CmdMigrateMessagesCopy
    CmdMigrateMessagesDrop
    CmdMigrateMessagesRename
    CmdSpamNeedsMigration
    CmdMigrateSpamCreate
    CmdMigrateSpamCopy
    CmdMigrateSpamDrop
    CmdMigrateSpamRename
)
```

- [ ] **Step 2: Add detection queries (`*NeedsMigration`)**

In SQLite, query `PRAGMA table_info(messages)` and check whether `hash` is `pk = 1`. In Postgres, query `information_schema.table_constraints` for the primary key.

```go
Add(CmdMessagesNeedsMigration, engine.Query{
    Sqlite: `SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'hash' AND pk = 1`,
    Postgres: `SELECT COUNT(*) FROM information_schema.table_constraints tc
        JOIN information_schema.key_column_usage kcu
          ON tc.constraint_name = kcu.constraint_name
        WHERE tc.table_name = 'messages' AND tc.constraint_type = 'PRIMARY KEY'
          AND kcu.column_name = 'hash'`,
}).
Add(CmdSpamNeedsMigration, engine.Query{
    Sqlite: `SELECT COUNT(*) FROM pragma_table_info('spam') WHERE name = 'user_id' AND pk = 1`,
    Postgres: `SELECT COUNT(*) FROM information_schema.table_constraints tc
        JOIN information_schema.key_column_usage kcu
          ON tc.constraint_name = kcu.constraint_name
        WHERE tc.table_name = 'spam' AND tc.constraint_type = 'PRIMARY KEY'
          AND kcu.column_name = 'user_id'`,
}).
```

Both return `1` when migration is needed (old PK is `hash` / `user_id`), `0` when already migrated.

- [ ] **Step 3: Add create-new-table queries**

```go
Add(CmdMigrateMessagesCreate, engine.Query{
    Sqlite: `CREATE TABLE messages_new (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        hash TEXT NOT NULL,
        gid TEXT NOT NULL DEFAULT '',
        time TIMESTAMP,
        chat_id INTEGER,
        user_id INTEGER,
        user_name TEXT,
        msg_id INTEGER,
        UNIQUE(gid, hash)
    )`,
    Postgres: `CREATE TABLE messages_new (
        id BIGSERIAL PRIMARY KEY,
        hash TEXT NOT NULL,
        gid TEXT NOT NULL DEFAULT '',
        time TIMESTAMP,
        chat_id BIGINT,
        user_id BIGINT,
        user_name TEXT,
        msg_id INTEGER,
        UNIQUE(gid, hash)
    )`,
}).
Add(CmdMigrateSpamCreate, engine.Query{
    Sqlite: `CREATE TABLE spam_new (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        user_id INTEGER NOT NULL,
        gid TEXT NOT NULL DEFAULT '',
        time TIMESTAMP,
        checks TEXT,
        UNIQUE(gid, user_id)
    )`,
    Postgres: `CREATE TABLE spam_new (
        id BIGSERIAL PRIMARY KEY,
        user_id BIGINT NOT NULL,
        gid TEXT NOT NULL DEFAULT '',
        time TIMESTAMP,
        checks TEXT,
        UNIQUE(gid, user_id)
    )`,
}).
```

- [ ] **Step 4: Add copy-with-dedup queries**

Strategy: SELECT from old table, ORDER BY `time DESC` so the most-recent row of each `(gid, key)` wins, INSERT INTO new with `ON CONFLICT DO NOTHING`. Because old PK only enforced uniqueness on `hash` / `user_id` alone, the old table can have at most one row per hash / user_id — so no dedup needed for true cross-chat collisions yet (the bug never let them coexist). But conflicting `gid` values for the same row can occur during in-flight migration if the backfill set `gid` and the row already existed; the `ON CONFLICT DO NOTHING` handles that defensively.

```go
Add(CmdMigrateMessagesCopy, engine.Query{
    Sqlite: `INSERT OR IGNORE INTO messages_new (hash, gid, time, chat_id, user_id, user_name, msg_id)
        SELECT hash, gid, time, chat_id, user_id, user_name, msg_id FROM messages
        ORDER BY time DESC`,
    Postgres: `INSERT INTO messages_new (hash, gid, time, chat_id, user_id, user_name, msg_id)
        SELECT hash, gid, time, chat_id, user_id, user_name, msg_id FROM messages
        ORDER BY time DESC
        ON CONFLICT (gid, hash) DO NOTHING`,
}).
Add(CmdMigrateSpamCopy, engine.Query{
    Sqlite: `INSERT OR IGNORE INTO spam_new (user_id, gid, time, checks)
        SELECT user_id, gid, time, checks FROM spam
        ORDER BY time DESC`,
    Postgres: `INSERT INTO spam_new (user_id, gid, time, checks)
        SELECT user_id, gid, time, checks FROM spam
        ORDER BY time DESC
        ON CONFLICT (gid, user_id) DO NOTHING`,
}).
```

- [ ] **Step 5: Add drop/rename queries**

```go
Add(CmdMigrateMessagesDrop, engine.Query{
    Sqlite:   `DROP TABLE messages`,
    Postgres: `DROP TABLE messages`,
}).
Add(CmdMigrateMessagesRename, engine.Query{
    Sqlite:   `ALTER TABLE messages_new RENAME TO messages`,
    Postgres: `ALTER TABLE messages_new RENAME TO messages`,
}).
Add(CmdMigrateSpamDrop, engine.Query{
    Sqlite:   `DROP TABLE spam`,
    Postgres: `DROP TABLE spam`,
}).
Add(CmdMigrateSpamRename, engine.Query{
    Sqlite:   `ALTER TABLE spam_new RENAME TO spam`,
    Postgres: `ALTER TABLE spam_new RENAME TO spam`,
})
```

- [ ] **Step 6: Build**

```bash
go build ./...
```

Expected: clean.

---

## Task 3: Wire the migration into `Locator.migrate`

Add the migration logic to the existing `migrate` method in `app/storage/locator.go` (currently ends around line 200). The migration runs INSIDE the existing transaction, AFTER the gid-backfill, BEFORE the final `log.Printf`.

**Files:**
- Modify: `app/storage/locator.go`

- [ ] **Step 1: Find the existing migration body**

```bash
grep -n "log.Printf.*locator tables migrated" app/storage/locator.go
```

Note the line. The migration insertion point is immediately before that log.

- [ ] **Step 2: Insert migration steps**

Just before `log.Printf("[DEBUG] locator tables migrated")`, insert:

```go
// migrate messages PK from hash-only to (gid, hash) composite UNIQUE
if err := migrateLocatorTable(ctx, tx, l, "messages",
    CmdMessagesNeedsMigration, CmdMigrateMessagesCreate, CmdMigrateMessagesCopy,
    CmdMigrateMessagesDrop, CmdMigrateMessagesRename); err != nil {
    return fmt.Errorf("failed to migrate messages table: %w", err)
}

// migrate spam PK from user_id-only to (gid, user_id) composite UNIQUE
if err := migrateLocatorTable(ctx, tx, l, "spam",
    CmdSpamNeedsMigration, CmdMigrateSpamCreate, CmdMigrateSpamCopy,
    CmdMigrateSpamDrop, CmdMigrateSpamRename); err != nil {
    return fmt.Errorf("failed to migrate spam table: %w", err)
}
```

- [ ] **Step 3: Add the helper function**

At the bottom of `app/storage/locator.go` (file-private):

```go
// migrateLocatorTable performs a create-copy-drop-rename migration for one locator
// table when its old single-column PK needs to become a composite UNIQUE(gid, key).
// Idempotent: detection query short-circuits when migration was already applied.
// Runs inside the caller's transaction.
func migrateLocatorTable(
    ctx context.Context, tx *sqlx.Tx, l *Locator, tableName string,
    cmdNeeds, cmdCreate, cmdCopy, cmdDrop, cmdRename engine.DBCmd,
) error {
    var needs int
    needsQuery, err := locatorQueries.Pick(l.Type(), cmdNeeds)
    if err != nil {
        return fmt.Errorf("pick %s migration detection query: %w", tableName, err)
    }
    if err := tx.GetContext(ctx, &needs, needsQuery); err != nil {
        return fmt.Errorf("detect %s migration need: %w", tableName, err)
    }
    if needs == 0 {
        return nil // already migrated
    }

    var beforeCount int
    if err := tx.GetContext(ctx, &beforeCount, "SELECT COUNT(*) FROM "+tableName); err != nil {
        return fmt.Errorf("count %s before migration: %w", tableName, err)
    }

    for _, cmd := range []engine.DBCmd{cmdCreate, cmdCopy, cmdDrop, cmdRename} {
        q, err := locatorQueries.Pick(l.Type(), cmd)
        if err != nil {
            return fmt.Errorf("pick %s migration query: %w", tableName, err)
        }
        if _, err := tx.ExecContext(ctx, q); err != nil {
            return fmt.Errorf("exec %s migration step: %w", tableName, err)
        }
    }

    var afterCount int
    if err := tx.GetContext(ctx, &afterCount, "SELECT COUNT(*) FROM "+tableName); err != nil {
        return fmt.Errorf("count %s after migration: %w", tableName, err)
    }
    if afterCount != beforeCount {
        log.Printf("[WARN] %s migration dedup: %d rows before, %d rows after (%d collapsed)",
            tableName, beforeCount, afterCount, beforeCount-afterCount)
    } else {
        log.Printf("[INFO] %s migration: %d rows migrated to composite-key schema", tableName, beforeCount)
    }
    return nil
}
```

Add `"github.com/jmoiron/sqlx"` to imports if not present. The current file uses `l.SQL.GetContext` patterns; adapt if the actual function signatures differ.

- [ ] **Step 4: Build**

```bash
go build ./...
```

- [ ] **Step 5: Try the locator tests — many will pass now because migration runs automatically**

```bash
go test -race ./app/storage/ -run TestLocator -v 2>&1 | tail -40
```

Expected: most tests pass after migration. Tests asserting OLD schema invariants (e.g., "duplicate hash overwrites") may need updates — Task 4 handles those.

---

## Task 4: Update locator tests

The legacy tests assert behavior under the old buggy schema (e.g., "INSERT OR REPLACE on hash overwrites cross-chat"). Phase 3 explicitly changes this behavior. Update tests to:
1. Drop assertions that relied on cross-chat overwrite (those described a bug)
2. Add assertions that the new schema permits same-hash-different-gid coexistence

**Files:**
- Modify: `app/storage/locator_test.go`

- [ ] **Step 1: Find affected tests**

```bash
grep -n "PRIMARY KEY\|TestLocator_\|cross.gid\|same hash" app/storage/locator_test.go | head -20
```

Identify tests that need updates.

- [ ] **Step 2: For each failing test, fix the assertion**

Typical pattern: a test inserted two rows with the same hash, different gid, then asserted only one row exists. Under the new schema, both rows coexist. Update to assert both exist.

Be conservative: only modify what the schema change forces. Do NOT refactor tests for style.

- [ ] **Step 3: Add a new cross-chat coexistence test**

Add to `app/storage/locator_test.go`:

```go
func TestLocator_CrossChat_SameHashCoexists(t *testing.T) {
    ctx := context.Background()
    db, err := engine.New(ctx, ":memory:", "instance-a")
    require.NoError(t, err)
    defer db.Close()

    locA, err := NewLocator(ctx, time.Hour, 0, db)
    require.NoError(t, err)
    require.NoError(t, locA.AddMessage(ctx, "hello", 100, 1, "user1", 10))

    // scope the same DB to a different gid (simulates Phase 4 multi-chat wiring)
    dbB := db.WithGID("instance-b")
    locB, err := NewLocator(ctx, time.Hour, 0, dbB)
    require.NoError(t, err)
    require.NoError(t, locB.AddMessage(ctx, "hello", 200, 2, "user2", 20))

    // both rows must coexist — old schema would have collapsed them via PK(hash)
    metaA, ok := locA.Message(ctx, "hello")
    require.True(t, ok)
    assert.Equal(t, int64(100), metaA.ChatID)

    metaB, ok := locB.Message(ctx, "hello")
    require.True(t, ok)
    assert.Equal(t, int64(200), metaB.ChatID)
}
```

Add analogous `TestLocator_CrossChat_SameUserCoexists` for the spam table:

```go
func TestLocator_CrossChat_SameUserCoexists(t *testing.T) {
    ctx := context.Background()
    db, err := engine.New(ctx, ":memory:", "instance-a")
    require.NoError(t, err)
    defer db.Close()

    locA, err := NewLocator(ctx, time.Hour, 0, db)
    require.NoError(t, err)
    require.NoError(t, locA.AddSpam(ctx, 555, []spamcheck.Response{{Name: "test", Spam: true}}))

    dbB := db.WithGID("instance-b")
    locB, err := NewLocator(ctx, time.Hour, 0, dbB)
    require.NoError(t, err)
    require.NoError(t, locB.AddSpam(ctx, 555, []spamcheck.Response{{Name: "test", Spam: true}}))

    // both rows must coexist for the same user_id across gids
    metaA, ok := locA.Spam(ctx, 555)
    require.True(t, ok)
    _ = metaA
    metaB, ok := locB.Spam(ctx, 555)
    require.True(t, ok)
    _ = metaB
}
```

If `locA.Spam(...)` returns a struct without a chat/gid field that's testable, use `_` and just verify `ok == true`.

- [ ] **Step 4: Run locator tests**

```bash
go test -race ./app/storage/ -run TestLocator -v -count=1 2>&1 | tail -40
```

Expected: all pass.

- [ ] **Step 5: Suite regression**

```bash
go test -race ./app/storage/... -count=1 2>&1 | tail
```

- [ ] **Step 6: Commit Tasks 1-4**

```bash
git add app/storage/locator.go app/storage/locator_test.go
git commit -m "Migrate messages/spam to composite UNIQUE(gid, key)"
```

50 chars, imperative.

---

## Task 5: Migration idempotency test

Test that running `migrate()` twice doesn't break anything.

**Files:**
- Modify: `app/storage/locator_test.go`

- [ ] **Step 1: Write the test**

```go
func TestLocator_Migration_Idempotent(t *testing.T) {
    ctx := context.Background()
    db, err := engine.New(ctx, ":memory:", "test-instance")
    require.NoError(t, err)
    defer db.Close()

    loc, err := NewLocator(ctx, time.Hour, 0, db)
    require.NoError(t, err)
    require.NoError(t, loc.AddMessage(ctx, "x", 1, 1, "u", 1))

    // re-running migrate must be a no-op
    // expose migrate via NewLocator second call OR call a helper if exposed
    loc2, err := NewLocator(ctx, time.Hour, 0, db)
    require.NoError(t, err)
    meta, ok := loc2.Message(ctx, "x")
    require.True(t, ok)
    assert.Equal(t, int64(1), meta.ChatID)
}
```

If `NewLocator` doesn't run migration on every call (because it's wrapped in `sync.Once`), call the internal `migrate` directly or remove the second call — the test then just asserts that the first migrate completed and the table is queryable.

- [ ] **Step 2: Run**

```bash
go test -race ./app/storage/ -run TestLocator_Migration_Idempotent -v
```

- [ ] **Step 3: Commit**

```bash
git add app/storage/locator_test.go
git commit -m "Test locator migration is idempotent"
```

---

## Task 6: Legacy-schema migration test

Simulate an existing install on the old schema — manually create tables with the old PK shape, insert some rows, then run `NewLocator` and verify migration happens correctly.

**Files:**
- Modify: `app/storage/locator_test.go`

- [ ] **Step 1: Write the test**

```go
func TestLocator_Migration_FromLegacySchema(t *testing.T) {
    ctx := context.Background()
    db, err := engine.New(ctx, ":memory:", "test-instance")
    require.NoError(t, err)
    defer db.Close()

    // manually create legacy schema (PRIMARY KEY hash; no UNIQUE(gid,hash))
    _, err = db.ExecContext(ctx, `CREATE TABLE messages (
        hash TEXT PRIMARY KEY,
        gid TEXT NOT NULL DEFAULT '',
        time TIMESTAMP,
        chat_id INTEGER,
        user_id INTEGER,
        user_name TEXT,
        msg_id INTEGER
    )`)
    require.NoError(t, err)
    _, err = db.ExecContext(ctx, `CREATE TABLE spam (
        user_id INTEGER PRIMARY KEY,
        gid TEXT NOT NULL DEFAULT '',
        time TIMESTAMP,
        checks TEXT
    )`)
    require.NoError(t, err)

    // seed legacy data (gid still empty as in old installs)
    _, err = db.ExecContext(ctx,
        `INSERT INTO messages (hash, time, chat_id, user_id, user_name, msg_id) VALUES (?, datetime('now'), 100, 1, 'u', 10)`,
        "abc")
    require.NoError(t, err)
    _, err = db.ExecContext(ctx,
        `INSERT INTO spam (user_id, time, checks) VALUES (?, datetime('now'), 'null')`, 555)
    require.NoError(t, err)

    // NewLocator triggers migrate(): adds gid column (already there) + backfills gid + new schema migration
    loc, err := NewLocator(ctx, time.Hour, 0, db)
    require.NoError(t, err)

    // verify rows survived migration
    meta, ok := loc.Message(ctx, "abc")
    require.True(t, ok, "legacy row must survive migration")
    assert.Equal(t, int64(100), meta.ChatID)
    assert.Equal(t, "test-instance", meta.UserName) // backfill set gid; row visible
}
```

The exact assertions depend on the existing `MsgMeta` struct shape — read it and adapt. The point is to assert the legacy row survives and is queryable under the new schema with backfilled gid.

- [ ] **Step 2: Run**

```bash
go test -race ./app/storage/ -run TestLocator_Migration_FromLegacySchema -v
```

If the test fails because the helper migrateLocatorTable expects a fresh layout, debug — likely a query mismatch on `pragma_table_info` or `INSERT INTO ... SELECT` mismatch.

- [ ] **Step 3: Commit**

```bash
git add app/storage/locator_test.go
git commit -m "Test legacy schema migration to composite UNIQUE"
```

---

## Task 7: Postgres migration test (containerized)

If `app/storage/` already has a Postgres test harness (via `github.com/go-pkgz/testutils`), add the legacy-schema test for Postgres too.

**Files:**
- Modify: `app/storage/locator_test.go`

- [ ] **Step 1: Find the Postgres harness**

```bash
grep -n 'testutils\|postgres\|TestLocator.*[Pp]ostgres' app/storage/*_test.go | head
```

If a `setupPG`/`pgEngine`/`runPostgres` helper exists, use it. If not, skip this task — Postgres validation lands in CI integration.

- [ ] **Step 2: Add the Postgres variant**

Mirror Task 6's `TestLocator_Migration_FromLegacySchema` but use the Postgres engine. SQL adjustments: `BIGSERIAL` and `CURRENT_TIMESTAMP` instead of `datetime('now')`.

- [ ] **Step 3: Run**

```bash
go test -race ./app/storage/ -run TestLocator_Migration_FromLegacySchema_Postgres -v
```

- [ ] **Step 4: Commit**

```bash
git add app/storage/locator_test.go
git commit -m "Test postgres legacy schema migration"
```

---

## Task 8: Final verification

- [ ] **Step 1: Full module test**

```bash
go test -race ./... -count=1
```

- [ ] **Step 2: Lint**

```bash
docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:latest golangci-lint run 2>&1 | tail
```

- [ ] **Step 3: Normalise comments (skip if tool not installed locally; rely on CI)**

```bash
command -v unfuck-ai-comments >/dev/null && unfuck-ai-comments run --fmt --skip=mocks ./app/storage/... || echo "skipped"
```

- [ ] **Step 4: Push branch**

```bash
git push fork multichat/phase3-storage-migration
```

---

## Self-Review Checklist

| Spec requirement | Where it lands |
|---|---|
| `messages` UNIQUE(gid, hash) | Task 1 (fresh) + Task 3 (migration) |
| `spam` UNIQUE(gid, user_id) | Task 1 (fresh) + Task 3 (migration) |
| INSERT ON CONFLICT updated for new keys | Task 1 |
| Create-copy-drop-rename migration | Task 2 + Task 3 |
| Idempotent (detect-and-skip) | Task 3 (cmdNeeds query) + Task 5 |
| Backfill coordinated with existing migrate() | Task 3 (runs after backfill) |
| Cross-chat coexistence test | Task 4 |
| Legacy-schema migration test | Task 6 |
| Postgres parity | Task 7 (best-effort) |

Out of scope:
- Listener routing (Phase 4)
- Per-chat `Locator` instances in `runtimeChatContext` (Phase 4)
- Lifting Phase 2 cap on `Telegram.Groups` (Phase 4)

---

## Done definition

- All Pre-flight + Task checkboxes ticked.
- `go test -race ./... -count=1` clean.
- `golangci-lint run` clean.
- Single-chat installs upgrade automatically via the existing `migrate()` flow; legacy rows preserved.
- Migration is idempotent.
- Branch contains 4-6 commits (Tasks 1-4 batched, plus Tasks 5/6/7 as separate).
