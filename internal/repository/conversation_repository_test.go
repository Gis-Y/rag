package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"pai-smart-go/internal/model"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A small database/sql driver checks our transaction order and emitted SQL without
// external services. It does not pretend to verify MySQL locking or SQL execution.
type conversationSQLStep struct {
	contains []string
	args     map[int]interface{}
	columns  []string
	rows     [][]driver.Value
	affected int64
	err      error
}

type conversationSQLScript struct {
	t     *testing.T
	mu    sync.Mutex
	steps []conversationSQLStep
}

func (s *conversationSQLScript) next(query string, args []driver.NamedValue) (conversationSQLStep, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.steps) == 0 {
		s.t.Errorf("unexpected SQL: %s", query)
		return conversationSQLStep{}, errors.New("unexpected SQL")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	for _, fragment := range step.contains {
		if !strings.Contains(query, fragment) {
			s.t.Errorf("SQL %q must contain %q", query, fragment)
			return step, errors.New("SQL mismatch")
		}
	}
	for i, expected := range step.args {
		if i >= len(args) || !reflect.DeepEqual(args[i].Value, expected) {
			s.t.Errorf("SQL %q argument %d: got %v, want %v", query, i, args, expected)
			return step, errors.New("SQL argument mismatch")
		}
	}
	return step, step.err
}

type conversationConnector struct{ script *conversationSQLScript }
type conversationDriver struct{}
type conversationConn struct{ script *conversationSQLScript }
type conversationTx struct{ script *conversationSQLScript }
type conversationRows struct {
	columns []string
	rows    [][]driver.Value
}
type conversationResult int64

func (c conversationConnector) Connect(context.Context) (driver.Conn, error) {
	return conversationConn{c.script}, nil
}
func (conversationConnector) Driver() driver.Driver         { return conversationDriver{} }
func (conversationDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }
func (conversationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (conversationConn) Close() error { return nil }
func (c conversationConn) Begin() (driver.Tx, error) {
	_, err := c.script.next("BEGIN", nil)
	return conversationTx{c.script}, err
}
func (c conversationConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	step, err := c.script.next(query, args)
	return &conversationRows{step.columns, step.rows}, err
}
func (c conversationConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	step, err := c.script.next(query, args)
	return conversationResult(step.affected), err
}
func (t conversationTx) Commit() error        { _, err := t.script.next("COMMIT", nil); return err }
func (t conversationTx) Rollback() error      { _, err := t.script.next("ROLLBACK", nil); return err }
func (r *conversationRows) Columns() []string { return r.columns }
func (r *conversationRows) Close() error      { return nil }
func (r *conversationRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}
func (conversationResult) LastInsertId() (int64, error)   { return 1, nil }
func (r conversationResult) RowsAffected() (int64, error) { return int64(r), nil }

func newConversationSQLTest(t *testing.T, steps ...conversationSQLStep) *conversationRepository {
	t.Helper()
	script := &conversationSQLScript{t: t, steps: steps}
	sqlDB := sql.OpenDB(conversationConnector{script})
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
		DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
		if len(script.steps) != 0 {
			t.Errorf("%d SQL expectations were not reached: %+v", len(script.steps), script.steps)
		}
	})
	return &conversationRepository{db: db}
}

func stateSQLStep(version, last, until int64, locked bool) conversationSQLStep {
	step := conversationSQLStep{contains: []string{"SELECT * FROM `conversation_states`", "id = ? AND user_id = ?"},
		args:    map[int]interface{}{0: "conv", 1: int64(7)},
		columns: []string{"id", "user_id", "version", "last_turn", "summary", "summary_until", "context_after"},
		rows:    [][]driver.Value{{"conv", int64(7), version, last, "{}", until, int64(0)}}}
	if locked {
		step.contains = append(step.contains, "FOR UPDATE")
	}
	return step
}

func requestSQLStep(found bool) conversationSQLStep {
	step := conversationSQLStep{contains: []string{"SELECT * FROM `conversations`", "conversation_id = ? AND turn_id = ? AND user_id = ?"},
		args: map[int]interface{}{0: "conv", 1: "req", 2: int64(7)}, columns: []string{"turn_no", "question", "answer"}}
	if found {
		step.rows = [][]driver.Value{{int64(5), "问题", "答案"}}
	}
	return step
}

