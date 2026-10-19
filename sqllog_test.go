package higomapper

import (
	"strings"
	"testing"

	"github.com/dunpju/higo-mapper/core"
)

// ─── SQL 日志渲染测试 ─────────────────────────────────────────────

func bound(sql string, params ...any) *core.BoundSql {
	return &core.BoundSql{SQL: sql, Parameters: params}
}

func TestRenderInlinesParams(t *testing.T) {
	b := bound("SELECT id, name FROM users WHERE id = ? AND name = ? AND active = ?",
		int64(42), "张三", true)
	rendered := RenderSQLForLog(b)
	want := "SELECT id, name FROM users WHERE id = 42 AND name = '张三' AND active = 1"
	if rendered != want {
		t.Errorf("rendered: %q", rendered)
	}
}

func TestRenderNullAndFloat(t *testing.T) {
	b := bound("INSERT INTO t (a, b) VALUES (?, ?)", nil, 3.5)
	if got := RenderSQLForLog(b); got != "INSERT INTO t (a, b) VALUES (NULL, 3.5)" {
		t.Errorf("rendered: %q", got)
	}
}

func TestRenderEscapesQuote(t *testing.T) {
	b := bound("WHERE n = ?", "it's")
	if got := RenderSQLForLog(b); got != "WHERE n = 'it''s'" {
		t.Errorf("rendered: %q", got)
	}
}

func TestRenderKeepsPlaceholderWhenParamsShort(t *testing.T) {
	b := bound("VALUES (?, ?, ?)", int64(1))
	if got := RenderSQLForLog(b); got != "VALUES (1, ?, ?)" {
		t.Errorf("rendered: %q", got)
	}
}

func TestRenderCollapsesNewlines(t *testing.T) {
	b := bound("SELECT id\nFROM users\nWHERE id = ?", int64(7))
	if got := RenderSQLForLog(b); got != "SELECT id FROM users WHERE id = 7" {
		t.Errorf("rendered: %q", got)
	}
}

func TestRenderCollapsesConsecutiveNewlines(t *testing.T) {
	b := bound("SELECT id\n\n\n\nFROM users")
	if got := RenderSQLForLog(b); got != "SELECT id FROM users" {
		t.Errorf("rendered: %q", got)
	}
}

func TestRenderCollapsesCRLF(t *testing.T) {
	b := bound("SELECT a\r\n\r\nFROM t\rWHERE x = ?", int64(1))
	if got := RenderSQLForLog(b); got != "SELECT a FROM t WHERE x = 1" {
		t.Errorf("rendered: %q", got)
	}
}

func TestRenderCollapsesMultilineXMLStyle(t *testing.T) {
	b := bound("SELECT id, name FROM users\n  WHERE status = ?\n  AND id IN (?, ?)",
		int64(1), int64(2), int64(3))
	want := "SELECT id, name FROM users WHERE status = 1 AND id IN (2, 3)"
	if got := RenderSQLForLog(b); got != want {
		t.Errorf("rendered: %q", got)
	}
}

func TestRenderCollapsesConsecutiveSpaces(t *testing.T) {
	b := bound("SELECT   id,\n                name\n         FROM   users")
	if got := RenderSQLForLog(b); got != "SELECT id, name FROM users" {
		t.Errorf("rendered: %q", got)
	}
}

func TestRenderCollapsesNewlineInParamValue(t *testing.T) {
	// 参数值内含换行同样折叠，保证日志单行
	b := bound("INSERT INTO t (v) VALUES (?)", "a\nb")
	if got := RenderSQLForLog(b); got != "INSERT INTO t (v) VALUES ('a b')" {
		t.Errorf("rendered: %q", got)
	}
}

func TestCollapseLogWhitespaceFastPathAndRuns(t *testing.T) {
	// 无需折叠时原样返回（仅单空格、无换行/制表）
	if got := collapseLogWhitespace("SELECT id FROM t WHERE a = 1"); got != "SELECT id FROM t WHERE a = 1" {
		t.Errorf("fast path: %q", got)
	}
	if got := collapseLogWhitespace("a   b"); got != "a b" {
		t.Errorf("连续空格: %q", got)
	}
	if got := collapseLogWhitespace("a \n\t b"); got != "a b" {
		t.Errorf("混合: %q", got)
	}
	if got := collapseLogWhitespace("a\tb"); got != "a b" {
		t.Errorf("制表: %q", got)
	}
	// 首尾空白折叠为单个空格（保留，不去除）
	if got := collapseLogWhitespace("\n  SELECT 1\n"); got != " SELECT 1 " {
		t.Errorf("首尾: %q", got)
	}
	if got := collapseLogWhitespace("a\rb"); got != "a b" {
		t.Errorf("CR: %q", got)
	}
}

func TestRenderExecuteLineColored(t *testing.T) {
	line := renderExecuteLine("SELECT 1", true)
	if !strings.HasPrefix(line, "\x1b[38;5;166m") || !strings.HasSuffix(line, "\x1b[0m") {
		t.Errorf("colored: %q", line)
	}
	if !strings.Contains(line, "Execute SQL: SELECT 1") {
		t.Errorf("colored content: %q", line)
	}
}

func TestRenderExecuteLinePlain(t *testing.T) {
	line := renderExecuteLine("SELECT 1", false)
	if line != "Execute SQL: SELECT 1" {
		t.Errorf("plain: %q", line)
	}
	if strings.Contains(line, "\x1b") {
		t.Errorf("不应含 ANSI 转义: %q", line)
	}
}
