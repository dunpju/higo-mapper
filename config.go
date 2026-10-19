package higomapper

// 配置管理模块。
//
// 解析 higo-mapper.toml 配置文件，定义数据库连接、mapper 路径、类型别名等。
// 同时提供基于 glob（支持 ** 跨层级）的 XML mapper 文件发现能力。
//
// # 配置优先级（高 → 低）
//
//  1. 环境变量（HIGO_MAPPER_*，部署/CI 覆盖用）
//  2. 编程式 builder（With* 方法）
//  3. TOML 文件默认值（LoadConfigFile）
//
// env 层只在变量存在时覆盖对应字段（缺失则保留编程/TOML 值）。通过应用顺序编码优先级——
// 把 WithEnvOverrides() 放在链式调用最后：
//
//	config, err := LoadConfigFile("higo-mapper.toml") // 3. TOML
//	config = config.WithURL("programmatic-url")       // 2. 编程
//	config, err = config.WithEnvOverrides()           // 1. env（最高）
//
// # 支持的环境变量
//
//	变量                                覆盖字段                          解析
//	HIGO_MAPPER_DRIVER                  environment.driver               字符串
//	HIGO_MAPPER_URL（或 DATABASE_URL）  environment.url                  字符串（前者优先）
//	HIGO_MAPPER_POOL_MAX                environment.pool_max_connections int
//	HIGO_MAPPER_POOL_MIN                environment.pool_min_connections int
//	HIGO_MAPPER_PATHS                   settings.mapper_paths           逗号分隔列表
//	HIGO_MAPPER_REFRESH_MS              settings.mapper_refresh_interval_ms
//	HIGO_MAPPER_SQL_LOG                 settings.sql_log                布尔（true/1/yes/false/0/no）
//	HIGO_MAPPER_SQL_LOG_SLOW_MS         settings.sql_log_slow_threshold_ms
//	HIGO_MAPPER_TYPE_ALIASES            type_aliases                    逗号分隔 name=type（合并）

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/bmatcuk/doublestar/v4"
)

// Config 根配置结构，对应 higo-mapper.toml
type Config struct {
	// Environment 主数据库环境
	Environment EnvironmentConfig
	// Environments 可选的命名环境（多数据库支持）
	Environments map[string]EnvironmentConfig
	// Settings 全局设置
	Settings SettingsConfig
	// TypeAliases 类型别名：XML 短名 → Go 全限定类型名
	TypeAliases map[string]string
	// TypeHandlers 自定义类型处理器注册项（解析保留；处理器需编程式注册）
	TypeHandlers []TypeHandlerEntry
}

// EnvironmentConfig 数据库环境配置
type EnvironmentConfig struct {
	// Driver 驱动类型："mysql" | "postgres" | "sqlite"
	Driver string
	// URL 数据库连接 URL
	URL string
	// PoolMaxConnections 连接池最大连接数
	PoolMaxConnections int
	// PoolMinConnections 连接池最小连接数
	PoolMinConnections int
}

// SettingsConfig 全局设置
type SettingsConfig struct {
	// MapperPaths XML mapper 文件的 glob 模式列表
	MapperPaths []string
	// MapperRefreshIntervalMs 热重载轮询间隔（毫秒），0 表示禁用
	MapperRefreshIntervalMs uint64
	// SqlLog 是否开启 SQL 执行日志（耗时 + 可读 SQL；默认关闭）
	SqlLog bool
	// SqlLogSlowThresholdMs 慢查询阈值（毫秒）；仅记录耗时 ≥ 此值的 SQL。0 表示记录全部。
	SqlLogSlowThresholdMs uint64
}

// TypeHandlerEntry 自定义类型处理器注册项
type TypeHandlerEntry struct {
	// Type Go 类型全限定路径
	Type string `toml:"type"`
	// Handler 处理器全限定路径
	Handler string `toml:"handler"`
}

// DefaultConfig 默认配置（连接池 10/2，mapper 路径 mappers/**/*.xml）
func DefaultConfig() *Config {
	return &Config{
		Environment:  defaultEnvironmentConfig(),
		Environments: map[string]EnvironmentConfig{},
		Settings:     defaultSettingsConfig(),
		TypeAliases:  map[string]string{},
		TypeHandlers: nil,
	}
}

func defaultEnvironmentConfig() EnvironmentConfig {
	return EnvironmentConfig{PoolMaxConnections: 10, PoolMinConnections: 2}
}

