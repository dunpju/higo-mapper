package core

import (
	"encoding/xml"
	"io"
	"strings"
)

// MyBatisXmlParser MyBatis XML 解析器。
//
// 基于 encoding/xml 的 Token 拉取式解析，事件模型与 Rust 版的 quick-xml 一致；
// 实体会被标准库自动解码（&gt; → >），自闭合标签表现为 Start 紧跟 End。
type MyBatisXmlParser struct {
	dec *xml.Decoder
	// capturedSelectKey 语句体解析期间捕获的 selectKey（瞬态，每条语句重置）
	capturedSelectKey *SelectKey
}

// NewMyBatisXmlParser 从 XML 字符串创建解析器
func NewMyBatisXmlParser(xmlContent string) *MyBatisXmlParser {
	return &MyBatisXmlParser{dec: xml.NewDecoder(strings.NewReader(xmlContent))}
}

// ParseMapper 便捷函数：解析 XML 字符串并返回 Mapper
func ParseMapper(xmlContent string) (*Mapper, error) {
	return NewMyBatisXmlParser(xmlContent).ParseMapper()
}

// ParseMapper 解析 mapper 文件
func (p *MyBatisXmlParser) ParseMapper() (*Mapper, error) {
	mapper := NewMapper()
	inMapper := false

	for {
		tok, err := p.dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, newParseError("%s", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if !inMapper && t.Name.Local != "mapper" {
				continue
			}
			switch t.Name.Local {
			case "mapper":
				inMapper = true
				// namespace 缺失时保持空（与 Rust 版一致，不报错）
				if ns, err := attrValue(t, "namespace"); err == nil {
					mapper.Namespace = ns
				}
			case "select", "insert", "update", "delete":
				stmt, err := p.parseSqlStatement(stmtTypeOf(t.Name.Local), t)
				if err != nil {
					return nil, err
				}
				mapper.Statements[stmt.ID] = stmt
			case "resultMap":
				rm, err := p.parseResultMap(t)
				if err != nil {
					return nil, err
				}
				mapper.ResultMaps[rm.ID] = rm
			case "sql":
				id, err := requireAttr(t, "id", "<sql>标签缺少id属性")
				if err != nil {
					return nil, err
				}
				var discard strings.Builder
				var contents []SqlNode
				if err := p.parseSqlContent(&discard, &contents); err != nil {
					return nil, err
				}
				mapper.SqlFragments[id] = contents
			}
		case xml.EndElement:
			if t.Name.Local == "mapper" {
				return mapper, nil
			}
		}
	}

	return mapper, nil
}

func stmtTypeOf(name string) StatementType {
	switch name {
	case "insert":
		return StatementInsert
	case "update":
		return StatementUpdate
	case "delete":
		return StatementDelete
	default:
		return StatementSelect
	}
}

// attrValue 按名称读取属性值（标准库已完成实体解码）
func attrValue(e xml.StartElement, name string) (string, error) {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value, nil
		}
	}
	return "", newParseError("<%s>缺少%s属性", e.Name.Local, name)
}

// requireAttr 必需属性：缺失时报解析错误
func requireAttr(e xml.StartElement, name, errMsg string) (string, error) {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value, nil
		}
	}
	return "", newParseError("%s", errMsg)
}

// optionalAttr 可选属性：缺失或空字符串视为未设置
func optionalAttr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name && a.Value != "" {
			return a.Value
		}
	}
	return ""
}

// parseSqlStatement 解析 SQL 语句（select/insert/update/delete 的标签体）
func (p *MyBatisXmlParser) parseSqlStatement(stmtType StatementType, start xml.StartElement) (*SqlStatement, error) {
	stmt := &SqlStatement{StmtType: stmtType}
	for _, a := range start.Attr {
		switch a.Name.Local {
		case "id":
			stmt.ID = a.Value
		case "parameterType":
			stmt.ParameterType = a.Value
		case "resultType":
			stmt.ResultType = a.Value
		case "resultMap":
			stmt.ResultMap = a.Value
		}
	}

	var sqlBuf strings.Builder
	var nodes []SqlNode
	p.capturedSelectKey = nil
	if err := p.parseSqlContent(&sqlBuf, &nodes); err != nil {
		return nil, err
	}

	stmt.SQL = sqlBuf.String()
	if len(nodes) == 1 {
		stmt.DynamicSQL = nodes[0]
	} else if len(nodes) > 1 {
		stmt.DynamicSQL = &MixedNode{Contents: nodes}
	}
	stmt.Parameters = extractParameters(stmt.SQL)
	stmt.SelectKey = p.capturedSelectKey
	p.capturedSelectKey = nil

	return stmt, nil
}

// parseSqlContent 解析当前元素的 SQL 内容：文本入 sqlBuf（仅本层文本），
// 非空文本与动态标签按序收集到 nodes，直到本层 EndElement。
func (p *MyBatisXmlParser) parseSqlContent(sqlBuf *strings.Builder, nodes *[]SqlNode) error {
	for {
		tok, err := p.dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return newParseError("%s", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if err := p.handleDynamicTag(t, nodes); err != nil {
				return err
			}
		case xml.EndElement:
			// 本层元素闭合
			return nil
		case xml.CharData:
			text := string(t)
			sqlBuf.WriteString(text)
			if strings.TrimSpace(text) != "" {
				*nodes = append(*nodes, &TextNode{Content: text})
			}
		}
	}
}

