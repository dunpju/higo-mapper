package higomapper

import (
	"context"
	"strings"
	"testing"

	"github.com/dunpju/higo-mapper/core"
)

// ─── DSN 转换单测 ─────────────────────────────────────────────────

func TestResolveDSNMySql(t *testing.T) {
	dsn, err := resolveDSN("mysql", "mysql://user:pass@localhost:3306/mydb?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	if dsn != "user:pass@tcp(localhost:3306)/mydb?parseTime=true" {
		t.Errorf("dsn: %s", dsn)
	}
}

func TestResolveDSNMySqlNoPortNoQuery(t *testing.T) {
	dsn, err := resolveDSN("mysql", "mysql://root@localhost/db")
	if err != nil {
		t.Fatal(err)
	}
	if dsn != "root@tcp(localhost)/db" {
		t.Errorf("dsn: %s", dsn)
	}
}

func TestResolveDSNPostgresPassthrough(t *testing.T) {
	url := "postgres://user:pass@localhost:5432/mydb?sslmode=disable"
	dsn, err := resolveDSN("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	if dsn != url {
		t.Errorf("dsn: %s", dsn)
	}
}

func TestResolveDSNSqliteMemory(t *testing.T) {
	dsn, err := resolveDSN("sqlite", "sqlite::memory:")
	if err != nil {
		t.Fatal(err)
	}
	if dsn != "file::memory:?cache=shared" {
		t.Errorf("dsn: %s", dsn)
	}
}

func TestResolveDSNSqliteFile(t *testing.T) {
	dsn, err := resolveDSN("sqlite", "sqlite:///data/app.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if dsn != "file:/data/app.sqlite" {
		t.Errorf("dsn: %s", dsn)
	}
}

func TestResolveDSNRawDSN(t *testing.T) {
	// 不成形 URL：按原始 DSN 处理
	raw := "user:pass@tcp(localhost:3306)/db"
	dsn, err := resolveDSN("mysql", raw)
	if err != nil {
		t.Fatal(err)
	}
	if dsn != raw {
		t.Errorf("dsn: %s", dsn)
	}
}

// ─── 环境构建校验 ─────────────────────────────────────────────────

func TestEnvironmentInvalidDriver(t *testing.T) {
	_, err := NewEnvironment(context.Background(), EnvironmentConfig{
		Driver: "oracle", URL: "oracle://localhost/db",
		PoolMaxConnections: 5, PoolMinConnections: 1,
	})
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "不支持的数据库驱动") || !strings.Contains(msg, "oracle") {
		t.Errorf("err: %s", msg)
	}
}

