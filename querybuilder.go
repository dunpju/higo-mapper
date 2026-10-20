package higomapper

// QueryBuilder：程序化链式查询构造器（ThinkPHP 风格）。
//
// 为简单查询提供免 XML 的编程式 API，产物为 BoundSql，与 XML mapper 语句
// 走同一执行层（session 的 SelectBound / CountBound）——事务路由、SQL 日志、
// Before/After 事件全部免费复用。
//
// # 注入安全
//
//   - 值一律参数化（? 占位符），永不内联
//   - 标识符（表名/列名/排序字段）经白名单校验：每段须匹配
//     [A-Za-z_][A-Za-z0-9_]*，允许 . 分隔的限定名（t.col），拒绝引号、括号、
//     分号、空格等一切特殊字符
//   - 排序方向仅限字面 ASC / DESC
//   - WhereRaw 是显式逃生舱：SQL 片段原样拼接，注入风险由调用方自负
//     （仅用于框架未覆盖的表达式，值仍应走参数）
//
// # 空集合语义
//
//   - WhereIn 传入空集合 → 生成恒假条件 1 = 0（匹配不到任何行，合法 SQL）
//   - WhereNotIn 传入空集合 → 生成恒真占位 1 = 1（匹配所有行）
//
// # 示例
//
//	bound, err := higomapper.NewQueryBuilder().
//		Table("users").
//		Field("id").Field("name").
//		WhereEq("status", 1).
//		WhereLike("name", "%张%").
//		WhereBetween("age", 18, 60).
//		OrderDesc("id").
//		Limit(10).
//		Build()
//	users, err := higomapper.SelectBound[User](ctx, session, bound)

import (
	"strconv"
	"strings"

	"github.com/dunpju/higo-mapper/core"
)

// condOp 比较操作符
type condOp string

const (
	opEq        condOp = "="
	opNe        condOp = "<>"
	opGt        condOp = ">"
	opLt        condOp = "<"
	opGe        condOp = ">="
	opLe        condOp = "<="
	opLike      condOp = "LIKE"
	opNotLike   condOp = "NOT LIKE"
	opIn        condOp = "IN"
	opNotIn     condOp = "NOT IN"
	opBetween   condOp = "BETWEEN"
	opIsNull    condOp = "IS NULL"
	opIsNotNull condOp = "IS NOT NULL"
)

// qbCondition WHERE 条件（简单比较 / IN / BETWEEN / 空判定 / 原生片段）
type qbCondition struct {
	kind   string // "simple" | "in" | "between" | "null" | "raw"
	col    string
	op     condOp
	val    any
	vals   []any
	high   any
	negate bool
	rawSQL string
}

// QueryBuilder 链式查询构造器（详见模块文档）
type QueryBuilder struct {
	table      string
	fields     []string
	conditions []qbCondition
	orderBy    []qbOrder
	limit      uint64
	hasLimit   bool
	offset     uint64
	hasOffset  bool
	// err 延迟到 Build() 才暴露的错误（保持链式调用无 error 包裹）
	err error
}

type qbOrder struct {
	col  string
	desc bool
}

// NewQueryBuilder 创建空构造器（后续须指定 Table）
func NewQueryBuilder() *QueryBuilder {
	return &QueryBuilder{}
}

// ─── 配置方法（链式，错误延迟到 Build）────────────────────────────

// Table 指定表名（重复调用后者覆盖）
func (b *QueryBuilder) Table(table string) *QueryBuilder {
	if err := validateIdent(table); err != nil {
		b.err = err
		return b
	}
	b.table = table
	return b
}

// Field 追加 SELECT 字段（默认 *）；字段名经标识符校验
func (b *QueryBuilder) Field(field string) *QueryBuilder {
	if err := validateIdent(field); err != nil {
		b.err = err
		return b
	}
	b.fields = append(b.fields, field)
	return b
}

