package storage

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

func (s *StorageTestSuite) TestNewLocator() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			const ttl = 10 * time.Minute
			const minSize = 1

			locator, err := NewLocator(ctx, ttl, minSize, db)
			s.Require().NoError(err)
			s.NotNil(locator)
		})
	}
}

func (s *StorageTestSuite) TestLocator_GetUserMessageIDs() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			locator, err := NewLocator(ctx, time.Hour, 1000, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")
			defer db.Exec("DROP TABLE spam")

			userID := int64(100)

			// add multiple messages for the same user
			msgIDs := []int{10, 20, 30, 40, 50}
			for i, msgID := range msgIDs {
				msg := fmt.Sprintf("message%d", i)
				s.Require().NoError(locator.AddMessage(ctx, msg, 123, userID, "user1", msgID))
				time.Sleep(10 * time.Millisecond) // ensure different timestamps
			}

			// add message for different user
			s.Require().NoError(locator.AddMessage(ctx, "other message", 123, 200, "user2", 60))

			// get all message IDs for user
			ids, err := locator.GetUserMessageIDs(ctx, userID, 100)
			s.Require().NoError(err)
			s.Require().Len(ids, 5)

			// should be in reverse chronological order (newest first)
			s.Equal(50, ids[0])
			s.Equal(40, ids[1])
			s.Equal(30, ids[2])
			s.Equal(20, ids[3])
			s.Equal(10, ids[4])

			// test with limit
			ids, err = locator.GetUserMessageIDs(ctx, userID, 3)
			s.Require().NoError(err)
			s.Require().Len(ids, 3)
			s.Equal(50, ids[0])
			s.Equal(40, ids[1])
			s.Equal(30, ids[2])

			// test different user
			ids, err = locator.GetUserMessageIDs(ctx, 200, 100)
			s.Require().NoError(err)
			s.Require().Len(ids, 1)
			s.Equal(60, ids[0])

			// test non-existent user
			ids, err = locator.GetUserMessageIDs(ctx, 999, 100)
			s.Require().NoError(err)
			s.Empty(ids)
		})
	}
}

func (s *StorageTestSuite) TestLocator_CountUserMessages() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			locator, err := NewLocator(ctx, time.Hour, 1000, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")
			defer db.Exec("DROP TABLE spam")

			userID := int64(100)

			// initially zero
			count, err := locator.CountUserMessages(ctx, strconv.FormatInt(userID, 10))
			s.Require().NoError(err)
			s.Equal(0, count)

			// add 3 messages for user 100
			for i, msgID := range []int{10, 20, 30} {
				s.Require().NoError(locator.AddMessage(ctx, fmt.Sprintf("msg-%d", i), 123, userID, "user1", msgID))
			}

			// add a message for a different user
			s.Require().NoError(locator.AddMessage(ctx, "other", 123, 200, "user2", 60))

			count, err = locator.CountUserMessages(ctx, strconv.FormatInt(userID, 10))
			s.Require().NoError(err)
			s.Equal(3, count)

			// verify other user counted independently
			count, err = locator.CountUserMessages(ctx, "200")
			s.Require().NoError(err)
			s.Equal(1, count)

			// non-existent user yields zero
			count, err = locator.CountUserMessages(ctx, "999")
			s.Require().NoError(err)
			s.Equal(0, count)

			// invalid user id string fails
			_, err = locator.CountUserMessages(ctx, "not-a-number")
			s.Require().Error(err)
		})
	}
}