func defaultSettingsConfig() SettingsConfig {
	return SettingsConfig{MapperPaths: []string{"mappers/**/*.xml"}}
}

// ─── TOML 解析（中间结构 + 指针字段，精确区分「缺失」与「零值」）──────

type rawConfig struct {
	Environment  rawEnvironment            `toml:"environment"`
	Environments map[string]rawEnvironment `toml:"environments"`
	Settings     rawSettings               `toml:"settings"`
	TypeAliases  map[string]string         `toml:"type_aliases"`
	TypeHandlers []TypeHandlerEntry        `toml:"type_handlers"`
}

type rawEnvironment struct {
	Driver             string `toml:"driver"`
	URL                string `toml:"url"`
	PoolMaxConnections *int   `toml:"pool_max_connections"`
	PoolMinConnections *int   `toml:"pool_min_connections"`
}

type rawSettings struct {
	MapperPaths             []string `toml:"mapper_paths"`
	MapperRefreshIntervalMs uint64   `toml:"mapper_refresh_interval_ms"`
	SqlLog                  *bool    `toml:"sql_log"`
	SqlLogSlowThresholdMs   uint64   `toml:"sql_log_slow_threshold_ms"`
}

func (r rawEnvironment) toConfig() EnvironmentConfig {
	c := defaultEnvironmentConfig()
	c.Driver = r.Driver
	c.URL = r.URL
	if r.PoolMaxConnections != nil {
		c.PoolMaxConnections = *r.PoolMaxConnections
	}
	if r.PoolMinConnections != nil {
		c.PoolMinConnections = *r.PoolMinConnections
	}
	return c
}

func (r rawSettings) toConfig() SettingsConfig {
	c := defaultSettingsConfig()
	if len(r.MapperPaths) > 0 {
		c.MapperPaths = r.MapperPaths
	}
	c.MapperRefreshIntervalMs = r.MapperRefreshIntervalMs
	if r.SqlLog != nil {
		c.SqlLog = *r.SqlLog
	}
	c.SqlLogSlowThresholdMs = r.SqlLogSlowThresholdMs
	return c
}

// ParseConfigToml 从 TOML 字符串解析配置
func ParseConfigToml(tomlStr string) (*Config, error) {
	var raw rawConfig
	if _, err := toml.Decode(tomlStr, &raw); err != nil {
		return nil, newConfigErrorWrap(err, "TOML 解析失败")
	}
	cfg := &Config{
		Environment:  raw.Environment.toConfig(),
		Settings:     raw.Settings.toConfig(),
		TypeAliases:  map[string]string{},
		TypeHandlers: raw.TypeHandlers,
	}
	if raw.Environments != nil {
		cfg.Environments = make(map[string]EnvironmentConfig, len(raw.Environments))
		for k, e := range raw.Environments {
			cfg.Environments[k] = e.toConfig()
		}
	} else {
		cfg.Environments = map[string]EnvironmentConfig{}
	}
	for k, v := range raw.TypeAliases {
		cfg.TypeAliases[k] = v
	}
	return cfg, nil
}

// LoadConfigFile 从文件加载配置
func LoadConfigFile(path string) (*Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, newConfigErrorWrap(err, "无法读取配置文件 %s", path)
	}
	return ParseConfigToml(string(content))
}

// ─── 编程式 builder（链式）────────────────────────────────────────

// WithEnvironment 设置主环境
func (c *Config) WithEnvironment(env EnvironmentConfig) *Config {
	c.Environment = env
	return c
}

// WithDriver 单独覆盖 driver（不影响 url / 连接池等其他环境字段）
func (c *Config) WithDriver(driver string) *Config {
	c.Environment.Driver = driver
	return c
}

// WithURL 单独覆盖数据库连接 URL
func (c *Config) WithURL(url string) *Config {
	c.Environment.URL = url
	return c
}

// WithPoolMaxConnections 单独覆盖连接池最大连接数
func (c *Config) WithPoolMaxConnections(max int) *Config {
	c.Environment.PoolMaxConnections = max
	return c
}

// WithPoolMinConnections 单独覆盖连接池最小连接数
func (c *Config) WithPoolMinConnections(min int) *Config {
	c.Environment.PoolMinConnections = min
	return c
}

// WithTypeAlias 注册单个类型别名（合并入现有 map）
func (c *Config) WithTypeAlias(alias, fullPath string) *Config {
	if c.TypeAliases == nil {
		c.TypeAliases = map[string]string{}
	}
	c.TypeAliases[alias] = fullPath
	return c
}

