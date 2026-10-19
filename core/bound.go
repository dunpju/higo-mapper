package core

import (
	"fmt"
	"strings"
	"unicode"
)

// BoundSql 绑定后的 SQL：含 ? 占位符的 SQL 字符串 + 有序参数列表。
//
// 两阶段解析的绑定阶段输出：? 占位符与 Parameters 一一对应（按出现顺序），
// 可直接绑定到 database/sql 执行。${} 内联部分保持原样（无法参数化）。
type BoundSql struct {
	// SQL 含 ? 占位符的 SQL（${} 内联部分保持原样）
	SQL string
	// Parameters 有序参数列表，与 SQL 中 ? 占位符一一对应
	Parameters []any
}

// NewBoundSql 创建带初始 SQL 的 BoundSql（参数列表为空）
func NewBoundSql(sql string) *BoundSql {
	return &BoundSql{SQL: sql}
}

// ParamCount 参数数量
func (b *BoundSql) ParamCount() int { return len(b.Parameters) }

// HasParams 是否包含参数
func (b *BoundSql) HasParams() bool { return len(b.Parameters) > 0 }

// replaceParametersBound 替换 #{...} 为 ? 占位符（参数进列表），${...} 原样内联。
//
// 这是 replaceParameters 的「绑定版本」：#{} 不再内联值，
// 而是输出 ? 并将值按出现顺序收集到 BoundSql.Parameters。
func replaceParametersBound(content string, params Params) (*BoundSql, error) {
	// 短路：无占位符的字面文本直接返回（零参数）
	if !strings.Contains(content, "#{") && !strings.Contains(content, "${") {
		return &BoundSql{SQL: content}, nil
	}
	// 先处理 ${...} — 原样内联（无法参数化）
	withDollar := dollarParamRe.ReplaceAllStringFunc(content, func(m string) string {
		path := m[2 : len(m)-1]
		if v, ok := params.GetParam(path); ok {
			return inlineRaw(v)
		}
		return fmt.Sprintf("/* MISSING:${%s} */", path)
	})
	// 再处理 #{...} — 替换为 ? 占位符 + 参数进列表（按出现顺序）
	var boundParams []any
	sql := hashParamRe.ReplaceAllStringFunc(withDollar, func(m string) string {
		path := m[2 : len(m)-1]
		if v, ok := params.GetParam(path); ok {
			boundParams = append(boundParams, v)
			return "?"
		}
		return fmt.Sprintf("/* MISSING:#%s */", path)
	})
	return &BoundSql{SQL: sql, Parameters: boundParams}, nil
}

// joinWithSpacesBound 将节点序列拼接为 BoundSql，支持 bind 变量注入。
//
// joinWithSpaces 的「绑定版本」：拼接 SQL 文本的同时保持参数顺序。
func joinWithSpacesBound(nodes []SqlNode, params Params, mapper *Mapper) (*BoundSql, error) {
	result := &BoundSql{}
	var enriched Params

	for _, n := range nodes {
		if b, ok := n.(*BindNode); ok {
			if enriched == nil {
				enriched = copyParams(params)
			}
			resolved, err := replaceParameters(b.Value, enriched)
			if err != nil {
				return nil, err
			}
			enriched[b.Name] = resolved
			continue
		}
		p := params
		if enriched != nil {
			p = enriched
		}
		child, err := GenerateBoundSQL(n, p, mapper)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(child.SQL) != "" {
			if result.SQL != "" {
				result.SQL += " "
			}
			result.SQL += child.SQL
			result.Parameters = append(result.Parameters, child.Parameters...)
		}
	}

	// 合并连续空白（不影响 ? 与参数的对应关系）
	result.SQL = collapseWhitespace(result.SQL)
	return result, nil
}

