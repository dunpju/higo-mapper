package higomapper

import (
	"strings"
	"testing"
)

func TestQueryBuilderBasicSelect(t *testing.T) {
	bound, err := NewQueryBuilder().
		Table("users").
		Field("id").
		Field("name").
		Field("u.age").
		WhereEq("status", 1).
		WhereLike("name", "%张%").
		OrderDesc("id").
		Limit(10).
		Offset(20).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "SELECT id, name, u.age FROM users WHERE status = ? AND name LIKE ? ORDER BY id DESC LIMIT ? OFFSET ?"
	if bound.SQL != want {
		t.Fatalf("SQL 不匹配:\n got: %s\nwant: %s", bound.SQL, want)
	}
	if len(bound.Parameters) != 4 {
		t.Fatalf("参数数量 = %d, want 4: %v", len(bound.Parameters), bound.Parameters)
	}
	if bound.Parameters[0] != int64(1) { // int 经规范化变 int64
		t.Fatalf("参数[0] = %T(%v), want int64(1)", bound.Parameters[0], bound.Parameters[0])
	}
	if bound.Parameters[1] != "%张%" || bound.Parameters[2] != int64(10) || bound.Parameters[3] != int64(20) {
		t.Fatalf("参数不匹配: %v", bound.Parameters)
	}
}

func TestQueryBuilderNoFieldsSelectsStar(t *testing.T) {
	bound, err := NewQueryBuilder().Table("t").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if bound.SQL != "SELECT * FROM t" {
		t.Fatalf("SQL = %q", bound.SQL)
	}
	if len(bound.Parameters) != 0 {
		t.Fatalf("参数应为空: %v", bound.Parameters)
	}
}

func TestQueryBuilderAllCondOps(t *testing.T) {
	bound, err := NewQueryBuilder().
		Table("t").
		WhereNe("a", 1).
		WhereGt("b", 2).
		WhereLt("c", 3).
		WhereGe("d", 4).
		WhereLe("e", 5).
		WhereNotLike("f", "%x%").
		WhereBetween("g", 1, 9).
		WhereNull("h").
		WhereNotNull("i").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "SELECT * FROM t WHERE a <> ? AND b > ? AND c < ? AND d >= ? AND e <= ? AND f NOT LIKE ? AND g BETWEEN ? AND ? AND h IS NULL AND i IS NOT NULL"
	if bound.SQL != want {
		t.Fatalf("SQL 不匹配:\n got: %s\nwant: %s", bound.SQL, want)
	}
	if len(bound.Parameters) != 8 {
		t.Fatalf("参数数量 = %d, want 8", len(bound.Parameters))
	}
}

func TestQueryBuilderInSemantics(t *testing.T) {
	// 非空 IN
	bound, err := NewQueryBuilder().Table("t").WhereIn("id", 1, 2, 3).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(bound.SQL, "id IN (?, ?, ?)") {
		t.Fatalf("IN SQL = %q", bound.SQL)
	}
	if len(bound.Parameters) != 3 {
		t.Fatalf("参数数量 = %d", len(bound.Parameters))
	}

	// 空集 NOT IN → 1 = 1
	bound, err = NewQueryBuilder().Table("t").WhereNotIn("id").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(bound.SQL, "1 = 1") || strings.Contains(bound.SQL, "NOT IN") {
		t.Fatalf("空 NOT IN SQL = %q", bound.SQL)
	}
	if len(bound.Parameters) != 0 {
		t.Fatalf("空 NOT IN 不应有参数: %v", bound.Parameters)
	}
}

func TestQueryBuilderWhereRaw(t *testing.T) {
	bound, err := NewQueryBuilder().
		Table("t").
		WhereEq("a", 1).
		WhereRaw("(b = ? OR c = ?)", 2, 3).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "SELECT * FROM t WHERE a = ? AND (b = ? OR c = ?)"
	if bound.SQL != want {
		t.Fatalf("SQL 不匹配:\n got: %s\nwant: %s", bound.SQL, want)
	}
	if len(bound.Parameters) != 3 {
		t.Fatalf("参数数量 = %d", len(bound.Parameters))
	}
}

func TestQueryBuilderBuildCountIgnoresFieldsOrderLimit(t *testing.T) {
	bound, err := NewQueryBuilder().
		Table("users").
		Field("id").
		WhereEq("status", 1).
		OrderDesc("id").
		Limit(5).
		BuildCount()
	if err != nil {
		t.Fatalf("BuildCount: %v", err)
	}
	if bound.SQL != "SELECT COUNT(*) FROM users WHERE status = ?" {
		t.Fatalf("count SQL = %q", bound.SQL)
	}
	if len(bound.Parameters) != 1 {
		t.Fatalf("参数数量 = %d", len(bound.Parameters))
	}
}

