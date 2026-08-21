// Package store 是 SQLite 数据访问层。
//
// 所有时间在库里都是 Unix 秒（UTC）；Go 侧统一用 time.Time（UTC）。
// 用 nullTime/timeVal 这一对辅助函数在两者之间转换。
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库并执行迁移。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建数据目录: %w", err)
		}
	}

	// 三个关键参数，缺一个都会在多探针并发上报时炸出 SQLITE_BUSY：
	//
	//   journal_mode=WAL   读写不互相阻塞（探针在写，面板页面在读）
	//   busy_timeout       拿不到锁时自动等待重试，而不是立刻报错
	//   _txlock=immediate  事务一开始就申请写锁。默认的 deferred 事务是先读后写，
	//                      升级写锁时如果撞上别的写者，SQLite 会直接返回 BUSY 且
	//                      **不走 busy_timeout 重试**——流量入账正好是"先读计数器
	//                      再写"的模式，不改成 immediate 必然踩这个坑。
	dsn := path + "?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}

	// SQLite 同时只允许一个写者。连接开多了只会让并发写互相排队等锁，
	// 反而放大 BUSY 的概率；4 个够用了。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

func (s *Store) migrate() error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("读取迁移目录: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		b, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("读取迁移 %s: %w", name, err)
		}
		// 迁移全部写成幂等的（IF NOT EXISTS），所以每次启动直接重放即可，
		// 不需要额外的版本表。
		if _, err := s.db.Exec(string(b)); err != nil {
			return fmt.Errorf("执行迁移 %s: %w", name, err)
		}
	}
	return nil
}

// --- 时间转换辅助 ---

func timeVal(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func nullTime(t *time.Time) sql.NullInt64 {
	if t == nil || t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UTC().Unix(), Valid: true}
}

func timePtr(v sql.NullInt64) *time.Time {
	if !v.Valid || v.Int64 == 0 {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

func nullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func int64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}