func sqlOp(name string) conversationSQLStep { return conversationSQLStep{contains: []string{name}} }

func TestAppendConversationTurnTransactionAndIdempotency(t *testing.T) {
	turn := model.Conversation{TurnID: "req", Question: "问题", Answer: "答案"}
	t.Run("archive before CAS then commit", func(t *testing.T) {
		repo := newConversationSQLTest(t, sqlOp("BEGIN"), stateSQLStep(8, 4, 2, true), requestSQLStep(false),
			conversationSQLStep{contains: []string{"INSERT INTO `conversations`"}, args: map[int]interface{}{0: int64(7), 1: "conv", 2: "req", 3: int64(5)}, affected: 1},
			conversationSQLStep{contains: []string{"UPDATE `conversation_states`", "`last_turn`=?", "`version`=?", "id = ? AND user_id = ? AND version = ?"}, affected: 1}, sqlOp("COMMIT"))
		if err := repo.AppendConversationTurn(context.Background(), 7, "conv", 8, turn); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("same request succeeds even with old version", func(t *testing.T) {
		repo := newConversationSQLTest(t, sqlOp("BEGIN"), stateSQLStep(9, 5, 2, true), requestSQLStep(true), sqlOp("COMMIT"))
		if err := repo.AppendConversationTurn(context.Background(), 7, "conv", 8, turn); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("different payload with reused request rejected", func(t *testing.T) {
		repo := newConversationSQLTest(t, sqlOp("BEGIN"), stateSQLStep(9, 5, 2, true), requestSQLStep(true), sqlOp("ROLLBACK"))
		changed := turn
		changed.Answer = "被覆盖的答案"
		if err := repo.AppendConversationTurn(context.Background(), 7, "conv", 8, changed); !errors.Is(err, ErrTurnIDConflict) {
			t.Fatal(err)
		}
	})
	t.Run("stale new request does not insert", func(t *testing.T) {
		repo := newConversationSQLTest(t, sqlOp("BEGIN"), stateSQLStep(9, 5, 2, true), requestSQLStep(false), sqlOp("ROLLBACK"))
		if err := repo.AppendConversationTurn(context.Background(), 7, "conv", 8, turn); !errors.Is(err, ErrConversationChanged) {
			t.Fatal(err)
		}
	})
	t.Run("CAS failure rolls back inserted turn", func(t *testing.T) {
		repo := newConversationSQLTest(t, sqlOp("BEGIN"), stateSQLStep(8, 4, 2, true), requestSQLStep(false),
			conversationSQLStep{contains: []string{"INSERT INTO `conversations`"}, affected: 1}, sqlOp("UPDATE `conversation_states`"), sqlOp("ROLLBACK"))
		if err := repo.AppendConversationTurn(context.Background(), 7, "conv", 8, turn); !errors.Is(err, ErrConversationChanged) {
			t.Fatal(err)
		}
	})
}

func TestConversationSummaryCASAndWatermark(t *testing.T) {
	t.Run("summary only advances state", func(t *testing.T) {
		repo := newConversationSQLTest(t, sqlOp("BEGIN"), stateSQLStep(3, 10, 2, true),
			conversationSQLStep{contains: []string{"UPDATE `conversation_states`", "`summary`=?", "`summary_until`=?", "`version`=?", "id = ? AND user_id = ? AND version = ?"}, affected: 1}, sqlOp("COMMIT"))
		if err := repo.UpdateConversationSummary(context.Background(), 7, "conv", 3, 7, `{"topic":"部署"}`); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		name           string
		version, until uint64
	}{{"stale version", 2, 7}, {"past archive", 3, 11}, {"backward watermark", 3, 1}, {"same watermark", 3, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newConversationSQLTest(t, sqlOp("BEGIN"), stateSQLStep(3, 10, 2, true), sqlOp("ROLLBACK"))
			if err := repo.UpdateConversationSummary(context.Background(), 7, "conv", tc.version, tc.until, `{}`); err == nil {
				t.Fatal("invalid summary accepted")
			}
		})
	}
	repo := newConversationSQLTest(t)
	if err := repo.UpdateConversationSummary(context.Background(), 7, "conv", 3, 7, `not JSON`); err == nil {
		t.Fatal("malformed summary accepted")
	}
}

type conversationCommandHook struct{ process func(redis.Cmder) }

func (h conversationCommandHook) BeforeProcess(ctx context.Context, _ redis.Cmder) (context.Context, error) {
	return ctx, errors.New("intercept test command")
}
func (h conversationCommandHook) AfterProcess(_ context.Context, cmd redis.Cmder) error {
	cmd.SetErr(nil)
	h.process(cmd)
	return cmd.Err()
}
func (conversationCommandHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, errors.New("unexpected pipeline")
}
func (conversationCommandHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func TestLegacyMigrationPreservesRawPairsAndFailsClosed(t *testing.T) {
	messages := []model.ChatMessage{{Role: "user", Content: "不要使用外部服务", Timestamp: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)}, {Role: "assistant", Content: "这是建议，不是决定"}}
	encoded, _ := json.Marshal(messages)
	missing := conversationSQLStep{contains: []string{"SELECT * FROM `conversation_states`", "user_id = ?"}, columns: []string{"id"}}
	for _, tc := range []struct {
		name, history string
		redisErr      error
		success       bool
	}{
		{"complete history", string(encoded), nil, true}, {"malformed JSON", "{bad", nil, false}, {"orphan message", `[{"role":"user","content":"不能丢"}]`, nil, false}, {"Redis unavailable", "", errors.New("offline"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []conversationSQLStep{missing}
			if tc.success {
				steps = append(steps, sqlOp("BEGIN"), conversationSQLStep{contains: []string{"INSERT INTO `conversation_states`"}, affected: 1},
					conversationSQLStep{contains: []string{"INSERT INTO `conversations`"}, args: map[int]interface{}{0: int64(7), 1: "conv", 3: int64(1), 4: messages[0].Content, 5: messages[1].Content}, affected: 1}, sqlOp("COMMIT"))
			}
			repo := newConversationSQLTest(t, steps...)
			client := redis.NewClient(&redis.Options{Addr: "unused:0"})
			t.Cleanup(func() { _ = client.Close() })
			repo.redisClient = client
			calls := 0
			client.AddHook(conversationCommandHook{process: func(cmd redis.Cmder) {
				calls++
				if calls == 1 {
					args := cmd.Args()
					if args[1] != legacyConversationScript || args[3] != "user:7:current_conversation" {
						t.Errorf("wrong migration script %v", args)
					}
					if tc.redisErr != nil {
						cmd.SetErr(tc.redisErr)
					} else {
						cmd.(*redis.Cmd).SetVal([]interface{}{"conv", tc.history})
					}
				} else {
					if !tc.success || cmd.Args()[1] != cacheConversationScript {
						t.Errorf("must not alter failed migration: %v", cmd.Args())
					}
					cmd.(*redis.Cmd).SetVal(int64(1))
				}
			}})
			id, err := repo.GetOrCreateConversationID(context.Background(), 7)
			if tc.success && (err != nil || id != "conv") {
				t.Fatalf("migration: %q, %v", id, err)
			}
			if !tc.success && err == nil {
				t.Fatal("unreadable legacy data was discarded")
			}
		})
	}
	t.Run("existing SQL state does not need Redis", func(t *testing.T) {
		repo := newConversationSQLTest(t, conversationSQLStep{contains: []string{"SELECT * FROM `conversation_states`", "user_id = ?"}, columns: []string{"id", "user_id"}, rows: [][]driver.Value{{"conv", int64(7)}}})
		if id, err := repo.GetOrCreateConversationID(context.Background(), 7); err != nil || id != "conv" {
			t.Fatalf("%q %v", id, err)
		}
	})
}

func TestConversationArchiveOwnerFilterAndRecentOrder(t *testing.T) {
	repo := newConversationSQLTest(t, conversationSQLStep{contains: []string{"SELECT * FROM `conversations`", "user_id = ? AND conversation_id = ? AND turn_no > ?", "ORDER BY turn_no ASC", "LIMIT ?"},
		args: map[int]interface{}{0: int64(7), 1: "conv", 2: int64(40), 3: int64(25)}, columns: []string{"turn_no", "question", "answer"}, rows: [][]driver.Value{{int64(41), "原话", "原答"}}},
		conversationSQLStep{contains: []string{"conversation_id = ?", "ORDER BY turn_no DESC", "LIMIT ?"}, args: map[int]interface{}{0: "conv", 1: int64(10)}, columns: []string{"turn_no", "question", "answer"}, rows: [][]driver.Value{{int64(42), "新问题", "新答案"}, {int64(41), "原话", "原答"}}})
	turns, err := repo.GetConversationTurns(context.Background(), 7, "conv", 40, 25)
	if err != nil || len(turns) != 1 || turns[0].TurnNo != 41 {
		t.Fatalf("%+v %v", turns, err)
	}
	history, err := repo.GetConversationHistory(context.Background(), "conv")
	if err != nil || len(history) != 4 || history[0].Content != "原话" || history[3].Content != "新答案" {
		t.Fatalf("%+v %v", history, err)
	}
}

func TestConversationAdminPageFiltersInSQLAndKeepsSources(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 2, 23, 59, 59, 0, time.UTC)
	userID := uint(7)
	created := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	repo := newConversationSQLTest(t,
		conversationSQLStep{contains: []string{"SELECT count(*) FROM `conversations`", "user_id = ?", "created_at >= ?", "created_at <= ?"},
			args: map[int]interface{}{0: int64(7), 1: start, 2: end}, columns: []string{"count(*)"}, rows: [][]driver.Value{{int64(21)}}},
		conversationSQLStep{contains: []string{"SELECT * FROM `conversations`", "user_id = ?", "created_at >= ?", "created_at <= ?", "ORDER BY created_at DESC, id DESC", "LIMIT"},
			args:    map[int]interface{}{0: int64(7), 1: start, 2: end},
			columns: []string{"id", "user_id", "conversation_id", "turn_id", "turn_no", "question", "answer", "sources", "created_at"},
			rows:    [][]driver.Value{{int64(9), int64(7), "conv", "req", int64(9), "问题", "答案", []byte(`[{"number":1,"documentId":11,"version":"v1","fileName":"报告.pdf"}]`), created}}},
	)
	turns, total, err := repo.FindConversationTurnsPage(context.Background(), &userID, &start, &end, 20, 10)
	if err != nil || total != 21 || len(turns) != 1 || len(turns[0].Sources) != 1 || turns[0].Sources[0].DocumentID != 11 {
		t.Fatalf("admin page lost archive/source data: total=%d turns=%+v err=%v", total, turns, err)
	}
}

func TestConversationHistoryValidationAndCompatibility(t *testing.T) {
	var messages []model.ChatMessage
	for i := 0; i < 12; i++ {
		messages = append(messages, model.ChatMessage{Role: "user", Content: fmt.Sprint(i)}, model.ChatMessage{Role: "assistant", Content: fmt.Sprint(i)})
	}
	if got := trimConversationHistory(messages); !reflect.DeepEqual(got, messages[4:]) {
		t.Fatal("cache must retain whole recent turns")
	}
	turns, err := turnsFromMessages(7, "conv", messages)
	if err != nil || len(turns) != 12 || turns[11].TurnNo != 12 {
		t.Fatalf("%+v %v", turns, err)
	}
	if err := validateHistoryAppend(turns[:10], turns); err != nil {
		t.Fatal(err)
	}
	if err := validateHistoryAppend(turns, turns[:10]); !errors.Is(err, ErrConversationChanged) {
		t.Fatal("stale snapshot accepted")
	}
	changed := append([]model.Conversation(nil), turns...)
	changed[0].Answer = "edited"
	if err := validateHistoryAppend(turns, changed); !errors.Is(err, ErrConversationChanged) {
		t.Fatal("edited history accepted")
	}
	for _, invalid := range [][]model.ChatMessage{messages[:1], {{Role: "assistant", Content: "orphan"}, {Role: "user", Content: "wrong order"}}, {{Role: "user", Content: "q"}, {Role: "assistant", Content: ""}}} {
		if _, err := turnsFromMessages(7, "conv", invalid); err == nil {
			t.Fatalf("invalid pair accepted: %+v", invalid)
		}
	}
	for _, id := range []string{"", "bad id", "中文", strings.Repeat("x", 65)} {
		if validConversationID(id) {
			t.Errorf("invalid ID accepted: %q", id)
		}
	}
	if err := validateTurn(7, "conv", model.Conversation{UserID: 8, TurnID: "req", Question: "q", Answer: "a"}); err == nil {
		t.Fatal("owner mismatch accepted")
	}
}