func TestEnvironmentEmptyURL(t *testing.T) {
	_, err := NewEnvironment(context.Background(), EnvironmentConfig{
		Driver: "sqlite", PoolMaxConnections: 5, PoolMinConnections: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "URL 不能为空") {
		t.Errorf("err: %v", err)
	}
}

// ─── 注册表测试 ───────────────────────────────────────────────────

func TestRegistryRegisterAndGet(t *testing.T) {
	reg := NewMapperRegistry()
	ns, err := reg.RegisterFromXML(`<mapper namespace="com.test.UserMapper">
        <select id="findById">SELECT * FROM users WHERE id = #{id}</select>
    </mapper>`)
	if err != nil {
		t.Fatal(err)
	}
	if ns != "com.test.UserMapper" {
		t.Errorf("ns: %s", ns)
	}
	if reg.Len() != 1 {
		t.Errorf("len: %d", reg.Len())
	}
	mapper := reg.GetMapper("com.test.UserMapper")
	if mapper == nil {
		t.Fatal("mapper 不应为 nil")
	}
	if _, ok := mapper.Statements["findById"]; !ok {
		t.Error("缺少 findById")
	}
	if reg.GetMapper("not.exist") != nil {
		t.Error("不存在的 namespace 应返回 nil")
	}
}

func TestTypeAliasResolve(t *testing.T) {
	reg := NewTypeAliasRegistry()
	reg.Register("int", "i32")
	if reg.Resolve("int") != "i32" {
		t.Error("别名解析")
	}
	if reg.Resolve("unknown") != "unknown" {
		t.Error("未注册别名应原样返回")
	}
}

// ─── 类型处理器测试 ───────────────────────────────────────────────

func TestTypeHandlerRegistryDefaults(t *testing.T) {
	r := NewTypeHandlerRegistry()
	for _, name := range []string{"i64", "i32", "f64", "bool", "string", "time"} {
		if _, ok := r.Lookup(name); !ok {
			t.Errorf("缺少标准处理器 %s", name)
		}
	}
	// 别名
	for _, alias := range []string{"long", "bigint", "int", "String", "DateTime"} {
		if _, ok := r.Lookup(alias); !ok {
			t.Errorf("缺少别名 %s", alias)
		}
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Error("未知类型不应命中")
	}
}

func TestInt64HandlerRoundtrip(t *testing.T) {
	h := Int64Handler{}
	if v, _ := h.GetResult(int64(42)); v != int64(42) {
		t.Errorf("GetResult: %v", v)
	}
	if v, _ := h.GetResult("123"); v != int64(123) {
		t.Errorf("string GetResult: %v", v)
	}
	if v, _ := h.GetResult(nil); v != nil {
		t.Errorf("nil GetResult: %v", v)
	}
	if v, _ := h.SetParameter(float64(7)); v != int64(7) {
		t.Errorf("SetParameter: %v", v)
	}
	if _, err := h.SetParameter("str"); err == nil {
		t.Error("非数字应报错")
	}
}

func TestBoolHandlerRoundtrip(t *testing.T) {
	h := BoolHandler{}
	if v, _ := h.GetResult(int64(1)); v != true {
		t.Errorf("1 → true: %v", v)
	}
	if v, _ := h.GetResult(int64(0)); v != false {
		t.Errorf("0 → false: %v", v)
	}
	if v, _ := h.SetParameter(int64(2)); v != true {
		t.Errorf("2 → true: %v", v)
	}
}

func TestStringHandlerRoundtrip(t *testing.T) {
	h := StringHandler{}
	if v, _ := h.GetResult(int64(5)); v != "5" {
		t.Errorf("int → string: %v", v)
	}
	if v, _ := h.GetResult([]byte("blob")); v != "blob" {
		t.Errorf("blob → string: %v", v)
	}
}

// ─── 参数处理测试 ─────────────────────────────────────────────────

func TestNormalizeValue(t *testing.T) {
	if v := NormalizeValue(42); v != int64(42) {
		t.Errorf("int: %v", v)
	}
	if v := NormalizeValue(float32(1.5)); v != float64(1.5) {
		t.Errorf("float32: %v", v)
	}
	if v := NormalizeValue(nil); v != nil {
		t.Errorf("nil: %v", v)
	}
	// 数组
	if v := NormalizeValue([]int{1, 2}); len(v.([]any)) != 2 {
		t.Errorf("[]int: %v", v)
	}
}

func TestBindValue(t *testing.T) {
	if v, _ := BindValue(nil); v != nil {
		t.Errorf("nil: %v", v)
	}
	if v, _ := BindValue(true); v != true {
		t.Errorf("bool: %v", v)
	}
	// 数组 → JSON 字符串
	v, err := BindValue([]any{int64(1), int64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if v != "[1,2]" {
		t.Errorf("array json: %v", v)
	}
}

func TestValidatePlaceholderCount(t *testing.T) {
	b := &core.BoundSql{SQL: "SELECT ?, ?, ?", Parameters: []any{nil, nil, nil}}
	if err := ValidatePlaceholderCount(b); err != nil {
		t.Errorf("一致时不应报错: %v", err)
	}
	b = &core.BoundSql{SQL: "SELECT ?, ?", Parameters: []any{nil}}
	err := ValidatePlaceholderCount(b)
	if err == nil || !strings.Contains(err.Error(), "参数数量不匹配") {
		t.Errorf("err: %v", err)
	}
}

func TestBindArguments(t *testing.T) {
	b := &core.BoundSql{
		SQL:        "INSERT INTO t (a,b,c) VALUES (?,?,?)",
		Parameters: []any{int64(1), "x", nil},
	}
	args, err := BindArguments(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 3 || args[0] != int64(1) || args[1] != "x" || args[2] != nil {
		t.Errorf("args: %v", args)
	}
}
