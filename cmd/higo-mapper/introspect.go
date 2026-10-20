// 数据库结构内省（schema introspection）。
//
// 支持 MySQL（information_schema）与 SQLite（PRAGMA table_info / sqlite_master），
// 产出 TableMeta（表名 + 列元数据），供代码生成消费。

package main

import (
	"database/sql"
	"fmt"
	"strings"

	higomapper "github.com/dunpju/higo-mapper"
)

// ColumnMeta 单列元数据
type ColumnMeta struct {
	// Name 列名
	Name string
	// DataType 声明类型（MySQL DATA_TYPE 如 "varchar"；SQLite 亲和声明如 "TEXT"）
	DataType string
	// FullType 完整列类型（MySQL COLUMN_TYPE 如 "varchar(255)"；SQLite 同 DataType）
	FullType string
	// Nullable 是否可空
	Nullable bool
	// IsPrimaryKey 是否主键
	IsPrimaryKey bool
}

// TableMeta 单表元数据
type TableMeta struct {
	// Name 表名
	Name string
	// Columns 列（按库内顺序）
	Columns []ColumnMeta
}

// PrimaryKey 主键列（无主键返回 nil）
func (m *TableMeta) PrimaryKey() *ColumnMeta {
	for i := range m.Columns {
		if m.Columns[i].IsPrimaryKey {
			return &m.Columns[i]
		}
	}
	return nil
}

// dialectOfURL 从连接 URL 推断方言（scheme）
func dialectOfURL(url string) (string, bool) {
	scheme := strings.ToLower(strings.SplitN(url, ":", 2)[0])
	switch scheme {
	case "mysql", "mariadb":
		return "mysql", true
	case "sqlite":
		return "sqlite", true
	}
	return "", false
}

// resolveCLIDSN / driverCLIDriver 复用运行时的 URL→DSN 与驱动名解析
func resolveCLIDSN(dialect, rawURL string) (string, error) {
	return higomapper.ResolveDSN(dialect, rawURL)
}

func driverCLIDriver(dialect string) string {
	return higomapper.DriverNameOf(dialect)
}

// listTables 列出库内全部用户表
func listTables(db *sql.DB, dialect string) ([]string, error) {
	var query string
	switch dialect {
	case "mysql":
		query = "SELECT TABLE_NAME FROM information_schema.TABLES " +
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME"
	case "sqlite":
		query = "SELECT name FROM sqlite_master WHERE type = 'table' " +
			"AND name NOT LIKE 'sqlite_%' ORDER BY name"
	default:
		return nil, fmt.Errorf("不支持的方言: %s", dialect)
	}
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// introspectTable 内省单张表
func introspectTable(db *sql.DB, dialect, table string) (*TableMeta, error) {
	var columns []ColumnMeta
	var err error
	switch dialect {
	case "mysql":
		columns, err = introspectMySQL(db, table)
	case "sqlite":
		columns, err = introspectSQLite(db, table)
	default:
		return nil, fmt.Errorf("不支持的方言: %s", dialect)
	}
	if err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("表 '%s' 不存在或没有列", table)
	}
	return &TableMeta{Name: table, Columns: columns}, nil
}

// introspectMySQL information_schema.COLUMNS（COLUMN_KEY = 'PRI' 即主键）
func introspectMySQL(db *sql.DB, table string) ([]ColumnMeta, error) {
	rows, err := db.Query(
		"SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, COLUMN_KEY "+
			"FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnMeta
	for rows.Next() {
		var name, dataType, fullType, nullable, key string
		if err := rows.Scan(&name, &dataType, &fullType, &nullable, &key); err != nil {
			return nil, err
		}
		out = append(out, ColumnMeta{
			Name:         name,
			DataType:     dataType,
			FullType:     fullType,
			Nullable:     strings.EqualFold(nullable, "YES"),
			IsPrimaryKey: key == "PRI",
		})
	}
	return out, rows.Err()
}

// introspectSQLite PRAGMA table_info（pk > 0 即主键）
func introspectSQLite(db *sql.DB, table string) ([]ColumnMeta, error) {
	// PRAGMA 不支持参数绑定；表名经 quoteIdent 双引号转义后拼接（防注入）
	rows, err := db.Query("PRAGMA table_info(" + quoteIdent(table) + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnMeta
	for rows.Next() {
		var cid, notnull, pk int64
		var name, ty string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ty, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, ColumnMeta{
			Name:         name,
			DataType:     ty,
			FullType:     ty,
			Nullable:     notnull == 0,
			IsPrimaryKey: pk > 0,
		})
	}
	return out, rows.Err()
}

// quoteIdent 标识符引用（PRAGMA 表名参数化不支持，用双引号转义防注入）
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
