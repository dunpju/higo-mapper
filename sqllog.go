package higomapper

// SQL 执行日志。
//
// 受配置 [settings] sql_log 开关控制。启用后，在每次 SQL 执行点记录
// 「耗时 + 可读 SQL（参数内联进 ?）」一条日志，经包级 Logger 输出。
//
// # 输出示例
//
//	[时间] Consume Time: 44 ms
//	 Execute SQL: SELECT `examId`, `examName` FROM exam WHERE (`examId` IN (69902) AND `isDelete` = 0)
//
// 「Execute SQL」行在输出到终端（stdout/stderr 任一为字符终端）时以红褐色
// ANSI 颜色渲染，便于在日志流中快速定位；重定向到文件等非终端场景自动去掉
// 颜色码，避免转义序列污染日志文件。
//
// # 消费方
//
// 本包默认不输出任何日志（Logger 为 nil）。要看到输出，应用可设置：
//
//	SetLogger(log.New(os.Stdout, "", log.LstdFlags))

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/dunpju/higo-mapper/core"
)

// Logger 包级日志输出器；nil 表示不输出。应用可随时替换（如接 slog 适配器）。
var Logger *log.Logger

// SetLogger 设置 SQL 日志输出器（nil 关闭输出）
func SetLogger(l *log.Logger) { Logger = l }

// SQLColor SQL 行前景色：红褐色（ANSI 256 色 #D75F00）
const sqlColor = "\x1b[38;5;166m"

// colorReset ANSI 颜色复位码
const colorReset = "\x1b[0m"

// SqlLogConfig SQL 日志配置（从 [settings] 解析）
type SqlLogConfig struct {
	// Enabled 是否开启 SQL 执行日志
	Enabled bool
	// SlowThresholdMs 慢查询阈值（毫秒）；仅记录耗时 ≥ 此值的 SQL。0 表示记录全部。
	SlowThresholdMs uint64
}

// ShouldLog 该次执行是否应被记录（开关开启 + 达到慢查询阈值）
func (c SqlLogConfig) ShouldLog(elapsed time.Duration) bool {
	if !c.Enabled {
		return false
	}
	if c.SlowThresholdMs == 0 {
		return true
	}
	return uint64(elapsed.Milliseconds()) >= c.SlowThresholdMs
}

// RenderSQLForLog 把 BoundSql 的参数内联进 ? 占位符，生成可读的日志 SQL。
//
// 字符串值加单引号并转义内嵌引号；数字/布尔/NULL 原样输出；参数少于占位符时保留 ?。
// XML 中多行/缩进书写的 SQL，其连续空白（换行/制表/空格，单个或连续多个）
// 会被折叠为单个空格，保证日志单行且紧凑。
//
// 注意：按字符扫描 ? 做替换，若 SQL 文本中存在字面 ?（如字符串字面量内）会被误替换，
// 仅供日志可读性，不影响实际执行。
func RenderSQLForLog(bound *core.BoundSql) string {
	sql := bound.SQL
	var out strings.Builder
	out.Grow(len(sql) + len(bound.Parameters)*4)
	paramIdx := 0
	for _, ch := range sql {
		if ch != '?' {
			out.WriteRune(ch)
			continue
		}
		if paramIdx < len(bound.Parameters) {
			out.WriteString(logInline(bound.Parameters[paramIdx]))
			paramIdx++
		} else {
			out.WriteByte('?')
		}
	}
	return collapseLogWhitespace(out.String())
}

// logInline 单个参数值的日志内联形式
func logInline(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if t {
			return "1"
		}
		return "0"
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

// collapseLogWhitespace 将连续空白字符（换行/\r/制表/空格，单个或连续多个）
// 折叠为单个空格。也覆盖内联参数值中含空白的情况，确保整条日志始终单行。
func collapseLogWhitespace(s string) string {
	needs := strings.ContainsAny(s, "\n\r\t") || strings.Contains(s, "  ")
	if !needs {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	inRun := false
	for _, ch := range s {
		if isSpaceRune(ch) {
			if !inRun {
				out.WriteByte(' ')
				inRun = true
			}
		} else {
			out.WriteRune(ch)
			inRun = false
		}
	}
	return out.String()
}

// isSpaceRune 空白判定（与 Rust char::is_whitespace 对齐：Unicode 空白类）
func isSpaceRune(ch rune) bool {
	switch ch {
	case ' ', '\t', '\n', '\r', '\v', '\f', 0x85, 0xA0:
		return true
	}
	return unicode.IsSpace(ch)
}

// isCharDevice 文件是否为字符终端（TTY）
func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// LogExecution 若配置启用且达到阈值，记录一条 SQL 执行日志（耗时 + 可读 SQL）。
//
// 成功与失败路径均会记录（耗时本身有诊断价值）。
// 输出到终端时「Execute SQL」行以红褐色渲染。
func LogExecution(config SqlLogConfig, bound *core.BoundSql, elapsed time.Duration) {
	if !config.ShouldLog(elapsed) || Logger == nil {
		return
	}
	sql := RenderSQLForLog(bound)
	executeLine := renderExecuteLine(sql, outputSupportsColor())
	Logger.Printf("Consume Time: %d ms\n %s", elapsed.Milliseconds(), executeLine)
}

// renderExecuteLine 渲染日志第二行「Execute SQL: ...」；
// colorize 为真时整体加红褐色 ANSI 颜色。
func renderExecuteLine(sql string, colorize bool) string {
	if colorize {
		return sqlColor + "Execute SQL: " + sql + colorReset
	}
	return "Execute SQL: " + sql
}

// outputSupportsColor 输出是否为终端：日志可能写 stdout 或 stderr，
// 任一为字符终端即视为需要颜色。重定向到文件/管道时不附加颜色码。
func outputSupportsColor() bool {
	return isCharDevice(os.Stdout) || isCharDevice(os.Stderr)
}