// Fields 批量追加 SELECT 字段
func (b *QueryBuilder) Fields(fields ...string) *QueryBuilder {
	for _, f := range fields {
		b = b.Field(f)
	}
	return b
}

// WhereEq 等值条件 col = ?
func (b *QueryBuilder) WhereEq(col string, val any) *QueryBuilder {
	return b.pushSimple(col, opEq, val)
}

// WhereNe 不等条件 col <> ?
func (b *QueryBuilder) WhereNe(col string, val any) *QueryBuilder {
	return b.pushSimple(col, opNe, val)
}

// WhereGt 大于 col > ?
func (b *QueryBuilder) WhereGt(col string, val any) *QueryBuilder {
	return b.pushSimple(col, opGt, val)
}

// WhereLt 小于 col < ?
func (b *QueryBuilder) WhereLt(col string, val any) *QueryBuilder {
	return b.pushSimple(col, opLt, val)
}

// WhereGe 大于等于 col >= ?
func (b *QueryBuilder) WhereGe(col string, val any) *QueryBuilder {
	return b.pushSimple(col, opGe, val)
}

// WhereLe 小于等于 col <= ?
func (b *QueryBuilder) WhereLe(col string, val any) *QueryBuilder {
	return b.pushSimple(col, opLe, val)
}

// WhereLike 模糊匹配 col LIKE ?（通配符由调用方在值中提供）
func (b *QueryBuilder) WhereLike(col string, val any) *QueryBuilder {
	return b.pushSimple(col, opLike, val)
}

// WhereNotLike 反向模糊匹配 col NOT LIKE ?
func (b *QueryBuilder) WhereNotLike(col string, val any) *QueryBuilder {
	return b.pushSimple(col, opNotLike, val)
}

// WhereIn 集合条件 col IN (?, ?, ...)；空集合生成恒假 1 = 0
func (b *QueryBuilder) WhereIn(col string, vals ...any) *QueryBuilder {
	return b.pushIn(col, vals, false)
}

// WhereNotIn 反向集合 col NOT IN (?, ?, ...)；空集合生成恒真占位 1 = 1
func (b *QueryBuilder) WhereNotIn(col string, vals ...any) *QueryBuilder {
	return b.pushIn(col, vals, true)
}

// WhereBetween 区间 col BETWEEN ? AND ?（闭区间）
func (b *QueryBuilder) WhereBetween(col string, low, high any) *QueryBuilder {
	if err := validateIdent(col); err != nil {
		b.err = err
		return b
	}
	lv := NormalizeValue(low)
	hv := NormalizeValue(high)
	b.conditions = append(b.conditions, qbCondition{kind: "between", col: col, val: lv, high: hv})
	return b
}

// WhereNull col IS NULL
func (b *QueryBuilder) WhereNull(col string) *QueryBuilder {
	if err := validateIdent(col); err != nil {
		b.err = err
		return b
	}
	b.conditions = append(b.conditions, qbCondition{kind: "null", col: col})
	return b
}

// WhereNotNull col IS NOT NULL
func (b *QueryBuilder) WhereNotNull(col string) *QueryBuilder {
	if err := validateIdent(col); err != nil {
		b.err = err
		return b
	}
	b.conditions = append(b.conditions, qbCondition{kind: "null", col: col, negate: true})
	return b
}

// WhereRaw 原生条件片段（逃生舱）：SQL 原样 AND 拼接，? 对应 params 顺序。
//
// 片段内不做任何校验，注入风险由调用方自负；值请始终走 params 参数化。
func (b *QueryBuilder) WhereRaw(sqlFragment string, params ...any) *QueryBuilder {
	b.conditions = append(b.conditions, qbCondition{kind: "raw", rawSQL: sqlFragment, vals: params})
	return b
}

// OrderAsc 追加升序排序（多列可多次调用）
func (b *QueryBuilder) OrderAsc(col string) *QueryBuilder {
	return b.pushOrder(col, false)
}