func (s *StorageTestSuite) TestLocator_CountUserMessages_GIDIsolation() {
	db1, err := engine.NewSqlite(":memory:", "gr1")
	s.Require().NoError(err)
	defer db1.Close()

	db2, err := engine.NewSqlite(":memory:", "gr2")
	s.Require().NoError(err)
	defer db2.Close()

	ctx := context.Background()

	locator1, err := NewLocator(ctx, time.Hour, 5, db1)
	s.Require().NoError(err)
	locator2, err := NewLocator(ctx, time.Hour, 5, db2)
	s.Require().NoError(err)

	// add messages for the same user id under different gids
	s.Require().NoError(locator1.AddMessage(ctx, "m1", 1, 42, "u", 1))
	s.Require().NoError(locator1.AddMessage(ctx, "m2", 1, 42, "u", 2))
	s.Require().NoError(locator2.AddMessage(ctx, "m3", 2, 42, "u", 3))

	count1, err := locator1.CountUserMessages(ctx, "42")
	s.Require().NoError(err)
	s.Equal(2, count1, "locator1 should count only its own gid messages")

	count2, err := locator2.CountUserMessages(ctx, "42")
	s.Require().NoError(err)
	s.Equal(1, count2, "locator2 should count only its own gid messages")
}

func (s *StorageTestSuite) TestLocator_UserMessageIDs() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			locator, err := NewLocator(ctx, time.Hour, 1000, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")
			defer db.Exec("DROP TABLE spam")

			userID := int64(100)
			msgIDs := []int{10, 20, 30, 40, 50}
			for i, msgID := range msgIDs {
				s.Require().NoError(locator.AddMessage(ctx, fmt.Sprintf("m-%d", i), 123, userID, "user1", msgID))
				time.Sleep(10 * time.Millisecond) // ensure ordering by time
			}

			// fetch all, newest first
			ids, err := locator.UserMessageIDs(ctx, "100", 100)
			s.Require().NoError(err)
			s.Require().Len(ids, 5)
			s.Equal([]int{50, 40, 30, 20, 10}, ids)

			// limit honored
			ids, err = locator.UserMessageIDs(ctx, "100", 3)
			s.Require().NoError(err)
			s.Equal([]int{50, 40, 30}, ids)

			// non-existent user yields empty
			ids, err = locator.UserMessageIDs(ctx, "999", 10)
			s.Require().NoError(err)
			s.Empty(ids)

			// invalid user id string fails
			_, err = locator.UserMessageIDs(ctx, "abc", 10)
			s.Require().Error(err)
		})
	}
}

func (s *StorageTestSuite) TestLocator_AddAndRetrieveMessage() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			locator, err := NewLocator(ctx, time.Hour, 1000, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")
			defer db.Exec("DROP TABLE spam")

			msg := "test message"
			chatID := int64(123)
			userID := int64(456)
			userName := "user1"
			msgID := 789

			s.Require().NoError(locator.AddMessage(ctx, msg, chatID, userID, userName, msgID))

			retrievedMsg, found := locator.Message(ctx, msg)
			s.Require().True(found)
			s.Equal(MsgMeta{Time: retrievedMsg.Time, ChatID: chatID, UserID: userID, UserName: userName, MsgID: msgID}, retrievedMsg)

			res := locator.UserNameByID(ctx, userID)
			s.Equal(userName, res)

			res = locator.UserNameByID(ctx, 123456)
			s.Empty(res)

			id := locator.UserIDByName(ctx, userName)
			s.Equal(userID, id)

			id = locator.UserIDByName(ctx, "user2")
			s.Equal(int64(0), id)
		})
	}
}

func (s *StorageTestSuite) TestLocator_AddAndRetrieveManyMessage() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			locator, err := NewLocator(ctx, time.Hour, 1000, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")
			defer db.Exec("DROP TABLE spam")

			// add 100 messages for 10 users
			for i := range 100 {
				userID := int64(i%10 + 1)
				s.Require().NoError(locator.AddMessage(ctx, fmt.Sprintf("test message %d", i), 1234, userID, "name"+strconv.Itoa(int(userID)), i))
			}

			for i := range 100 {
				retrievedMsg, found := locator.Message(ctx, fmt.Sprintf("test message %d", i))
				s.Require().True(found)
				s.Equal(MsgMeta{Time: retrievedMsg.Time, ChatID: int64(1234), UserID: int64(i%10 + 1), UserName: "name" + strconv.Itoa(i%10+1), MsgID: i}, retrievedMsg)
			}
		})
	}
}