// GenerateBoundSQL 生成 BoundSql（两阶段绑定的绑定阶段）。
//
// 与 GenerateSQL 结构完全一致，区别仅在于叶子节点（Text）的参数处理：
//   - #{param} → ? 占位符 + 参数进入 BoundSql.Parameters
//   - ${param} → 原样内联
func GenerateBoundSQL(node SqlNode, params Params, mapper *Mapper) (*BoundSql, error) {
	switch n := node.(type) {
	case *TextNode:
		return replaceParametersBound(n.Content, params)

	case *IfNode:
		if evaluateCondition(n.Test, params) {
			return joinWithSpacesBound(n.Contents, params, mapper)
		}
		return &BoundSql{}, nil

	case *ForeachNode:
		items := foreachItems(params, n.Collection)
		if len(items) == 0 {
			return &BoundSql{}, nil
		}
		// 克隆一次父参数表，循环内仅覆盖 item/index
		tmp := copyParams(params)
		result := &BoundSql{SQL: n.Open}
		for i, item := range items {
			if i > 0 {
				result.SQL += n.Separator
			}
			tmp[n.Item] = item
			if n.Index != "" {
				tmp[n.Index] = int64(i)
			}
			child, err := joinWithSpacesBound(n.Contents, tmp, mapper)
			if err != nil {
				return nil, err
			}
			result.SQL += child.SQL
			result.Parameters = append(result.Parameters, child.Parameters...)
		}
		result.SQL += n.Close
		return result, nil

	case *TrimNode:
		bound, err := joinWithSpacesBound(n.Contents, params, mapper)
		if err != nil {
			return nil, err
		}
		bound.SQL = stripOverrides(bound.SQL, n.PrefixOverrides, "", true)
		bound.SQL = stripOverrides(bound.SQL, n.SuffixOverrides, "", false)
		if p := strings.TrimRightFunc(n.Prefix, unicode.IsSpace); p != "" && bound.SQL != "" {
			bound.SQL = p + " " + strings.TrimLeftFunc(bound.SQL, unicode.IsSpace)
		}
		if s := strings.TrimLeftFunc(n.Suffix, unicode.IsSpace); s != "" && bound.SQL != "" {
			bound.SQL = strings.TrimRightFunc(bound.SQL, unicode.IsSpace) + " " + s
		}
		return bound, nil

	case *ChooseNode:
		for _, w := range n.Whens {
			if evaluateCondition(w.Test, params) {
				return joinWithSpacesBound(w.Contents, params, mapper)
			}
		}
		if n.Otherwise != nil {
			return joinWithSpacesBound(n.Otherwise, params, mapper)
		}
		return &BoundSql{}, nil

	case *BindNode:
		return &BoundSql{}, nil // 在 joinWithSpacesBound 中处理

	case *IncludeNode:
		fragment, err := lookupFragment(mapper, n.RefID)
		if err != nil {
			return nil, err
		}
		result := &BoundSql{}
		for _, node := range fragment {
			part, err := GenerateBoundSQL(node, params, mapper)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(part.SQL) == "" {
				continue
			}
			if result.SQL != "" {
				result.SQL += " "
			}
			result.SQL += part.SQL
			result.Parameters = append(result.Parameters, part.Parameters...)
		}
		return result, nil

	case *WhereNode:
		bound, err := joinWithSpacesBound(n.Contents, params, mapper)
		if err != nil {
			return nil, err
		}
		bound.SQL = stripOverrides(bound.SQL, n.PrefixOverrides, "AND |OR ", true)
		bound.SQL = stripOverrides(bound.SQL, n.SuffixOverrides, "", false)
		if bound.SQL == "" {
			return bound, nil
		}
		bound.SQL = "WHERE " + strings.TrimLeftFunc(bound.SQL, unicode.IsSpace)
		return bound, nil

	case *SetNode:
		bound, err := joinWithSpacesBound(n.Contents, params, mapper)
		if err != nil {
			return nil, err
		}
		bound.SQL = stripOverrides(bound.SQL, n.PrefixOverrides, "", true)
		bound.SQL = stripOverrides(bound.SQL, n.SuffixOverrides, ",", false)
		if bound.SQL == "" {
			return bound, nil
		}
		bound.SQL = "SET " + strings.TrimLeftFunc(bound.SQL, unicode.IsSpace)
		return bound, nil

	case *MixedNode:
		return joinWithSpacesBound(n.Contents, params, mapper)
	}
	return &BoundSql{}, nil
}

// BuildBoundSQL 两阶段绑定：按语句 ID 查找并生成 BoundSql（参数化模式）。
//
// 与 BuildSQL 的区别：
//   - #{param} → 替换为 ? 占位符，参数值按出现顺序进入 BoundSql.Parameters
//   - ${param} → 原样内联（无法参数化，保持原行为）
//
// 建议优先使用本方法以获得参数化查询的安全性（防 SQL 注入）。
func (m *Mapper) BuildBoundSQL(statementID string, params Params) (*BoundSql, error) {
	stmt, ok := m.Statements[statementID]
	if !ok {
		return nil, newStatementNotFoundError(statementID)
	}
	if stmt.DynamicSQL != nil {
		return GenerateBoundSQL(stmt.DynamicSQL, params, m)
	}
	return replaceParametersBound(stmt.SQL, params)
}