// handleDynamicTag 处理动态 SQL 标签。
//
// 子内容经独立的丢弃缓冲解析（与 Rust 版一致：子文本不进入父层 stmt.SQL）。
func (p *MyBatisXmlParser) handleDynamicTag(e xml.StartElement, nodes *[]SqlNode) error {
	switch e.Name.Local {
	case "if":
		test, err := requireAttr(e, "test", "<if>标签缺少test属性")
		if err != nil {
			return err
		}
		var discard strings.Builder
		var contents []SqlNode
		if err := p.parseSqlContent(&discard, &contents); err != nil {
			return err
		}
		*nodes = append(*nodes, &IfNode{Test: strings.TrimSpace(test), Contents: contents})

	case "bind":
		name, err := requireAttr(e, "name", "<bind>标签缺少name属性")
		if err != nil {
			return err
		}
		value, err := requireAttr(e, "value", "<bind>标签缺少value属性")
		if err != nil {
			return err
		}
		*nodes = append(*nodes, &BindNode{Name: name, Value: value})
		// 消费可能的闭合标签（<bind/> 自闭合 或 <bind></bind> 空体）
		return p.skipElement()

	case "include":
		refID, err := requireAttr(e, "refid", "<include>标签缺少refid属性")
		if err != nil {
			return err
		}
		*nodes = append(*nodes, &IncludeNode{RefID: refID})
		// 消费可能的闭合标签
		return p.skipElement()

	case "foreach":
		collection, err := requireAttr(e, "collection", "<foreach>标签缺少collection属性")
		if err != nil {
			return err
		}
		item, err := requireAttr(e, "item", "<foreach>标签缺少item属性")
		if err != nil {
			return err
		}
		var discard strings.Builder
		var contents []SqlNode
		if err := p.parseSqlContent(&discard, &contents); err != nil {
			return err
		}
		*nodes = append(*nodes, &ForeachNode{
			Collection: collection,
			Item:       item,
			Index:      optionalAttr(e, "index"),
			Open:       defaultAttr(e, "open"),
			Separator:  defaultAttr(e, "separator"),
			Close:      defaultAttr(e, "close"),
			Contents:   contents,
		})

	case "where":
		var discard strings.Builder
		var contents []SqlNode
		if err := p.parseSqlContent(&discard, &contents); err != nil {
			return err
		}
		*nodes = append(*nodes, &WhereNode{
			PrefixOverrides: optionalAttr(e, "prefixOverrides"),
			SuffixOverrides: optionalAttr(e, "suffixOverrides"),
			Contents:        contents,
		})

	case "trim":
		var discard strings.Builder
		var contents []SqlNode
		if err := p.parseSqlContent(&discard, &contents); err != nil {
			return err
		}
		*nodes = append(*nodes, &TrimNode{
			Prefix:          optionalAttr(e, "prefix"),
			PrefixOverrides: optionalAttr(e, "prefixOverrides"),
			Suffix:          optionalAttr(e, "suffix"),
			SuffixOverrides: optionalAttr(e, "suffixOverrides"),
			Contents:        contents,
		})

	case "set":
		var discard strings.Builder
		var contents []SqlNode
		if err := p.parseSqlContent(&discard, &contents); err != nil {
			return err
		}
		*nodes = append(*nodes, &SetNode{
			PrefixOverrides: optionalAttr(e, "prefixOverrides"),
			SuffixOverrides: optionalAttr(e, "suffixOverrides"),
			Contents:        contents,
		})

	case "choose":
		node, err := p.parseChoose()
		if err != nil {
			return err
		}
		*nodes = append(*nodes, node)

	case "selectKey":
		// 解析 selectKey，捕获但不加入 SQL 节点
		keyProperty, err := requireAttr(e, "keyProperty", "<selectKey>缺少keyProperty属性")
		if err != nil {
			return err
		}
		order := SelectKeyAfter
		if strings.EqualFold(optionalAttr(e, "order"), "BEFORE") {
			order = SelectKeyBefore
		}
		var skBuf strings.Builder
		var skNodes []SqlNode
		if err := p.parseSqlContent(&skBuf, &skNodes); err != nil {
			return err
		}
		p.capturedSelectKey = &SelectKey{
			KeyProperty: keyProperty,
			ResultType:  optionalAttr(e, "resultType"),
			Order:       order,
			SQL:         strings.TrimSpace(skBuf.String()),
		}

	default:
		// 未知标签：跳过完整子树
		return p.skipElement()
	}
	return nil
}