// WithTypeAliases 整体替换类型别名表
func (c *Config) WithTypeAliases(aliases map[string]string) *Config {
	c.TypeAliases = aliases
	return c
}

// WithMapperPaths 设置 mapper 路径
func (c *Config) WithMapperPaths(paths []string) *Config {
	c.Settings.MapperPaths = paths
	return c
}

// WithHotReload 启用热重载
func (c *Config) WithHotReload(intervalMs uint64) *Config {
	c.Settings.MapperRefreshIntervalMs = intervalMs
	return c
}

// WithSqlLog 开启 SQL 执行日志（耗时 + 可读 SQL）。
//
// 等价于 toml [settings] sql_log = true。阈值可用 WithSqlLogSlowThresholdMs 设置。
func (c *Config) WithSqlLog(enabled bool) *Config {
	c.Settings.SqlLog = enabled
	return c
}

// WithSqlLogSlowThresholdMs 设置慢查询日志阈值（毫秒）：
// 仅记录耗时 ≥ 此值的 SQL。0 表示记录全部。
func (c *Config) WithSqlLogSlowThresholdMs(thresholdMs uint64) *Config {
	c.Settings.SqlLogSlowThresholdMs = thresholdMs
	return c
}

// ─── 环境变量覆盖层 ───────────────────────────────────────────────

// EnvSource 环境变量来源抽象。
//
// 生产用 StdEnv（读 os.Environ）；测试可传入自定义实现以避免并行测试竞态。
type EnvSource interface {
	// Get 读取指定变量；不存在返回 ("", false)
	Get(key string) (string, bool)
}

// StdEnv 进程环境变量源（生产用）
type StdEnv struct{}

// Get 实现 EnvSource
func (StdEnv) Get(key string) (string, bool) {
	return os.LookupEnv(key)
}

// 环境变量名常量（集中定义，便于维护与文档）
const (
	envDriver       = "HIGO_MAPPER_DRIVER"
	envURL          = "HIGO_MAPPER_URL"
	envDatabaseURL  = "DATABASE_URL"
	envPoolMax      = "HIGO_MAPPER_POOL_MAX"
	envPoolMin      = "HIGO_MAPPER_POOL_MIN"
	envPaths        = "HIGO_MAPPER_PATHS"
	envRefreshMs    = "HIGO_MAPPER_REFRESH_MS"
	envSqlLog       = "HIGO_MAPPER_SQL_LOG"
	envSqlLogSlowMs = "HIGO_MAPPER_SQL_LOG_SLOW_MS"
	envTypeAliases  = "HIGO_MAPPER_TYPE_ALIASES"
)

// ApplyEnvOverridesFrom 从指定 env 源应用环境变量覆盖（仅覆盖已设置的变量）。
//
// 优先级最高：在 TOML / 编程设置之后调用。变量缺失则保留原值；
// 解析失败返回 Config 错误（不静默吞错）。
func (c *Config) ApplyEnvOverridesFrom(src EnvSource) error {
	if v, ok := src.Get(envDriver); ok {
		c.Environment.Driver = v
	}
	// HIGO_MAPPER_URL 优先于 DATABASE_URL 别名
	if v, ok := src.Get(envURL); ok {
		c.Environment.URL = v
	} else if v, ok := src.Get(envDatabaseURL); ok {
		c.Environment.URL = v
	}
	if v, ok := src.Get(envPoolMax); ok {
		n, err := parseEnvInt(v, envPoolMax)
		if err != nil {
			return err
		}
		c.Environment.PoolMaxConnections = n
	}
	if v, ok := src.Get(envPoolMin); ok {
		n, err := parseEnvInt(v, envPoolMin)
		if err != nil {
			return err
		}
		c.Environment.PoolMinConnections = n
	}
	if v, ok := src.Get(envPaths); ok {
		var paths []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
		c.Settings.MapperPaths = paths
	}
	if v, ok := src.Get(envRefreshMs); ok {
		n, err := parseEnvUint64(v, envRefreshMs)
		if err != nil {
			return err
		}
		c.Settings.MapperRefreshIntervalMs = n
	}
	if v, ok := src.Get(envSqlLog); ok {
		b, err := parseEnvBool(v, envSqlLog)
		if err != nil {
			return err
		}
		c.Settings.SqlLog = b
	}
	if v, ok := src.Get(envSqlLogSlowMs); ok {
		n, err := parseEnvUint64(v, envSqlLogSlowMs)
		if err != nil {
			return err
		}
		c.Settings.SqlLogSlowThresholdMs = n
	}
	if v, ok := src.Get(envTypeAliases); ok {
		pairs, err := parseEnvAliases(v)
		if err != nil {
			return err
		}
		if c.TypeAliases == nil {
			c.TypeAliases = map[string]string{}
		}
		for k, t := range pairs {
			c.TypeAliases[k] = t
		}
	}
	return nil
}