// OrderDesc 追加降序排序（多列可多次调用）
func (b *QueryBuilder) OrderDesc(col string) *QueryBuilder {
	return b.pushOrder(col, true)
}

// Limit 限制行数（参数化 LIMIT ?）
func (b *QueryBuilder) Limit(limit uint64) *QueryBuilder {
	b.limit = limit
	b.hasLimit = true
	return b
}

// Offset 跳过行数（参数化 OFFSET ?；需与 Limit 同用）
func (b *QueryBuilder) Offset(offset uint64) *QueryBuilder {
	b.offset = offset
	b.hasOffset = true
	return b
}

// ─── 构建 ─────────────────────────────────────────────────────────

// Build 构建 SELECT 的 BoundSql
func (b *QueryBuilder) Build() (*core.BoundSql, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	var sb strings.Builder
	sb.WriteString("SELECT ")
	if len(b.fields) == 0 {
		sb.WriteByte('*')
	} else {
		sb.WriteString(strings.Join(b.fields, ", "))
	}
	sb.WriteString(" FROM ")
	sb.WriteString(b.table)

	params := make([]any, 0, 8)
	b.appendConditions(&sb, &params)

	if len(b.orderBy) > 0 {
		sb.WriteString(" ORDER BY ")
		parts := make([]string, 0, len(b.orderBy))
		for _, o := range b.orderBy {
			dir := "ASC"
			if o.desc {
				dir = "DESC"
			}
			parts = append(parts, o.col+" "+dir)
		}
		sb.WriteString(strings.Join(parts, ", "))
	}
	if b.hasLimit {
		sb.WriteString(" LIMIT ?")
		params = append(params, int64(b.limit))
	}
	if b.hasOffset {
		sb.WriteString(" OFFSET ?")
		params = append(params, int64(b.offset))
	}
	return &core.BoundSql{SQL: sb.String(), Parameters: params}, nil
}

// BuildCount 构建 COUNT 查询：SELECT COUNT(*) FROM <table> [WHERE …]
//
// 忽略字段、排序与分页（对总数无意义）。
func (b *QueryBuilder) BuildCount() (*core.BoundSql, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	var sb strings.Builder
	sb.WriteString("SELECT COUNT(*) FROM ")
	sb.WriteString(b.table)
	params := make([]any, 0, 8)
	b.appendConditions(&sb, &params)
	return &core.BoundSql{SQL: sb.String(), Parameters: params}, nil
}

// BuildPage 构建分页数据页 SQL（复用 PageRequest 校验，追加 LIMIT ? OFFSET ?）。
// 已手动设置 limit/offset 时报错。
func (b *QueryBuilder) BuildPage(page PageRequest) (*core.BoundSql, error) {
	if b.hasLimit || b.hasOffset {
		return nil, newPaginationError("build_page 与手动 limit/offset 互斥：分页请仅用 PageRequest")
	}
	offset, err := page.ValidatedOffset()
	if err != nil {
		return nil, err
	}
	bound, err := b.Build()
	if err != nil {
		return nil, err
	}
	appendLimitOffset(bound, page.PageSize, offset)
	return bound, nil
}

// ─── 内部辅助 ─────────────────────────────────────────────────────

func (b *QueryBuilder) pushSimple(col string, op condOp, val any) *QueryBuilder {
	if err := validateIdent(col); err != nil {
		b.err = err
		return b
	}
	b.conditions = append(b.conditions, qbCondition{kind: "simple", col: col, op: op, val: NormalizeValue(val)})
	return b
}

func (b *QueryBuilder) pushIn(col string, vals []any, negate bool) *QueryBuilder {
	if err := validateIdent(col); err != nil {
		b.err = err
		return b
	}
	norm := make([]any, 0, len(vals))
	for _, v := range vals {
		norm = append(norm, NormalizeValue(v))
	}
	b.conditions = append(b.conditions, qbCondition{kind: "in", col: col, vals: norm, negate: negate})
	return b
}

