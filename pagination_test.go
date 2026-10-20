package higomapper

import (
	"strings"
	"testing"

	"github.com/dunpju/higo-mapper/core"
)

func newBound(t *testing.T, sql string, params ...any) *core.BoundSql {
	t.Helper()
	return &core.BoundSql{SQL: sql, Parameters: params}
}

func TestPageRequestValidatedOffset(t *testing.T) {
	cases := []struct {
		req      PageRequest
		want     uint64
		wantFail bool
	}{
		{PageRequest{1, 10}, 0, false},
		{PageRequest{2, 10}, 10, false},
		{PageRequest{3, 25}, 50, false},
		{PageRequest{0, 10}, 0, true},
		{PageRequest{1, 0}, 0, true},
		{PageRequest{2, 0}, 0, true},
		// (pageNo-1)*pageSize 溢出 uint64
		{PageRequest{^uint64(0), ^uint64(0)}, 0, true},
	}
	for _, c := range cases {
		off, err := c.req.ValidatedOffset()
		if c.wantFail {
			if err == nil {
				t.Errorf("%v 应报错", c.req)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", c.req, err)
			continue
		}
		if off != c.want {
			t.Errorf("%v offset = %d, want %d", c.req, off, c.want)
		}
	}
}

func TestPageHelpers(t *testing.T) {
	p := NewPage([]int{1, 2}, 25, 3, 10)
	if p.TotalPages() != 3 {
		t.Fatalf("TotalPages = %d, want 3", p.TotalPages())
	}
	if p.HasNext() || !p.HasPrev() {
		t.Fatal("第 3/3 页：HasNext 应为 false，HasPrev 应为 true")
	}
	if p.IsEmpty() {
		t.Fatal("记录非空")
	}

	// total=11 → 2 页
	p2 := NewPage([]int{1}, 11, 1, 10)
	if p2.TotalPages() != 2 || !p2.HasNext() || p2.HasPrev() {
		t.Fatalf("11 条/页 10: totalPages=%d hasNext=%v hasPrev=%v", p2.TotalPages(), p2.HasNext(), p2.HasPrev())
	}

	// 空页
	e := EmptyPage[string](PageRequest{PageNo: 7, PageSize: 10})
	if e.Total != 0 || len(e.Records) != 0 || e.PageNo != 7 || e.TotalPages() != 0 {
		t.Fatalf("EmptyPage 异常: %+v", e)
	}

	// MapPage 保留元信息
	m := MapPage(p, func(v int) string { return "x" })
	if m.Total != 25 || m.PageNo != 3 || m.PageSize != 10 || len(m.Records) != 2 {
		t.Fatalf("MapPage 元信息丢失: %+v", m)
	}
}

func TestAppendLimitOffset(t *testing.T) {
	b := newBound(t, "SELECT * FROM t WHERE a = ?", int64(1))
	appendLimitOffset(b, 10, 20)
	if b.SQL != "SELECT * FROM t WHERE a = ? LIMIT ? OFFSET ?" {
		t.Fatalf("SQL = %q", b.SQL)
	}
	if len(b.Parameters) != 3 || b.Parameters[1] != int64(10) || b.Parameters[2] != int64(20) {
		t.Fatalf("参数 = %v", b.Parameters)
	}

	// 尾部空白与分号
	b = newBound(t, "SELECT 1  \n\t; ")
	appendLimitOffset(b, 1, 0)
	if !strings.HasSuffix(b.SQL, "SELECT 1 LIMIT ? OFFSET ?") {
		t.Fatalf("清理后 SQL = %q", b.SQL)
	}
}

func TestBuildCountSQL(t *testing.T) {
	b := newBound(t, "SELECT a FROM t WHERE x = ? ORDER BY a DESC", int64(1))
	c := buildCountSQL(b)
	if c.SQL != "SELECT COUNT(*) FROM (SELECT a FROM t WHERE x = ?) AS __higo_count_t" {
		t.Fatalf("count SQL = %q", c.SQL)
	}
	if len(c.Parameters) != 1 || c.Parameters[0] != int64(1) {
		t.Fatalf("参数 = %v", c.Parameters)
	}
}

func TestStripTopLevelOrderBy(t *testing.T) {
	cases := []struct{ in, want string }{
		// 无 ORDER BY 原样返回
		{"SELECT a FROM t", "SELECT a FROM t"},
		// 简单剥离
		{"SELECT a FROM t ORDER BY a", "SELECT a FROM t"},
		{"SELECT a FROM t ORDER BY a DESC, b", "SELECT a FROM t"},
		// 大小写不敏感
		{"SELECT a FROM t order by a", "SELECT a FROM t"},
		// 子查询内不剥
		{"SELECT * FROM (SELECT a FROM t ORDER BY a) x ORDER BY b", "SELECT * FROM (SELECT a FROM t ORDER BY a) x"},
		// 字符串字面量内的 ORDER 不是关键字
		{"SELECT 'order by x' FROM t", "SELECT 'order by x' FROM t"},
		{"SELECT 'it''s order by' FROM t", "SELECT 'it''s order by' FROM t"},
		// 引号标识符内的 ORDER 不是关键字
		{"SELECT \"order\" FROM t", "SELECT \"order\" FROM t"},
		{"SELECT `order` FROM t", "SELECT `order` FROM t"},
		// 非完整单词不匹配（ORDERED）
		{"SELECT ordered FROM t", "SELECT ordered FROM t"},
		// ORDER 后无 BY 不剥
		{"SELECT a FROM t ORDER", "SELECT a FROM t ORDER"},
		// UNION 尾部：剥最右一个
		{"SELECT a FROM t1 UNION SELECT a FROM t2 ORDER BY a", "SELECT a FROM t1 UNION SELECT a FROM t2"},
		// 函数调用内的不剥
		{"SELECT listagg(x ORDER BY y) FROM t", "SELECT listagg(x ORDER BY y) FROM t"},
	}
	for _, c := range cases {
		if got := stripTopLevelOrderBy(c.in); got != c.want {
			t.Errorf("strip(%q)\n got: %q\nwant: %q", c.in, got, c.want)
		}
	}
}

func TestEnsureNoTopLevelLimit(t *testing.T) {
	ok := []string{
		"SELECT * FROM t",
		"SELECT * FROM (SELECT * FROM t LIMIT 5) x",
		"SELECT 'limit 1' FROM t",
		"SELECT * FROM t WHERE a = 'x offset y'",
		"SELECT `limit` FROM t",
	}
	for _, sql := range ok {
		if err := ensureNoTopLevelLimit(sql); err != nil {
			t.Errorf("%q 不应报错: %v", sql, err)
		}
	}
	bad := []string{
		"SELECT * FROM t LIMIT 10",
		"select * from t limit 10",
		"SELECT * FROM t OFFSET 5",
		"SELECT * FROM t ORDER BY a LIMIT 1, 5",
	}
	for _, sql := range bad {
		if err := ensureNoTopLevelLimit(sql); err == nil {
			t.Errorf("%q 应报错", sql)
		}
	}
}

func TestTruncateForError(t *testing.T) {
	short := "SELECT 1"
	if truncateForError(short) != short {
		t.Fatal("短 SQL 不应截断")
	}
	long := strings.Repeat("中", 200) // 600 字节, 200 字符
	got := truncateForError(long)
	if !strings.HasSuffix(got, "...") {
		t.Fatal("应加省略号")
	}
	if n := len([]rune(got)); n != 123 {
		t.Fatalf("截断长度 = %d, want 123", n)
	}
}