// ApplyEnvOverrides 从进程环境变量（os.Environ）应用覆盖
func (c *Config) ApplyEnvOverrides() error {
	return c.ApplyEnvOverridesFrom(StdEnv{})
}

// WithEnvOverridesFrom 应用 env 覆盖后返回自身（链式，来自指定源）
func (c *Config) WithEnvOverridesFrom(src EnvSource) (*Config, error) {
	if err := c.ApplyEnvOverridesFrom(src); err != nil {
		return nil, err
	}
	return c, nil
}

// WithEnvOverrides 应用进程环境变量覆盖后返回自身（链式）
func (c *Config) WithEnvOverrides() (*Config, error) {
	if err := c.ApplyEnvOverrides(); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadLayeredConfig 一站式分层加载：TOML 文件 → 进程环境变量覆盖
func LoadLayeredConfig(path string) (*Config, error) {
	cfg, err := LoadConfigFile(path)
	if err != nil {
		return nil, err
	}
	return cfg.WithEnvOverrides()
}

// ─── env 解析辅助 ─────────────────────────────────────────────────

func parseEnvInt(v, name string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, newConfigError("环境变量 %s 期望整数，实际 '%s'", name, v)
	}
	return n, nil
}

func parseEnvUint64(v, name string) (uint64, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, newConfigError("环境变量 %s 期望无符号整数，实际 '%s'", name, v)
	}
	return n, nil
}

// parseEnvBool 解析布尔：true/1/yes/on → true；false/0/no/off → false（大小写不敏感）
func parseEnvBool(v, name string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	}
	return false, newConfigError("环境变量 %s 期望布尔（true/false/1/0/yes/no），实际 '%s'", name, v)
}

// parseEnvAliases 解析类型别名：`int=i32,long=i64` → {int: i32, long: i64}
func parseEnvAliases(v string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, t, found := strings.Cut(pair, "=")
		if !found {
			return nil, newConfigError(
				"环境变量 %s 期望 'name=type' 形式（逗号分隔），无效项 '%s'", envTypeAliases, pair)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(t)
	}
	return out, nil
}

// ─── mapper 文件发现 ──────────────────────────────────────────────

// DiscoverMapperFiles 按 glob 模式发现所有 XML mapper 文件（支持 ** 跨层级）。
//
// 相对路径基于 baseDir 解析；返回排序去重后的绝对文件路径列表。
func (c *Config) DiscoverMapperFiles(baseDir string) ([]string, error) {
	// baseDir 统一绝对化（doublestar 走 io/fs，需要确定性的根）
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, newConfigErrorWrap(err, "无法解析基准目录 %s", baseDir)
	}
	var files []string
	for _, pattern := range c.Settings.MapperPaths {
		// 处理绝对/相对 glob 模式（doublestar 统一用 / 分隔）
		full := pattern
		if !filepath.IsAbs(pattern) {
			full = filepath.ToSlash(filepath.Join(absBase, pattern))
		} else {
			full = filepath.ToSlash(pattern)
		}
		matches, err := doublestarGlob(full)
		if err != nil {
			return nil, newConfigError("无效的 glob 模式 '%s': %v", pattern, err)
		}
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
				files = append(files, m)
			}
		}
	}
	sort.Strings(files)
	// 去重（多个模式可能命中同一文件）
	deduped := files[:0]
	var prev string
	for i, f := range files {
		if i == 0 || f != prev {
			deduped = append(deduped, f)
		}
		prev = f
	}
	return deduped, nil
}

// doublestarGlob 在操作系统文件系统上执行 ** glob。
//
// doublestar v4 走 io/fs 接口：把绝对路径拆成「卷根 DirFS + 相对模式」，
// 匹配结果再拼回完整路径。
func doublestarGlob(absPattern string) ([]string, error) {
	vol := filepath.VolumeName(absPattern)
	rest := strings.TrimPrefix(absPattern[len(vol):], "/")
	root := vol
	if root == "" {
		root = "/"
	}
	fsys := os.DirFS(root)
	matches, err := doublestar.Glob(fsys, rest)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, root+"/"+m)
	}
	return out, nil
}
