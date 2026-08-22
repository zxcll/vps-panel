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
		//
		// 注意这个模型有个必须守住的规矩：**迁移文件描述的是"当前想要的
		// schema"**。后面的迁移放宽了前面某条约束，前面那个文件就得跟着改，
		// 不能只靠新文件去 DROP —— 否则每次启动都是"前面建、后面删"，
		// 一旦库里已经有了不满足旧约束的数据，前面那步就会把启动整个卡住。
		// 这个坑踩过一次（见 0002/0003 里的注释）。
		if _, err := s.db.Exec(string(b)); err != nil {
			return fmt.Errorf("执行迁移 %s: %w", name, err)
		}
	}
	return s.addMissingColumns()
}

// newColumn 是一条「表上该有但老库里可能还没有」的列。
type newColumn struct {
	table  string
	column string
	// ddl 是 ALTER TABLE ADD COLUMN 后面那一截，要带上类型和默认值。
	ddl string
}

// 给已有的库补列。
//
// 为什么不能写进 .sql 文件：SQLite 的 ALTER TABLE ADD COLUMN **没有**
// IF NOT EXISTS，而这个项目的迁移是每次启动全量重放的 —— 直接写进去，
// 第二次启动就会因为「列已存在」而报错，面板起不来。
//
// 所以走 PRAGMA table_info 查一下再决定加不加。CREATE TABLE 那边同时也
// 会带上新列，新装的库一次到位，这里只管老库。
var pendingColumns = []newColumn{
	{"cdt_accounts", "sync_interval_sec", "INTEGER NOT NULL DEFAULT 300"},
}

func (s *Store) addMissingColumns() error {
	for _, c := range pendingColumns {
		has, err := s.hasColumn(c.table, c.column)
		if err != nil {
			return fmt.Errorf("检查 %s.%s 是否存在: %w", c.table, c.column, err)
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.ddl)
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("给 %s 补列 %s: %w", c.table, c.column, err)
		}
	}
	return nil
}

// hasColumn 查表上有没有这一列。表本身不存在时返回 true，
// 让调用方跳过 —— 表都没有就轮不到补列，那是迁移文件的事。
func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	any := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false, err
		}
		any = true
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	// 一行都没有 = 表不存在。
	return !any, nil
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