// defaultAttr 读取属性值，缺失时返回空字符串
func defaultAttr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// parseChoose 解析 choose 标签内部结构（when/otherwise）
func (p *MyBatisXmlParser) parseChoose() (*ChooseNode, error) {
	node := &ChooseNode{}
	for {
		tok, err := p.dec.Token()
		if err == io.EOF {
			return node, nil
		}
		if err != nil {
			return nil, newParseError("%s", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "when":
				test, err := requireAttr(t, "test", "<when>标签缺少test属性")
				if err != nil {
					return nil, err
				}
				var discard strings.Builder
				var contents []SqlNode
				if err := p.parseSqlContent(&discard, &contents); err != nil {
					return nil, err
				}
				node.Whens = append(node.Whens, WhenNode{Test: strings.TrimSpace(test), Contents: contents})
			case "otherwise":
				var discard strings.Builder
				var contents []SqlNode
				if err := p.parseSqlContent(&discard, &contents); err != nil {
					return nil, err
				}
				node.Otherwise = contents
			default:
				if err := p.skipElement(); err != nil {
					return nil, err
				}
			}
		case xml.EndElement:
			if t.Name.Local == "choose" {
				return node, nil
			}
		}
	}
}

// parseResultMap 解析 <resultMap>
func (p *MyBatisXmlParser) parseResultMap(start xml.StartElement) (*ResultMap, error) {
	rm := &ResultMap{}
	for _, a := range start.Attr {
		switch a.Name.Local {
		case "id":
			rm.ID = a.Value
		case "type":
			rm.TypeName = a.Value
		}
	}
	if err := p.parseMappingBody(&rm.ResultColumns, &rm.Associations, &rm.Collections); err != nil {
		return nil, err
	}
	return rm, nil
}

// parseMappingBody 解析 resultMap / 嵌套映射的公共子体
// （<id>/<result>/<association>/<collection>，自闭合与带体两种形式）
func (p *MyBatisXmlParser) parseMappingBody(
	resultColumns *[]ResultColumn,
	associations *[]NestedMapping,
	collections *[]NestedMapping,
) error {
	for {
		tok, err := p.dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return newParseError("%s", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if err := p.handleMappingElement(t, resultColumns, associations, collections); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}

// handleMappingElement 处理单个映射元素（id/result/association/collection）
func (p *MyBatisXmlParser) handleMappingElement(
	e xml.StartElement,
	resultColumns *[]ResultColumn,
	associations *[]NestedMapping,
	collections *[]NestedMapping,
) error {
	switch e.Name.Local {
	case "id", "result":
		col := parseResultColumn(e, e.Name.Local == "id")
		*resultColumns = append(*resultColumns, col)
		// 消费闭合标签（自闭合形式为紧随的 End）
		return p.skipElement()

	case "association", "collection":
		nm := NestedMapping{}
		for _, a := range e.Attr {
			switch a.Name.Local {
			case "property":
				nm.Property = a.Value
			case "column":
				nm.Column = a.Value
			case "javaType", "ofType", "resultType":
				nm.NestedType = a.Value
			case "select":
				nm.Select = a.Value
			}
		}
		// 自闭合形式：parseMappingBody 立即读到本元素的 End 并返回
		if err := p.parseMappingBody(&nm.ResultColumns, &nm.Associations, &nm.Collections); err != nil {
			return err
		}
		if e.Name.Local == "collection" {
			*collections = append(*collections, nm)
		} else {
			*associations = append(*associations, nm)
		}
		return nil

	default:
		return p.skipElement()
	}
}

// parseResultColumn 从 <id>/<result> 标签解析列映射
func parseResultColumn(e xml.StartElement, isID bool) ResultColumn {
	col := ResultColumn{IsID: isID}
	for _, a := range e.Attr {
		switch a.Name.Local {
		case "property":
			col.Property = a.Value
		case "column":
			col.Column = a.Value
		case "javaType":
			col.JavaType = a.Value
		case "jdbcType":
			col.JdbcType = a.Value
		case "goType", "rustType":
			col.GoType = a.Value
		}
	}
	return col
}

// extractParameters 提取 SQL 中的参数名（支持 #{param} 和 ${param}；
// param 部分在首个 ':' 或 ',' 处截断，如 #{id,jdbcType=VARCHAR} → id）
func extractParameters(sql string) []string {
	seen := make(map[string]struct{})
	var params []string
	rs := []rune(sql)
	for i := 0; i < len(rs); i++ {
		if (rs[i] == '#' || rs[i] == '$') && i+1 < len(rs) && rs[i+1] == '{' {
			end := i + 2
			for end < len(rs) && rs[end] != '}' {
				end++
			}
			if end >= len(rs) {
				break
			}
			raw := string(rs[i+2 : end])
			name := raw
			if idx := strings.IndexAny(raw, ":,"); idx >= 0 {
				name = raw[:idx]
			}
			name = strings.TrimSpace(name)
			if name != "" {
				if _, ok := seen[name]; !ok {
					seen[name] = struct{}{}
					params = append(params, name)
				}
			}
			i = end
		}
	}
	return params
}

// skipElement 跳过当前元素的剩余部分（含嵌套子树；自闭合形式仅消费紧随的 End）
func (p *MyBatisXmlParser) skipElement() error {
	depth := 1
	for depth > 0 {
		tok, err := p.dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return newParseError("%s", err)
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}
