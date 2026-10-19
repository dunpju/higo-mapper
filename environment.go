package higomapper

// 数据库环境模块。
//
// Environment 封装 database/sql 连接池（*sql.DB），提供基于配置的
// 连接池创建与生命周期管理。URL 形式的连接串会被转换为对应驱动的 DSN：
//
//	mysql://user:pass@host:3306/db?x=y  →  user:pass@tcp(host:3306)/db?x=y
//	postgres://...                       →  原样交给 pgx（stdlib 接受 URL）
//	sqlite::memory:                      →  file::memory:?cache=shared
//	sqlite:///path/to/db.sqlite          →  file:/path/to/db.sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // mysql 驱动注册
	_ "github.com/jackc/pgx/v5/stdlib" // postgres 驱动注册
	_ "modernc.org/sqlite"             // sqlite 驱动注册（纯 Go，无 cgo）
)

// Environment 数据库环境：持有一个连接池，代表一个已配置的数据库环境。
//
// 通常由 NewSqlSessionFactory 创建，生命周期与应用相同。
type Environment struct {
	db     *sql.DB
	driver string
	url    string
}

// NewEnvironment 从配置创建数据库环境并建立连接池（含 Ping 校验）。
//
// 根据 driver 字段自动选择数据库后端；URL 形式的连接串被转换为驱动 DSN。
func NewEnvironment(ctx context.Context, cfg EnvironmentConfig) (*Environment, error) {
	driver := strings.ToLower(strings.TrimSpace(cfg.Driver))
	rawURL := cfg.URL

	switch driver {
	case "mysql", "postgres", "sqlite":
	default:
		return nil, newConfigError("不支持的数据库驱动: '%s'，支持的选项: mysql, postgres, sqlite", driver)
	}
	if rawURL == "" {
		return nil, newConfigError("数据库连接 URL 不能为空")
	}

	dsn, err := resolveDSN(driver, rawURL)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(driverNameOf(driver), dsn)
	if err != nil {
		return nil, newDatabaseConnectionError(err, rawURL)
	}
	db.SetMaxOpenConns(cfg.PoolMaxConnections)
	db.SetMaxIdleConns(cfg.PoolMinConnections)
	db.SetConnMaxLifetime(0)

	// 连接校验：确保配置真实可用（与 Rust 版 connect 即时建连一致）
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, newDatabaseConnectionError(err, rawURL)
	}

	return &Environment{db: db, driver: driver, url: rawURL}, nil
}

// driverNameOf 驱动标识 → database/sql 注册名
func driverNameOf(driver string) string {
	switch driver {
	case "postgres":
		return "pgx"
	default:
		return driver
	}
}

// resolveDSN URL 形式连接串 → 驱动 DSN。不成形 URL 时按原始 DSN 处理。
func resolveDSN(driver, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		// 不是 URL 形式：视为已是驱动 DSN（如 mysql 的 user:pass@tcp(...)/db）
		return rawURL, nil
	}

	switch driver {
	case "mysql":
		// mysql://user:pass@host:3306/db?x=y → user:pass@tcp(host:3306)/db?x=y
		if u.Scheme != "mysql" {
			return rawURL, nil
		}
		host := u.Host
		user := ""
		if u.User != nil {
			if pw, ok := u.User.Password(); ok {
				user = fmt.Sprintf("%s:%s", u.User.Username(), pw)
			} else {
				user = u.User.Username()
			}
		}
		dsn := ""
		if user != "" {
			dsn = user + "@"
		}
		dsn += "tcp(" + host + ")"
		dsn += u.Path
		if u.RawQuery != "" {
			dsn += "?" + u.RawQuery
		}
		return dsn, nil

	case "postgres":
		// postgres://... pgx stdlib 直接接受同形 URL（占位符 ? 自动重写为 $n）
		return rawURL, nil

	case "sqlite":
		// sqlite::memory: → file::memory:?cache=shared（池内共享内存库）
		if rawURL == "sqlite::memory:" {
			return "file::memory:?cache=shared", nil
		}
		// sqlite:///path → file:/path；sqlite:path → path
		if u.Scheme == "sqlite" {
			if strings.HasPrefix(u.Path, "/") {
				return "file:" + u.Path, nil
			}
			if u.Path != "" {
				return u.Path, nil
			}
			if strings.HasPrefix(u.Opaque, "/") {
				return "file:" + u.Opaque, nil
			}
			if u.Opaque != "" {
				return u.Opaque, nil
			}
			return rawURL, nil
		}
		return rawURL, nil
	}
	return rawURL, nil
}

// DB 获取连接池
func (e *Environment) DB() *sql.DB { return e.db }

// Driver 获取驱动标识（"mysql" | "postgres" | "sqlite"）
func (e *Environment) Driver() string { return e.driver }

// URL 获取连接 URL
func (e *Environment) URL() string { return e.url }

// Close 关闭连接池（等待所有活跃连接归还后关闭）
func (e *Environment) Close() error { return e.db.Close() }

// EnvironmentRegistry 支持多个命名环境的容器（多数据库场景）
type EnvironmentRegistry struct {
	environments map[string]*Environment
}

// NewEnvironmentRegistry 创建空注册表
func NewEnvironmentRegistry() *EnvironmentRegistry {
	return &EnvironmentRegistry{environments: map[string]*Environment{}}
}

// Insert 注册一个命名环境
func (r *EnvironmentRegistry) Insert(name string, env *Environment) {
	r.environments[name] = env
}

// Get 获取指定名称的环境；不存在返回 nil
func (r *EnvironmentRegistry) Get(name string) *Environment {
	return r.environments[name]
}

// Names 获取所有已注册的环境名
func (r *EnvironmentRegistry) Names() []string {
	names := make([]string, 0, len(r.environments))
	for k := range r.environments {
		names = append(names, k)
	}
	return names
}

// CloseAll 关闭所有连接池
func (r *EnvironmentRegistry) CloseAll() error {
	var firstErr error
	for _, env := range r.environments {
		if err := env.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