func (s *StorageTestSuite) TestLocator_AddAndRetrieveSpam() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			locator, err := NewLocator(ctx, time.Hour, 1000, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")
			defer db.Exec("DROP TABLE spam")

			userID := int64(456)
			checks := []spamcheck.Response{{Name: "test", Spam: true, Details: "test spam"}}

			s.Require().NoError(locator.AddSpam(ctx, userID, checks))

			retrievedSpam, found := locator.Spam(ctx, userID)
			s.Require().True(found)
			s.Equal(checks, retrievedSpam.Checks)
		})
	}
}

func (s *StorageTestSuite) TestLocator_CleanupLogic() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			ttl := 10 * time.Minute
			locator, err := NewLocator(ctx, ttl, 1, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")
			defer db.Exec("DROP TABLE spam")

			oldTime := time.Now().Add(-2 * ttl) // ensure this is older than the ttl

			// add two of each to both groups, we have minSize = 1, so we should have 2 to allow cleanup
			q := `INSERT INTO messages (hash, gid, time, chat_id, user_id, user_name, msg_id) VALUES (?, ?, ?, ?, ?, ?, ?)`
			_, err = locator.Exec(db.Adopt(q), "old_hash1", locator.GID(), oldTime, int64(111), int64(222), "old_user", 333)
			s.Require().NoError(err)
			_, err = locator.Exec(db.Adopt(q), "old_hash2", locator.GID(), oldTime, int64(111), int64(222), "old_user", 333)
			s.Require().NoError(err)

			_, err = locator.Exec(db.Adopt(`INSERT INTO spam (user_id, gid, time, checks) VALUES (?, ?, ?, ?)`),
				int64(222), locator.GID(), oldTime, `[{"Name":"old_test","Spam":true,"Details":"old spam"}]`)
			s.Require().NoError(err)
			_, err = locator.Exec(db.Adopt(`INSERT INTO spam (user_id, gid, time, checks) VALUES (?, ?, ?, ?)`),
				int64(223), locator.GID(), oldTime, `[{"Name":"old_test","Spam":true,"Details":"old spam"}]`)
			s.Require().NoError(err)

			s.Require().NoError(locator.cleanupMessages(ctx))
			s.Require().NoError(locator.cleanupSpam())

			var msgCountAfter, spamCountAfter int
			err = locator.Get(&msgCountAfter, db.Adopt(`SELECT COUNT(*) FROM messages WHERE gid = ?`), locator.GID())
			s.Require().NoError(err)
			err = locator.Get(&spamCountAfter, db.Adopt(`SELECT COUNT(*) FROM spam WHERE gid = ?`), locator.GID())
			s.Require().NoError(err)

			s.Equal(0, msgCountAfter, "messages should be cleaned up")
			s.Equal(0, spamCountAfter, "spam should be cleaned up")
		})
	}
}

func (s *StorageTestSuite) TestLocator_RetrieveNonExistentMessage() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			locator, err := NewLocator(ctx, time.Hour, 1000, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")
			defer db.Exec("DROP TABLE spam")

			msg := "non_existent_message"
			_, found := locator.Message(ctx, msg)
			s.False(found, "expected to not find a non-existent message")

			_, found = locator.Spam(ctx, 1234)
			s.False(found, "expected to not find a non-existent spam")
		})
	}
}

func (s *StorageTestSuite) TestLocator_SpamUnmarshalFailure() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			locator, err := NewLocator(ctx, time.Hour, 1000, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")
			defer db.Exec("DROP TABLE spam")

			// insert invalid JSON data directly into the database
			userID := int64(456)
			invalidJSON := "invalid json"
			_, err = locator.Exec(db.Adopt(`INSERT INTO spam (user_id, gid, time, checks) VALUES (?, ?, ?, ?)`),
				userID, locator.GID(), time.Now(), invalidJSON)
			s.Require().NoError(err)

			// attempt to retrieve the spam data, which should fail during unmarshalling
			_, found := locator.Spam(ctx, userID)
			s.False(found, "expected to not find valid data due to unmarshalling failure")
		})
	}
}