func TestQueryBuilderBuildPage(t *testing.T) {
	page := NewPageRequest(3, 10)
	bound, err := NewQueryBuilder().
		Table("users").
		WhereEq("status", 1).
		OrderDesc("id").
		BuildPage(page)
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	if !strings.HasSuffix(bound.SQL, "ORDER BY id DESC LIMIT ? OFFSET ?") {
		t.Fatalf("page SQL = %q", bound.SQL)
	}
	n := len(bound.Parameters)
	if n != 3 || bound.Parameters[n-2] != int64(10) || bound.Parameters[n-1] != int64(20) {
		t.Fatalf("分页参数不匹配: %v", bound.Parameters)
	}

	// 手动 limit/offset 与分页互斥
	_, err = NewQueryBuilder().Table("t").Limit(5).BuildPage(NewPageRequest(1, 10))
	if err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("应报互斥错误, got %v", err)
	}
	_, err = NewQueryBuilder().Table("t").Offset(5).BuildPage(NewPageRequest(1, 10))
	if err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("offset 也应互斥, got %v", err)
	}
}

func TestQueryBuilderIdentifierWhitelist(t *testing.T) {
	valid := []string{"users", "_t", "t1", "t.col", "a.b.c", "create_time0", "X_9"}
	for _, id := range valid {
		if err := validateIdent(id); err != nil {
			t.Errorf("标识符 %q 应合法: %v", id, err)
		}
	}
	invalid := []string{
		"", "1col", "col-name", "col name", "col;DROP TABLE users",
		"`col`", "\"col\"", "col--x", "t.", ".t", "t..c", "col)",
		"col,name", "col)", "user\x00",
	}
	for _, id := range invalid {
		if err := validateIdent(id); err == nil {
			t.Errorf("标识符 %q 应被拒绝", id)
		}
	}

	// 非法标识符在各入口均触发延迟错误
	for _, b := range []*QueryBuilder{
		NewQueryBuilder().Table("users; DROP TABLE users"),
		NewQueryBuilder().Table("t").Field("a; b"),
		NewQueryBuilder().Table("t").WhereEq("a = 1 OR 1", 1),
		NewQueryBuilder().Table("t").WhereIn("a;b", 1),
		NewQueryBuilder().Table("t").WhereBetween("a;b", 1, 2),
		NewQueryBuilder().Table("t").WhereNull("a;b"),
		NewQueryBuilder().Table("t").OrderDesc("a;b"),
	} {
		if _, err := b.Build(); err == nil {
			t.Errorf("非法标识符应延迟报错, SQL 应未构建")
		} else if kind := err.(*Error).Kind; kind != ErrConfig {
			t.Errorf("错误类别 = %v, want ErrConfig", kind)
		}
	}
}

func TestQueryBuilderMissingTable(t *testing.T) {
	if _, err := NewQueryBuilder().WhereEq("a", 1).Build(); err == nil {
		t.Fatal("缺表名应报错")
	}
	if _, err := NewQueryBuilder().BuildCount(); err == nil {
		t.Fatal("缺表名 BuildCount 应报错")
	}
}

func TestQueryBuilderMultiOrderAndNullParams(t *testing.T) {
	bound, err := NewQueryBuilder().
		Table("t").
		WhereEq("name", nil).
		OrderAsc("a").
		OrderDesc("b").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(bound.SQL, "name = ?") || bound.Parameters[0] != nil {
		t.Fatalf("null 条件异常: %q %v", bound.SQL, bound.Parameters)
	}
	if !strings.Contains(bound.SQL, "ORDER BY a ASC, b DESC") {
		t.Fatalf("多列排序 = %q", bound.SQL)
	}
}

func TestQueryBuilderValueNormalization(t *testing.T) {
	// int / float32 / []byte / struct 均规范化为通用类型
	type user struct {
		ID int    `json:"id"`
		N  string `json:"n"`
	}
	bound, err := NewQueryBuilder().
		Table("t").
		WhereEq("age", 18).             // int → int64
		WhereEq("score", 99.5).         // float64 直通
		WhereEq("tag", []byte("x")).    // []byte → string
		WhereEq("extra", user{1, "a"}). // struct → map
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p := bound.Parameters
	if p[0] != int64(18) {
		t.Fatalf("int 未规范化: %T", p[0])
	}
	if p[1] != 99.5 {
		t.Fatalf("float 丢失: %T", p[1])
	}
	if p[2] != "x" {
		t.Fatalf("[]byte 未转 string: %T", p[2])
	}
	m, ok := p[3].(map[string]any)
	if !ok || m["id"] != int64(1) || m["n"] != "a" {
		t.Fatalf("struct 未转 map: %T %v", p[3], p[3])
	}
}
