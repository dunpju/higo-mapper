package higomapper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── 配置测试 ─────────────────────────────────────────────────────

func TestParseTomlBasic(t *testing.T) {
	tomlStr := `
[environment]
driver = "mysql"
url = "mysql://user:pass@localhost:3306/mydb"
pool_max_connections = 20

[settings]
mapper_paths = ["mappers/**/*.xml"]
mapper_refresh_interval_ms = 5000

[type_aliases]
"int" = "i32"
"long" = "i64"
`
	config, err := ParseConfigToml(tomlStr)
	if err != nil {
		t.Fatal(err)
	}
	if config.Environment.Driver != "mysql" {
		t.Errorf("driver: %s", config.Environment.Driver)
	}
	if config.Environment.URL != "mysql://user:pass@localhost:3306/mydb" {
		t.Errorf("url: %s", config.Environment.URL)
	}
	if config.Environment.PoolMaxConnections != 20 {
		t.Errorf("pool_max: %d", config.Environment.PoolMaxConnections)
	}
	if config.Environment.PoolMinConnections != 2 { // 默认值
		t.Errorf("pool_min 默认应为 2: %d", config.Environment.PoolMinConnections)
	}
	if len(config.Settings.MapperPaths) != 1 || config.Settings.MapperPaths[0] != "mappers/**/*.xml" {
		t.Errorf("mapper_paths: %v", config.Settings.MapperPaths)
	}
	if config.Settings.MapperRefreshIntervalMs != 5000 {
		t.Errorf("refresh: %d", config.Settings.MapperRefreshIntervalMs)
	}
	if config.TypeAliases["int"] != "i32" {
		t.Errorf("aliases: %v", config.TypeAliases)
	}
}

func TestParseTomlMinimal(t *testing.T) {
	config, err := ParseConfigToml(`
[environment]
driver = "sqlite"
url = "sqlite::memory:"
`)
	if err != nil {
		t.Fatal(err)
	}
	if config.Environment.PoolMaxConnections != 10 { // 默认
		t.Errorf("pool_max 默认应为 10: %d", config.Environment.PoolMaxConnections)
	}
	if len(config.Settings.MapperPaths) == 0 { // 默认
		t.Error("mapper_paths 默认应为 mappers/**/*.xml")
	}
}

func TestParseTomlInvalid(t *testing.T) {
	if _, err := ParseConfigToml("this is not valid toml {{{"); err == nil {
		t.Error("无效 TOML 应报错")
	}
}

func TestParseSqlLogSettings(t *testing.T) {
	config, err := ParseConfigToml(`
[environment]
driver = "sqlite"
url = "sqlite::memory:"

[settings]
sql_log = true
sql_log_slow_threshold_ms = 100
`)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Settings.SqlLog || config.Settings.SqlLogSlowThresholdMs != 100 {
		t.Errorf("sql_log: %+v", config.Settings)
	}

	// 默认（缺省字段）应为关闭、阈值 0
	minimal, err := ParseConfigToml("[environment]\ndriver = \"sqlite\"\nurl = \"sqlite::memory:\"")
	if err != nil {
		t.Fatal(err)
	}
	if minimal.Settings.SqlLog || minimal.Settings.SqlLogSlowThresholdMs != 0 {
		t.Errorf("默认应为关闭: %+v", minimal.Settings)
	}
}

func TestBuilderPattern(t *testing.T) {
	config := DefaultConfig().
		WithEnvironment(EnvironmentConfig{
			Driver: "sqlite", URL: "sqlite::memory:",
			PoolMaxConnections: 5, PoolMinConnections: 1,
		}).
		WithMapperPaths([]string{"sql/**/*.xml"}).
		WithHotReload(1000)
	if config.Environment.Driver != "sqlite" {
		t.Errorf("driver: %s", config.Environment.Driver)
	}
	if len(config.Settings.MapperPaths) != 1 || config.Settings.MapperPaths[0] != "sql/**/*.xml" {
		t.Errorf("paths: %v", config.Settings.MapperPaths)
	}
	if config.Settings.MapperRefreshIntervalMs != 1000 {
		t.Errorf("refresh: %d", config.Settings.MapperRefreshIntervalMs)
	}
}