func (s *StorageTestSuite) TestLocator_HashCollisions() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			_, err := NewLocator(ctx, time.Hour, 1000, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")

			// force hash collision by manually inserting with same hash
			hash := "collision_hash"
			msg1 := MsgMeta{
				ChatID:   100,
				UserID:   1,
				UserName: "user1",
				MsgID:    1,
				Time:     time.Now(),
			}
			msg2 := MsgMeta{
				ChatID:   200,
				UserID:   2,
				UserName: "user2",
				MsgID:    2,
				Time:     time.Now(),
			}

			// insert first message
			q := db.Adopt(`INSERT INTO messages (hash, gid, time, chat_id, user_id, user_name, msg_id) VALUES (?, ?, ?, ?, ?, ?, ?)`)
			_, err = db.Exec(q, hash, db.GID(), msg1.Time, msg1.ChatID, msg1.UserID, msg1.UserName, msg1.MsgID)
			s.Require().NoError(err)

			// try to insert second message with same hash
			_, err = db.Exec(q, hash, db.GID(), msg2.Time, msg2.ChatID, msg2.UserID, msg2.UserName, msg2.MsgID)
			// should fail due to hash being primary key
			s.Require().Error(err)

			// verify only first message exists
			var count int
			err = db.Get(&count, db.Adopt("SELECT COUNT(*) FROM messages WHERE hash = ? AND gid = ?"), hash, db.GID())
			s.Require().NoError(err)
			s.Equal(1, count)
		})
	}
}

