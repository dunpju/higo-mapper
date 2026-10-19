package core

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Params 通用参数表示（列名/属性名 → 值）。
//
// 值的规范形态：nil / bool / int64 / float64 / string / []any / map[string]any，
// 但本包对其他 Go 数值类型（int、float32、json.Number 等）亦尽力兼容。
type Params map[string]any

// GetParam 获取单个参数值（支持嵌套点路径，如 "user.name"）
func (p Params) GetParam(key string) (any, bool) {
	if !strings.Contains(key, ".") {
		v, ok := p[key]
		return v, ok
	}
	parts := strings.Split(key, ".")
	cur, ok := p[parts[0]]
	if !ok {
		return nil, false
	}
	for _, part := range parts[1:] {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// GetCollection 获取集合参数（仅顶层键，不支持点路径——与 Rust 版一致）
func (p Params) GetCollection(key string) ([]any, bool) {
	if arr, ok := p[key].([]any); ok {
		return arr, true
	}
	return nil, false
}

// ─── 数值辅助 ─────────────────────────────────────────────────────

// asInt64 常见整数类型 → int64
func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int8:
		return int64(t), true
	case int16:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case uint:
		if t > uint(^uint64(0)>>1) {
			return 0, false
		}
		return int64(t), true
	case uint8:
		return int64(t), true
	case uint16:
		return int64(t), true
	case uint32:
		return int64(t), true
	case uint64:
		if t > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(t), true
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

// asFloat64 常见数值类型 → float64
func asFloat64(v any) (float64, bool) {
	if i, ok := asInt64(v); ok {
		return float64(i), true
	}
	switch t := v.(type) {
	case float32:
		return float64(t), true
	case float64:
		return t, true
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}

// formatNumber 数值的最短可往返文本（用于内联 SQL）
func formatNumber(v any) (string, bool) {
	if i, ok := asInt64(v); ok {
		return strconv.FormatInt(i, 10), true
	}
	if f, ok := asFloat64(v); ok {
		return strconv.FormatFloat(f, 'g', -1, 64), true
	}
	return "", false
}

// ─── 条件表达式求值 ───────────────────────────────────────────────

var conditionRe = regexp.MustCompile(`^\s*([\w\.\(\)]+)\s*([!=<>]+)\s*(.+?)\s*$`)

type keyValue struct {
	key   string
	cond  string
	value string
}

// conditionGroup 一组由 and 连接的条件；多个组之间用 or 连接
type conditionGroup struct {
	conds []keyValue
}

// parseConditionGroups 解析条件表达式，支持 "and" 与 "or" 连接（and 优先级高于 or）
func parseConditionGroups(expr string) ([]conditionGroup, error) {
	var groups []conditionGroup
	for _, orPart := range strings.Split(expr, " or ") {
		orPart = strings.TrimSpace(orPart)
		if orPart == "" {
			continue
		}
		var conds []keyValue
		for _, cond := range strings.Split(orPart, " and ") {
			cond = strings.TrimSpace(cond)
			if cond == "" {
				continue
			}
			m := conditionRe.FindStringSubmatch(cond)
			if m == nil {
				return nil, newInvalidConditionError(cond, "无法匹配 key op value 格式")
			}
			conds = append(conds, keyValue{key: m[1], cond: m[2], value: m[3]})
		}
		if len(conds) > 0 {
			groups = append(groups, conditionGroup{conds: conds})
		}
	}
	return groups, nil
}

// conditionCache test 表达式预编译缓存（test 文本来自静态 XML，集合有限且稳定）
var conditionCache = struct {
	sync.RWMutex
	m map[string][]conditionGroup
}{m: make(map[string][]conditionGroup)}

// resolveParamValue 解析参数键的值，支持 .size() / .isEmpty() 方法调用。
//
//   - key.size()    → 集合/字符串/映射的长度（int64）；缺失或 null → 0
//   - key.isEmpty() → 是否为空（bool）；缺失或 null → true
//   - 其他          → 原始参数值
func resolveParamValue(key string, params Params) (any, bool) {
	if base, ok := strings.CutSuffix(key, ".size()"); ok {
		v, _ := params.GetParam(base)
		switch t := v.(type) {
		case []any:
			return int64(len(t)), true
		case string:
			return int64(utf8.RuneCountInString(t)), true
		case map[string]any:
			return int64(len(t)), true
		case nil:
			return int64(0), true
		default:
			return v, true
		}
	}
	if base, ok := strings.CutSuffix(key, ".isEmpty()"); ok {
		v, _ := params.GetParam(base)
		switch t := v.(type) {
		case []any:
			return len(t) == 0, true
		case string:
			return t == "", true
		case map[string]any:
			return len(t) == 0, true
		case nil:
			return true, true
		default:
			return false, true
		}
	}
	return params.GetParam(key)
}

// evaluateSingle 评估单个比较条件
func evaluateSingle(kv keyValue, params Params) bool {
	v, found := resolveParamValue(kv.key, params)
	if !found {
		// 参数不存在时，仅 "key = null" 为 true（与 Rust 版逐字对齐）
		return kv.cond == "=" && kv.value == "null"
	}

	switch kv.cond {
	case "=", "==":
		if kv.value == "null" {
			// 参数存在且为 null 时，"key == null" 才为 true
			return v == nil
		}
		if b, ok := parseBoolLiteral(kv.value); ok {
			bv, isBool := v.(bool)
			return isBool && bv == b
		}
		if s, str := unquoteString(kv.value); str {
			sv, isStr := v.(string)
			return isStr && sv == s
		}
		if num, err := strconv.ParseInt(kv.value, 10, 64); err == nil {
			iv, isInt := asInt64(v)
			return isInt && iv == num
		}
		return false

	case "!=":
		if kv.value == "null" {
			// 参数存在且为 null 时，"key != null" 应为 false
			return v != nil
		}
		if b, ok := parseBoolLiteral(kv.value); ok {
			bv, isBool := v.(bool)
			return isBool && bv != b
		}
		if s, str := unquoteString(kv.value); str {
			sv, isStr := v.(string)
			return isStr && sv != s
		}
		if num, err := strconv.ParseInt(kv.value, 10, 64); err == nil {
			iv, isInt := asInt64(v)
			return isInt && iv != num
		}
		return false

	case ">", "<", ">=", "<=":
		target, err := strconv.ParseFloat(kv.value, 64)
		if err != nil {
			return false
		}
		cur, ok := asFloat64(v)
		if !ok {
			return false
		}
		switch kv.cond {
		case ">":
			return cur > target
		case "<":
			return cur < target
		case ">=":
			return cur >= target
		case "<=":
			return cur <= target
		}
		return false
	}
	return false
}

func parseBoolLiteral(s string) (bool, bool) {
	switch {
	case strings.EqualFold(s, "true"):
		return true, true
	case strings.EqualFold(s, "false"):
		return false, true
	}
	return false, false
}

func unquoteString(s string) (string, bool) {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1], true
	}
	return "", false
}

// evalGroups 对已解析的条件组求值（组间 or，组内 and）
func evalGroups(groups []conditionGroup, params Params) bool {
	for _, g := range groups {
		all := true
		for _, kv := range g.conds {
			if !evaluateSingle(kv, params) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// evaluateCondition 评估 <if>/<when> 的 test 表达式（带解析缓存）
func evaluateCondition(condition string, params Params) bool {
	conditionCache.RLock()
	groups, ok := conditionCache.m[condition]
	conditionCache.RUnlock()
	if !ok {
		g, err := parseConditionGroups(condition)
		if err != nil {
			g = nil // 失败回退为空 → 恒 false，保持旧行为
		}
		conditionCache.Lock()
		conditionCache.m[condition] = g
		conditionCache.Unlock()
		groups = g
	}
	return evalGroups(groups, params)
}

// ─── 内联值格式化 ─────────────────────────────────────────────────

// inlineRaw ${} 内联：字符串原样、数字文本、布尔 1/0、NULL、复杂类型 JSON 文本
func inlineRaw(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if t {
			return "1"
		}
		return "0"
	case string:
		return t
	}
	if s, ok := formatNumber(v); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// inlineQuoted #{} 内联（buildSQL 模式）：字符串加引号并转义内嵌单引号
func inlineQuoted(v any) string {
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
	}
	if s, ok := formatNumber(v); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return "'" + string(b) + "'"
}

// ─── 占位符替换（内联模式）─────────────────────────────────────────

var (
	hashParamRe   = regexp.MustCompile(`#\{([^}]*)\}`)
	dollarParamRe = regexp.MustCompile(`\$\{([^}]*)\}`)
)

// replaceParameters 替换 #{...}（字符串加引号）与 ${...}（原样内联）
func replaceParameters(content string, params Params) (string, error) {
	// 短路：无占位符的字面文本直接返回
	if !strings.Contains(content, "#{") && !strings.Contains(content, "${") {
		return content, nil
	}
	// 先处理 ${...} — 原样替换，不加引号
	withDollar := dollarParamRe.ReplaceAllStringFunc(content, func(m string) string {
		path := m[2 : len(m)-1]
		if v, ok := params.GetParam(path); ok {
			return inlineRaw(v)
		}
		return fmt.Sprintf("/* MISSING:${%s} */", path)
	})
	// 再处理 #{...} — 字符串加引号
	return hashParamRe.ReplaceAllStringFunc(withDollar, func(m string) string {
		path := m[2 : len(m)-1]
		if v, ok := params.GetParam(path); ok {
			return inlineQuoted(v)
		}
		return fmt.Sprintf("/* MISSING:#%s */", path)
	}), nil
}

// ─── 辅助函数 ─────────────────────────────────────────────────────

// copyParams 浅拷贝参数表（foreach 循环变量注入用；嵌套值只读共享）
func copyParams(params Params) Params {
	c := make(Params, len(params)+2)
	for k, v := range params {
		c[k] = v
	}
	return c
}

// collapseWhitespace 将任意连续空白归一化为单词间单个空格
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// stripOverrides 去除 SQL 前缀/后缀（overrides 以 "|" 分隔；未命中原样返回）
func stripOverrides(sql string, overrides, def string, stripPrefix bool) string {
	effective := overrides
	if effective == "" {
		effective = def
	}
	if effective == "" {
		return sql
	}
	for _, part := range strings.Split(effective, "|") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if stripPrefix {
			if rest, ok := strings.CutPrefix(sql, part); ok {
				return strings.TrimLeftFunc(rest, unicode.IsSpace)
			}
		} else if rest, ok := strings.CutSuffix(sql, part); ok {
			return strings.TrimRightFunc(rest, unicode.IsSpace)
		}
	}
	return sql
}

// joinWithSpaces 将节点序列拼接为 SQL，支持 bind 变量注入
func joinWithSpaces(nodes []SqlNode, params Params, mapper *Mapper) (string, error) {
	var raw strings.Builder
	var enriched Params // 仅遇到 <bind> 时才克隆父参数表

	for _, n := range nodes {
		if b, ok := n.(*BindNode); ok {
			if enriched == nil {
				enriched = copyParams(params)
			}
			resolved, err := replaceParameters(b.Value, enriched)
			if err != nil {
				return "", err
			}
			enriched[b.Name] = resolved
			continue
		}
		p := params
		if enriched != nil {
			p = enriched
		}
		sql, err := GenerateSQL(n, p, mapper)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(sql) != "" {
			if raw.Len() > 0 {
				raw.WriteByte(' ')
			}
			raw.WriteString(sql)
		}
	}

	return collapseWhitespace(raw.String()), nil
}

// foreachItems 取 foreach 的集合参数
func foreachItems(params Params, collection string) []any {
	if arr, ok := params.GetCollection(collection); ok {
		return arr
	}
	if v, ok := params.GetParam(collection); ok {
		if arr, ok := v.([]any); ok {
			return arr
		}
	}
	return nil
}

// lookupFragment 查找 SQL 片段（支持 namespace.id 形式，当前仅在同 mapper 内查找）
func lookupFragment(mapper *Mapper, refID string) ([]SqlNode, error) {
	if strings.Contains(refID, ".") {
		_, id, _ := strings.Cut(refID, ".")
		if frag, ok := mapper.SqlFragments[id]; ok {
			return frag, nil
		}
		return nil, newMissingFragmentError(refID)
	}
	if frag, ok := mapper.SqlFragments[refID]; ok {
		return frag, nil
	}
	return nil, newMissingFragmentError(refID)
}

// ─── 核心 SQL 生成（内联模式）──────────────────────────────────────

// GenerateSQL 按动态节点生成最终 SQL（内联模式：#{}/${} 直接内联值）
func GenerateSQL(node SqlNode, params Params, mapper *Mapper) (string, error) {
	switch n := node.(type) {
	case *TextNode:
		return replaceParameters(n.Content, params)

	case *IfNode:
		if evaluateCondition(n.Test, params) {
			return joinWithSpaces(n.Contents, params, mapper)
		}
		return "", nil

	case *ForeachNode:
		items := foreachItems(params, n.Collection)
		if len(items) == 0 {
			return "", nil
		}
		// 克隆一次父参数表，循环内仅覆盖 item/index
		tmp := copyParams(params)
		var b strings.Builder
		b.WriteString(n.Open)
		for i, item := range items {
			if i > 0 {
				b.WriteString(n.Separator)
			}
			tmp[n.Item] = item
			if n.Index != "" {
				tmp[n.Index] = int64(i)
			}
			sql, err := joinWithSpaces(n.Contents, tmp, mapper)
			if err != nil {
				return "", err
			}
			b.WriteString(sql)
		}
		b.WriteString(n.Close)
		return b.String(), nil

	case *TrimNode:
		sql, err := joinWithSpaces(n.Contents, params, mapper)
		if err != nil {
			return "", err
		}
		sql = stripOverrides(sql, n.PrefixOverrides, "", true)
		sql = stripOverrides(sql, n.SuffixOverrides, "", false)
		if p := strings.TrimRightFunc(n.Prefix, unicode.IsSpace); p != "" && sql != "" {
			sql = p + " " + strings.TrimLeftFunc(sql, unicode.IsSpace)
		}
		if s := strings.TrimLeftFunc(n.Suffix, unicode.IsSpace); s != "" && sql != "" {
			sql = strings.TrimRightFunc(sql, unicode.IsSpace) + " " + s
		}
		return sql, nil

	case *ChooseNode:
		for _, w := range n.Whens {
			if evaluateCondition(w.Test, params) {
				return joinWithSpaces(w.Contents, params, mapper)
			}
		}
		if n.Otherwise != nil {
			return joinWithSpaces(n.Otherwise, params, mapper)
		}
		return "", nil

	case *BindNode:
		return "", nil // 在 joinWithSpaces 中处理

	case *IncludeNode:
		fragment, err := lookupFragment(mapper, n.RefID)
		if err != nil {
			return "", err
		}
		var result strings.Builder
		for _, node := range fragment {
			sql, err := GenerateSQL(node, params, mapper)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(sql) == "" {
				continue
			}
			if result.Len() > 0 {
				result.WriteByte(' ')
			}
			result.WriteString(sql)
		}
		return result.String(), nil

	case *WhereNode:
		sql, err := joinWithSpaces(n.Contents, params, mapper)
		if err != nil {
			return "", err
		}
		sql = stripOverrides(sql, n.PrefixOverrides, "AND |OR ", true)
		sql = stripOverrides(sql, n.SuffixOverrides, "", false)
		if sql == "" {
			return "", nil
		}
		return "WHERE " + strings.TrimLeftFunc(sql, unicode.IsSpace), nil

	case *SetNode:
		sql, err := joinWithSpaces(n.Contents, params, mapper)
		if err != nil {
			return "", err
		}
		sql = stripOverrides(sql, n.PrefixOverrides, "", true)
		sql = stripOverrides(sql, n.SuffixOverrides, ",", false)
		if sql == "" {
			return "", nil
		}
		return "SET " + strings.TrimLeftFunc(sql, unicode.IsSpace), nil

	case *MixedNode:
		return joinWithSpaces(n.Contents, params, mapper)
	}
	return "", nil
}

// BuildSQL 一站式 SQL 生成：按语句 ID 查找并生成最终 SQL（内联模式）。
//
// 所有 #{param} / ${param} 都直接内联到 SQL 字符串中（字符串值自动加引号）。
func (m *Mapper) BuildSQL(statementID string, params Params) (string, error) {
	stmt, ok := m.Statements[statementID]
	if !ok {
		return "", newStatementNotFoundError(statementID)
	}
	if stmt.DynamicSQL != nil {
		return GenerateSQL(stmt.DynamicSQL, params, m)
	}
	// 纯静态 SQL，仅做参数替换
	return replaceParameters(stmt.SQL, params)
}
