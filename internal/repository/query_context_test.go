package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This driver blocks a real database/sql query until its context is canceled.
type canceledQueryDriver struct{ started chan struct{} }

func (d *canceledQueryDriver) Open(string) (driver.Conn, error) {
	return &canceledQueryConn{started: d.started}, nil
}
func (d *canceledQueryDriver) Connect(context.Context) (driver.Conn, error) { return d.Open("") }
func (d *canceledQueryDriver) Driver() driver.Driver                        { return d }

type canceledQueryConn struct{ started chan struct{} }

func (c *canceledQueryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *canceledQueryConn) Close() error              { return nil }
func (c *canceledQueryConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected begin") }
func (c *canceledQueryConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	c.started <- struct{}{}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		return nil, errors.New("SQL query did not receive cancellation")
	}
}

func TestPermissionSQLQueriesReceiveCancellation(t *testing.T) {
	queries := map[string]func(context.Context, *gorm.DB) error{
		"profile": func(ctx context.Context, db *gorm.DB) error {
			_, err := NewUserRepository(db).FindByUsername(ctx, "alice")
			return err
		},
		"organization-tags": func(ctx context.Context, db *gorm.DB) error {
			_, err := NewOrgTagRepository(db).FindAll(ctx)
			return err
		},
		"accessible-files": func(ctx context.Context, db *gorm.DB) error {
			_, err := NewUploadRepository(db, nil).FindAccessibleFiles(ctx, 1, []string{"TEAM"})
			return err
		},
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			sqlDB := sql.OpenDB(&canceledQueryDriver{started: started})
			t.Cleanup(func() { _ = sqlDB.Close() })
			db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
				DisableAutomaticPing: true,
				Logger:               logger.Default.LogMode(logger.Silent),
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- query(ctx, db) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("SQL query did not start")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected canceled query, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("SQL query ignored request cancellation")
			}
		})
	}
}