func (s *StorageTestSuite) TestLocator_Migration() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB

		s.Run(fmt.Sprintf("with %s", db.Type()), func() {
			if db.Type() == engine.Postgres {
				s.T().Skip("skipping postgres for now")
			}
			db.Exec("DROP TABLE IF EXISTS messages")
			db.Exec("DROP TABLE IF EXISTS spam")

			s.Run("migrate from old schema", func() {
				defer db.Exec("DROP TABLE messages")
				defer db.Exec("DROP TABLE spam")

				// setup old schema without gid
				_, err := db.Exec(`
					CREATE TABLE messages (
						hash TEXT PRIMARY KEY,
						time TIMESTAMP,
						chat_id INTEGER,
						user_id INTEGER,
						user_name TEXT,
						msg_id INTEGER
					);
					CREATE TABLE spam (
						user_id INTEGER PRIMARY KEY,
						time TIMESTAMP,
						checks TEXT
					)
				`)
				s.Require().NoError(err)

				// insert test data in old format
				_, err = db.Exec(`INSERT INTO messages (hash, time, chat_id, user_id, user_name, msg_id)
            		VALUES ('hash1', ?, 1, 100, 'user1', 1)`, time.Now())
				s.Require().NoError(err)

				checksJSON := `[{"Name":"test","Spam":true,"Details":"test"}]`
				_, err = db.Exec(`INSERT INTO spam (user_id, time, checks)
            		VALUES (100, ?, ?)`, time.Now(), checksJSON)
				s.Require().NoError(err)

				// run migration through NewLocator
				_, err = NewLocator(context.Background(), 10*time.Minute, 1, db)
				s.Require().NoError(err)

				// verify structure
				var msgCols, spamCols []struct {
					CID       int     `db:"cid"`
					Name      string  `db:"name"`
					Type      string  `db:"type"`
					NotNull   bool    `db:"notnull"`
					DfltValue *string `db:"dflt_value"`
					PK        bool    `db:"pk"`
				}
				err = db.Select(&msgCols, "PRAGMA table_info(messages)")
				s.Require().NoError(err)
				err = db.Select(&spamCols, "PRAGMA table_info(spam)")
				s.Require().NoError(err)

				msgColMap := make(map[string]string)
				for _, col := range msgCols {
					msgColMap[col.Name] = col.Type
				}
				s.Equal("TEXT", msgColMap["gid"])

				spamColMap := make(map[string]string)
				for _, col := range spamCols {
					spamColMap[col.Name] = col.Type
				}
				s.Equal("TEXT", spamColMap["gid"])

				// verify data migrated correctly
				var msgGID, spamGID string
				err = db.Get(&msgGID, "SELECT gid FROM messages WHERE hash = 'hash1'")
				s.Require().NoError(err)
				s.Equal("gr1", msgGID)

				err = db.Get(&spamGID, "SELECT gid FROM spam WHERE user_id = 100")
				s.Require().NoError(err)
				s.Equal("gr1", spamGID)
			})

			s.Run("migration idempotency", func() {
				_, err := NewLocator(ctx, time.Hour, 1000, db)
				s.Require().NoError(err)
				defer db.Exec("DROP TABLE messages")
				defer db.Exec("DROP TABLE spam")

				// create schema with new tables including gid
				createSchema, err := locatorQueries.Pick(db.Type(), CmdCreateLocatorTables)
				s.Require().NoError(err)
				_, err = db.Exec(createSchema)
				s.Require().NoError(err)

				// create first locator which should trigger migration
				_, err = NewLocator(context.Background(), 10*time.Minute, 1, db)
				s.Require().NoError(err)

				// create second locator which should trigger migration again
				_, err = NewLocator(context.Background(), 10*time.Minute, 1, db)
				s.Require().NoError(err)

				// verify structure remained the same
				var msgCols []struct {
					CID       int     `db:"cid"`
					Name      string  `db:"name"`
					Type      string  `db:"type"`
					NotNull   bool    `db:"notnull"`
					DfltValue *string `db:"dflt_value"`
					PK        bool    `db:"pk"`
				}
				err = db.Select(&msgCols, "PRAGMA table_info(messages)")
				s.Require().NoError(err)

				gidColCount := 0
				for _, col := range msgCols {
					if col.Name == "gid" {
						gidColCount++
					}
				}
				s.Equal(1, gidColCount, "should have exactly one gid column")
			})
		})
	}

}

func (s *StorageTestSuite) TestLocator_GIDIsolation() {
	db1, err := engine.NewSqlite(":memory:", "gr1")
	s.Require().NoError(err)
	defer db1.Close()

	db2, err := engine.NewSqlite(":memory:", "gr2")
	s.Require().NoError(err)
	defer db2.Close()

	ctx := context.Background()

	locator1, err := NewLocator(ctx, time.Hour, 5, db1)
	s.Require().NoError(err)

	locator2, err := NewLocator(ctx, time.Hour, 5, db2)
	s.Require().NoError(err)

	// add same message to both locators
	msg := "test message"
	err = locator1.AddMessage(ctx, msg, 100, 1, "user1", 1)
	s.Require().NoError(err)
	err = locator2.AddMessage(ctx, msg, 200, 2, "user2", 2)
	s.Require().NoError(err)

	// verify messages are isolated
	meta1, found := locator1.Message(ctx, msg)
	s.Require().True(found)
	s.Equal(int64(100), meta1.ChatID)
	s.Equal("user1", meta1.UserName)

	meta2, found := locator2.Message(ctx, msg)
	s.Require().True(found)
	s.Equal(int64(200), meta2.ChatID)
	s.Equal("user2", meta2.UserName)

	// verify spam data isolation
	checks1 := []spamcheck.Response{{Name: "test1", Spam: true}}
	checks2 := []spamcheck.Response{{Name: "test2", Spam: false}}

	err = locator1.AddSpam(ctx, 1, checks1)
	s.Require().NoError(err)
	err = locator2.AddSpam(ctx, 1, checks2)
	s.Require().NoError(err)

	// verify spam data is isolated
	spam1, found := locator1.Spam(ctx, 1)
	s.Require().True(found)
	s.True(spam1.Checks[0].Spam)

	spam2, found := locator2.Spam(ctx, 1)
	s.Require().True(found)
	s.False(spam2.Checks[0].Spam)
}