func TestGranularSetterDoesNotResetOthers(t *testing.T) {
	config := DefaultConfig().
		WithEnvironment(EnvironmentConfig{
			Driver: "mysql", URL: "mysql://x",
			PoolMaxConnections: 30, PoolMinConnections: 5,
		}).
		WithDriver("postgres")
	if config.Environment.Driver != "postgres" {
		t.Errorf("driver: %s", config.Environment.Driver)
	}
	if config.Environment.URL != "mysql://x" { // 未被重置
		t.Errorf("url 被意外重置: %s", config.Environment.URL)
	}
	if config.Environment.PoolMaxConnections != 30 {
		t.Errorf("pool_max: %d", config.Environment.PoolMaxConnections)
	}
}

func TestTypeAliasSetters(t *testing.T) {
	config := DefaultConfig().WithTypeAlias("int", "i32").WithTypeAlias("long", "i64")
	if config.TypeAliases["int"] != "i32" || config.TypeAliases["long"] != "i64" {
		t.Errorf("aliases: %v", config.TypeAliases)
	}
	// 整体替换
	config = config.WithTypeAliases(map[string]string{"dec": "f64"})
	if len(config.TypeAliases) != 1 || config.TypeAliases["int"] != "" {
		t.Errorf("整体替换后: %v", config.TypeAliases)
	}
}

// ─── env 覆盖测试（TestEnv，避免 os 环境并行竞态）─────────────────

// testEnv 测试用 env 源
type testEnv map[string]string

func (e testEnv) Get(key string) (string, bool) {
	v, ok := e[key]
	return v, ok
}

func TestEnvOverrideAllFields(t *testing.T) {
	env := testEnv{
		envDriver:       "postgres",
		envURL:          "postgres://h/db",
		envPoolMax:      "88",
		envPoolMin:      "8",
		envPaths:        "a/*.xml, b/**/*.xml",
		envRefreshMs:    "1234",
		envSqlLog:       "true",
		envSqlLogSlowMs: "200",
		envTypeAliases:  "int=i32, long=i64",
	}
	config := DefaultConfig()
	if err := config.ApplyEnvOverridesFrom(env); err != nil {
		t.Fatal(err)
	}
	if config.Environment.Driver != "postgres" {
		t.Errorf("driver: %s", config.Environment.Driver)
	}
	if config.Environment.URL != "postgres://h/db" {
		t.Errorf("url: %s", config.Environment.URL)
	}
	if config.Environment.PoolMaxConnections != 88 || config.Environment.PoolMinConnections != 8 {
		t.Errorf("pool: %d/%d", config.Environment.PoolMaxConnections, config.Environment.PoolMinConnections)
	}
	if len(config.Settings.MapperPaths) != 2 ||
		config.Settings.MapperPaths[0] != "a/*.xml" || config.Settings.MapperPaths[1] != "b/**/*.xml" {
		t.Errorf("paths: %v", config.Settings.MapperPaths)
	}
	if config.Settings.MapperRefreshIntervalMs != 1234 {
		t.Errorf("refresh: %d", config.Settings.MapperRefreshIntervalMs)
	}
	if !config.Settings.SqlLog || config.Settings.SqlLogSlowThresholdMs != 200 {
		t.Errorf("sql_log: %+v", config.Settings)
	}
	if config.TypeAliases["int"] != "i32" || config.TypeAliases["long"] != "i64" {
		t.Errorf("aliases: %v", config.TypeAliases)
	}
}

func TestEnvOverridePriorityEnvWins(t *testing.T) {
	env := testEnv{envURL: "env-url"}
	config, err := DefaultConfig().WithURL("programmatic-url").WithEnvOverridesFrom(env)
	if err != nil {
		t.Fatal(err)
	}
	if config.Environment.URL != "env-url" {
		t.Errorf("env 应胜: %s", config.Environment.URL)
	}
}