func (b *QueryBuilder) pushOrder(col string, desc bool) *QueryBuilder {
	if err := validateIdent(col); err != nil {
		b.err = err
		return b
	}
	b.orderBy = append(b.orderBy, qbOrder{col: col, desc: desc})
	return b
}

// appendConditions 追加 WHERE 子句（条件间 AND 连接）
func (b *QueryBuilder) appendConditions(sb *strings.Builder, params *[]any) {
	if len(b.conditions) == 0 {
		return
	}
	sb.WriteString(" WHERE ")
	for i, cond := range b.conditions {
		if i > 0 {
			sb.WriteString(" AND ")
		}
		switch cond.kind {
		case "simple":
			sb.WriteString(cond.col)
			sb.WriteByte(' ')
			sb.WriteString(string(cond.op))
			sb.WriteString(" ?")
			*params = append(*params, cond.val)
		case "in":
			switch {
			case len(cond.vals) == 0 && !cond.negate:
				sb.WriteString("1 = 0") // IN 空集 → 恒假
			case len(cond.vals) == 0:
				sb.WriteString("1 = 1") // NOT IN 空集 → 恒真占位（不约束结果）
			default:
				sb.WriteString(cond.col)
				if cond.negate {
					sb.WriteString(" NOT IN (")
				} else {
					sb.WriteString(" IN (")
				}
				sb.WriteString(strings.TrimSuffix(strings.Repeat("?, ", len(cond.vals)), ", "))
				sb.WriteByte(')')
				*params = append(*params, cond.vals...)
			}
		case "between":
			sb.WriteString(cond.col)
			sb.WriteString(" BETWEEN ? AND ?")
			*params = append(*params, cond.val, cond.high)
		case "null":
			sb.WriteString(cond.col)
			if cond.negate {
				sb.WriteString(" IS NOT NULL")
			} else {
				sb.WriteString(" IS NULL")
			}
		case "raw":
			sb.WriteString(cond.rawSQL)
			*params = append(*params, cond.vals...)
		}
	}
}

// check 汇总延迟错误与缺表校验
func (b *QueryBuilder) check() error {
	if b.err != nil {
		return b.err
	}
	if b.table == "" {
		return newConfigError("QueryBuilder 缺少表名：请调用 .Table(...)")
	}
	return nil
}

// validateIdent 标识符白名单校验：非空，. 分段，每段 [A-Za-z_][A-Za-z0-9_]*
func validateIdent(s string) error {
	invalid := func() error {
		return newConfigError(
			"非法标识符 '%s'：仅允许字母/数字/下划线与 '.' 分段，且每段以字母或下划线开头", s)
	}
	if s == "" {
		return invalid()
	}
	for _, seg := range strings.Split(s, ".") {
		if seg == "" {
			return invalid()
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			isAlpha := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
			isDigit := c >= '0' && c <= '9'
			if i == 0 {
				if !isAlpha && c != '_' {
					return invalid()
				}
			} else if !isAlpha && !isDigit && c != '_' {
				return invalid()
			}
		}
	}
	return nil
}

// qbInlineParams 参数内联展示（便于测试断言整体形态）
func qbInlineParams(bound *core.BoundSql) string {
	var inline = func(v any) string {
		switch t := v.(type) {
		case string:
			return "'" + t + "'"
		case nil:
			return "null"
		case bool:
			return strconv.FormatBool(t)
		case int64:
			return strconv.FormatInt(t, 10)
		case float64:
			return strconv.FormatFloat(t, 'g', -1, 64)
		}
		return "?"
	}
	out := bound.SQL
	for _, p := range bound.Parameters {
		if pos := strings.Index(out, "?"); pos >= 0 {
			out = out[:pos] + inline(p) + out[pos+1:]
		}
	}
	return out
}