func TestLocator_CrossChat_SameHashCoexists(t *testing.T) {
	ctx := context.Background()
	rootDB, err := engine.New(ctx, ":memory:", "instance-a")
	require.NoError(t, err)
	defer rootDB.Close()

	locA, err := NewLocator(ctx, time.Hour, 0, rootDB)
	require.NoError(t, err)
	require.NoError(t, locA.AddMessage(ctx, "hello", 100, 1, "user1", 10))

	// scope the same root DB to a different gid; same underlying table
	dbB := rootDB.WithGID("instance-b")
	locB, err := NewLocator(ctx, time.Hour, 0, dbB)
	require.NoError(t, err)
	require.NoError(t, locB.AddMessage(ctx, "hello", 200, 2, "user2", 20))

	metaA, ok := locA.Message(ctx, "hello")
	require.True(t, ok, "instance-a row must remain visible")
	assert.Equal(t, int64(100), metaA.ChatID)
	assert.Equal(t, int64(1), metaA.UserID)

	metaB, ok := locB.Message(ctx, "hello")
	require.True(t, ok, "instance-b row must remain visible (new schema allows coexistence)")
	assert.Equal(t, int64(200), metaB.ChatID)
	assert.Equal(t, int64(2), metaB.UserID)
}

func TestLocator_CrossChat_SameUserCoexists(t *testing.T) {
	ctx := context.Background()
	rootDB, err := engine.New(ctx, ":memory:", "instance-a")
	require.NoError(t, err)
	defer rootDB.Close()

	locA, err := NewLocator(ctx, time.Hour, 0, rootDB)
	require.NoError(t, err)
	require.NoError(t, locA.AddSpam(ctx, 555, []spamcheck.Response{{Name: "test-a", Spam: true}}))

	dbB := rootDB.WithGID("instance-b")
	locB, err := NewLocator(ctx, time.Hour, 0, dbB)
	require.NoError(t, err)
	require.NoError(t, locB.AddSpam(ctx, 555, []spamcheck.Response{{Name: "test-b", Spam: true}}))

	// both rows must coexist for the same user_id across gids
	var aCount, bCount int
	require.NoError(t, rootDB.GetContext(ctx, &aCount,
		"SELECT COUNT(*) FROM spam WHERE gid = ? AND user_id = ?", "instance-a", 555))
	assert.Equal(t, 1, aCount)
	require.NoError(t, rootDB.GetContext(ctx, &bCount,
		"SELECT COUNT(*) FROM spam WHERE gid = ? AND user_id = ?", "instance-b", 555))
	assert.Equal(t, 1, bCount)

	// also verify checks differ via public Spam() — proves gid-scoped lookup
	spamA, ok := locA.Spam(ctx, 555)
	require.True(t, ok)
	assert.Equal(t, "test-a", spamA.Checks[0].Name)
	spamB, ok := locB.Spam(ctx, 555)
	require.True(t, ok)
	assert.Equal(t, "test-b", spamB.Checks[0].Name)
}

func TestLocator_Migration_Idempotent(t *testing.T) {
	ctx := context.Background()
	db, err := engine.New(ctx, ":memory:", "test-instance")
	require.NoError(t, err)
	defer db.Close()

	loc, err := NewLocator(ctx, time.Hour, 0, db)
	require.NoError(t, err)
	require.NoError(t, loc.AddMessage(ctx, "x", 1, 1, "u", 1))

	// re-running NewLocator must be a no-op (migration short-circuits when already applied)
	loc2, err := NewLocator(ctx, time.Hour, 0, db)
	require.NoError(t, err)

	// data from the first locator must still be queryable through the second
	meta, ok := loc2.Message(ctx, "x")
	require.True(t, ok, "data must survive idempotent re-migration")
	assert.Equal(t, int64(1), meta.ChatID)
}