func TestEnvOverrideAbsentKeepsProgrammatic(t *testing.T) {
	config, err := DefaultConfig().
		WithDriver("mysql").WithURL("kept-url").WithPoolMaxConnections(15).
		WithEnvOverridesFrom(testEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if config.Environment.Driver != "mysql" || config.Environment.URL != "kept-url" ||
		config.Environment.PoolMaxConnections != 15 {
		t.Errorf("env 未设应保留: %+v", config.Environment)
	}
}

func TestDatabaseURLAlias(t *testing.T) {
	// HIGO_MAPPER_URL 优先于 DATABASE_URL
	env := testEnv{envDatabaseURL: "db-url-alias", envURL: "explicit-url"}
	config := DefaultConfig()
	if err := config.ApplyEnvOverridesFrom(env); err != nil {
		t.Fatal(err)
	}
	if config.Environment.URL != "explicit-url" {
		t.Errorf("url: %s", config.Environment.URL)
	}
	// 仅 DATABASE_URL 时生效
	env = testEnv{envDatabaseURL: "db-url-alias"}
	config = DefaultConfig().WithURL("orig")
	if err := config.ApplyEnvOverridesFrom(env); err != nil {
		t.Fatal(err)
	}
	if config.Environment.URL != "db-url-alias" {
		t.Errorf("url: %s", config.Environment.URL)
	}
}

func TestEnvBoolVariants(t *testing.T) {
	cases := []struct {
		raw      string
		expected bool
	}{
		{"true", true}, {"1", true}, {"YES", true}, {"on", true},
		{"false", false}, {"0", false}, {"No", false}, {"off", false},
	}
	for _, c := range cases {
		config := DefaultConfig()
		if err := config.ApplyEnvOverridesFrom(testEnv{envSqlLog: c.raw}); err != nil {
			t.Fatal(err)
		}
		if config.Settings.SqlLog != c.expected {
			t.Errorf("raw=%s: 期望 %v", c.raw, c.expected)
		}
	}
}

func TestEnvInvalidBoolErrors(t *testing.T) {
	config := DefaultConfig()
	err := config.ApplyEnvOverridesFrom(testEnv{envSqlLog: "maybe"})
	if err == nil || !strings.Contains(err.Error(), envSqlLog) {
		t.Errorf("应报含变量名的错误: %v", err)
	}
}

func TestEnvInvalidNumberErrors(t *testing.T) {
	config := DefaultConfig()
	err := config.ApplyEnvOverridesFrom(testEnv{envPoolMax: "not-a-number"})
	if err == nil || !strings.Contains(err.Error(), envPoolMax) {
		t.Errorf("应报含变量名的错误: %v", err)
	}
}

func TestEnvTypeAliasesMerges(t *testing.T) {
	config, err := DefaultConfig().WithTypeAlias("old", "OLD").
		WithEnvOverridesFrom(testEnv{envTypeAliases: "new=NEW"})
	if err != nil {
		t.Fatal(err)
	}
	if config.TypeAliases["old"] != "OLD" || config.TypeAliases["new"] != "NEW" {
		t.Errorf("应合并: %v", config.TypeAliases)
	}
}

func TestEnvPathsTrimsAndFiltersEmpty(t *testing.T) {
	config := DefaultConfig()
	if err := config.ApplyEnvOverridesFrom(testEnv{envPaths: " a.xml , , b.xml ,"}); err != nil {
		t.Fatal(err)
	}
	if len(config.Settings.MapperPaths) != 2 ||
		config.Settings.MapperPaths[0] != "a.xml" || config.Settings.MapperPaths[1] != "b.xml" {
		t.Errorf("paths: %v", config.Settings.MapperPaths)
	}
}

func TestEnvInvalidAliasFormatErrors(t *testing.T) {
	config := DefaultConfig()
	if err := config.ApplyEnvOverridesFrom(testEnv{envTypeAliases: "noequals"}); err == nil {
		t.Error("无效别名格式应报错")
	}
}

// ─── mapper 文件发现 ──────────────────────────────────────────────

func TestDiscoverMapperFiles(t *testing.T) {
	temp := t.TempDir()
	mappersDir := filepath.Join(temp, "mappers")
	sub := filepath.Join(mappersDir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		filepath.Join(mappersDir, "a.xml"),
		filepath.Join(mappersDir, "b.xml"),
		filepath.Join(sub, "c.xml"),
	} {
		if err := os.WriteFile(f, []byte("<mapper/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(mappersDir, "d.txt"), []byte("not xml"), 0o644); err != nil {
		t.Fatal(err)
	}

	config := DefaultConfig().WithMapperPaths([]string{"mappers/**/*.xml"})
	files, err := config.DiscoverMapperFiles(temp)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range files {
		names[filepath.Base(f)] = true
	}
	if !names["a.xml"] || !names["b.xml"] || !names["c.xml"] {
		t.Errorf("files: %v", files)
	}
	if names["d.txt"] {
		t.Error("不应包含 d.txt")
	}
}
