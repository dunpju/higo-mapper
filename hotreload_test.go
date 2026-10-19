package higomapper

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ─── 热重载测试 ───────────────────────────────────────────────────

func TestIsXML(t *testing.T) {
	if !isXML("a.xml") || !isXML("a.XML") || !isXML(filepath.Join("d", "b.Xml")) {
		t.Error("xml 扩展名判定")
	}
	if isXML("a.txt") || isXML("noext") {
		t.Error("非 xml 扩展名")
	}
}

func TestExtractWatchDirsGlobRecursive(t *testing.T) {
	dirs := ExtractWatchDirs([]string{"mappers/**/*.xml"}, "/app")
	if len(dirs) != 1 || filepath.Clean(dirs[0]) != filepath.Clean("/app/mappers") {
		t.Errorf("dirs: %v", dirs)
	}
}

func TestExtractWatchDirsSpecificFile(t *testing.T) {
	// 文件模式 → 取父目录
	dirs := ExtractWatchDirs([]string{"mappers/User.xml"}, "/app")
	found := false
	for _, d := range dirs {
		if filepath.Clean(d) == filepath.Clean("/app/mappers") {
			found = true
		}
	}
	if !found {
		t.Errorf("dirs: %v", dirs)
	}
}

func TestExtractWatchDirsMultipleDedup(t *testing.T) {
	dirs := ExtractWatchDirs([]string{"mappers/**/*.xml", "mappers/Order.xml"}, "/app")
	if len(dirs) != 1 {
		t.Errorf("两者都解析到 /app/mappers，应去重为 1: %v", dirs)
	}
}

func TestExtractWatchDirsWildcardStart(t *testing.T) {
	// 模式以通配符开头 → 监视 baseDir
	dirs := ExtractWatchDirs([]string{"**/*.xml"}, "/app")
	if len(dirs) != 1 || filepath.Clean(dirs[0]) != filepath.Clean("/app") {
		t.Errorf("dirs: %v", dirs)
	}
}

// e2e：修改 XML 文件 → 去抖后重解析 → 查询行为变化
func TestHotReloadReregistersOnChange(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过热重载 e2e")
	}
	baseDir := t.TempDir()
	mappersDir := filepath.Join(baseDir, "mappers")
	if err := os.MkdirAll(mappersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	xmlPath := filepath.Join(mappersDir, "HotMapper.xml")
	v1 := `<mapper namespace="com.test.HotMapper">
    <select id="pick">SELECT 1 AS v</select>
</mapper>`
	if err := os.WriteFile(xmlPath, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig().
		WithEnvironment(EnvironmentConfig{
			Driver: "sqlite", URL: "sqlite::memory:",
			PoolMaxConnections: 1, PoolMinConnections: 1,
		}).
		WithMapperPaths([]string{"mappers/**/*.xml"}).
		WithHotReload(100)
	factory, err := NewSqlSessionFactory(context.Background(), cfg, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	defer factory.Close()
	if !factory.HotReloadEnabled() {
		t.Fatal("热重载应已启用")
	}

	session := factory.OpenSession()
	defer session.Close()

	row := map[string]any{}
	query := func() (map[string]any, error) {
		rows, err := SelectList[map[string]any](context.Background(), session, "com.test.HotMapper", "pick", nil)
		if err != nil {
			return nil, err
		}
		return rows[0], nil
	}
	if row, err = query(); err != nil {
		t.Fatal(err)
	}
	// map 结果经 JSON 往返，数字统一为 float64
	if row["v"] != float64(1) {
		t.Fatalf("初版应返回 1: %v", row)
	}

	// 修改语句：SELECT 2
	v2 := `<mapper namespace="com.test.HotMapper">
    <select id="pick">SELECT 2 AS v</select>
</mapper>`
	if err := os.WriteFile(xmlPath, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}

	// 等待去抖安静期（100ms）+ 重解析 + fsnotify 延迟（Windows 上偶有滞后）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		row, err = query()
		if err == nil && row["v"] == float64(2) {
			return // 热重载生效
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("热重载未生效，最后结果: %v, err: %v", row, err)
}