func TestLocator_Migration_FromLegacySchema(t *testing.T) {
	ctx := context.Background()
	db, err := engine.New(ctx, ":memory:", "test-instance")
	require.NoError(t, err)
	defer db.Close()

	// BEFORE NewLocator: manually create legacy schema (PRIMARY KEY hash; no gid column)
	// This simulates a pre-Phase-2 install. The migrate function will:
	//   1. ALTER TABLE ADD COLUMN gid (skipped if already there)
	//   2. UPDATE rows SET gid = ? WHERE gid = ''
	//   3. NEW: detect old PK and run create-copy-drop-rename to new schema
	_, err = db.ExecContext(ctx, `CREATE TABLE messages (
        hash TEXT PRIMARY KEY,
        time TIMESTAMP,
        chat_id INTEGER,
        user_id INTEGER,
        user_name TEXT,
        msg_id INTEGER
    )`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE spam (
        user_id INTEGER PRIMARY KEY,
        time TIMESTAMP,
        checks TEXT
    )`)
	require.NoError(t, err)

	// seed legacy data (no gid column yet); use a real sha256 hash so loc.Message lookup works
	legacyMsg := "legacy message"
	legacyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(legacyMsg)))
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (hash, time, chat_id, user_id, user_name, msg_id) VALUES (?, datetime('now'), 100, 1, 'u', 10)`,
		legacyHash)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO spam (user_id, time, checks) VALUES (?, datetime('now'), '[]')`, 555)
	require.NoError(t, err)

	// NewLocator triggers full migrate(): ALTER ADD COLUMN gid + backfill + PK migration
	loc, err := NewLocator(ctx, time.Hour, 0, db)
	require.NoError(t, err)

	// legacy row must survive the migration
	meta, ok := loc.Message(ctx, legacyMsg)
	require.True(t, ok, "legacy row must survive migration")
	assert.Equal(t, int64(100), meta.ChatID)
	assert.Equal(t, int64(1), meta.UserID)
	assert.Equal(t, "u", meta.UserName)
	assert.Equal(t, 10, meta.MsgID)

	// verify gid was backfilled via direct SQL (MsgMeta doesn't expose gid)
	var gid string
	require.NoError(t, db.GetContext(ctx, &gid, "SELECT gid FROM messages WHERE hash = ?", legacyHash))
	assert.Equal(t, "test-instance", gid)

	// verify new schema is in place: messages.id column should exist now
	var hasIDColumn int
	require.NoError(t, db.GetContext(ctx, &hasIDColumn,
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'id'`))
	assert.Equal(t, 1, hasIDColumn, "messages must have surrogate id column after migration")

	// verify the spam row survived too
	var spamGID string
	require.NoError(t, db.GetContext(ctx, &spamGID, "SELECT gid FROM spam WHERE user_id = ?", 555))
	assert.Equal(t, "test-instance", spamGID)
}

// TestLocator_Migration_FromLegacySchema_Postgres mirrors TestLocator_Migration_FromLegacySchema
// against Postgres, exercising the pre-Phase-2 → Phase-3 migration path on the real engine.
// Runs through the suite so the existing pgContainer is reused (skipped in -short mode).
func (s *StorageTestSuite) TestLocator_Migration_FromLegacySchema_Postgres() {
	db, ok := s.dbs["postgres"]
	if !ok {
		s.T().Skip("postgres container unavailable (short mode)")
	}

	ctx := context.Background()

	// drop any previous state, then set up a legacy Postgres schema (hash PK; no gid column)
	_, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS messages`)
	s.Require().NoError(err)
	_, err = db.ExecContext(ctx, `DROP TABLE IF EXISTS spam`)
	s.Require().NoError(err)
	defer db.ExecContext(ctx, `DROP TABLE IF EXISTS messages`)
	defer db.ExecContext(ctx, `DROP TABLE IF EXISTS spam`)

	_, err = db.ExecContext(ctx, `CREATE TABLE messages (
        hash TEXT PRIMARY KEY,
        "time" TIMESTAMP,
        chat_id BIGINT,
        user_id BIGINT,
        user_name TEXT,
        msg_id INTEGER
    )`)
	s.Require().NoError(err)
	_, err = db.ExecContext(ctx, `CREATE TABLE spam (
        user_id BIGINT PRIMARY KEY,
        "time" TIMESTAMP,
        checks TEXT
    )`)
	s.Require().NoError(err)

	// seed legacy data; use a real sha256 hash so loc.Message lookup works post-migration
	legacyMsg := "legacy message"
	legacyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(legacyMsg)))
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (hash, "time", chat_id, user_id, user_name, msg_id) VALUES ($1, NOW(), 100, 1, 'u', 10)`,
		legacyHash)
	s.Require().NoError(err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO spam (user_id, "time", checks) VALUES ($1, NOW(), '[]')`, 555)
	s.Require().NoError(err)

	// NewLocator triggers full migrate(): ALTER ADD COLUMN gid + backfill + PK migration
	loc, err := NewLocator(ctx, time.Hour, 0, db)
	s.Require().NoError(err)

	// legacy row must survive the migration
	meta, found := loc.Message(ctx, legacyMsg)
	s.Require().True(found, "legacy row must survive migration")
	s.Equal(int64(100), meta.ChatID)
	s.Equal(int64(1), meta.UserID)
	s.Equal("u", meta.UserName)
	s.Equal(10, meta.MsgID)

	// verify gid was backfilled to the engine's gid
	var gid string
	s.Require().NoError(db.GetContext(ctx, &gid, db.Adopt("SELECT gid FROM messages WHERE hash = ?"), legacyHash))
	s.Equal(db.GID(), gid)

	// verify new schema is in place: messages.id column should exist now
	var hasIDColumn int
	s.Require().NoError(db.GetContext(ctx, &hasIDColumn,
		`SELECT COUNT(*) FROM information_schema.columns
         WHERE table_schema = current_schema() AND table_name = 'messages' AND column_name = 'id'`))
	s.Equal(1, hasIDColumn, "messages must have surrogate id column after migration")

	// verify the spam row survived too
	var spamGID string
	s.Require().NoError(db.GetContext(ctx, &spamGID, db.Adopt("SELECT gid FROM spam WHERE user_id = ?"), 555))
	s.Equal(db.GID(), spamGID)
}

func TestLocator_Migration_IndexesRecreated(t *testing.T) {
	ctx := context.Background()
	db, err := engine.New(ctx, ":memory:", "test-instance")
	require.NoError(t, err)
	defer db.Close()

	// seed legacy schema (no indexes — they'll be created via NewLocator → InitTable → CreateIndexes)
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

	// trigger NewLocator → InitTable → migrate (drop+rename) → CreateIndexes
	_, err = NewLocator(ctx, time.Hour, 0, db)
	require.NoError(t, err)

	// each name below MUST match exactly what CmdCreateLocatorIndexes for SQLite emits in locator.go
	expectedIndexes := []string{
		"idx_messages_user_id",
		"idx_messages_user_name",
		"idx_spam_time",
		"idx_messages_gid",
		"idx_messages_gid_user_id_time",
		"idx_spam_gid",
	}
	for _, idx := range expectedIndexes {
		var name string
		err := db.GetContext(ctx, &name,
			"SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?", idx)
		require.NoError(t, err, "index %s missing after migration", idx)
		assert.Equal(t, idx, name)
	}
}
