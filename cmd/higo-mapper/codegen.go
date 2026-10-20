// 代码生成：TableMeta → entity .go（json tag 映射列名）与 mapper XML 骨架。

package main

import (
	"fmt"
	"go/format"
	"strings"
)

// FieldSpec 单列的生成产物（字段名 + Go 类型 + json tag）
type FieldSpec struct {
	FieldName string
	GoType    string // 含可空的指针包装
	// GoTypeAttr goType 属性（json/decimal）——仅用于 XML resultMap
	GoTypeAttr string
	Note       string
	Column     ColumnMeta
}

// pascalCase snake_case → PascalCase（user_orders → UserOrders）
func pascalCase(name string) string {
	var sb strings.Builder
	for _, seg := range strings.Split(name, "_") {
		if seg == "" {
			continue
		}
		sb.WriteString(strings.ToUpper(seg[:1]) + seg[1:])
	}
	return sb.String()
}

// goFieldName 列名 → Go 导出字段名：先小写化并把 camelCase 转为 snake_case
// （userId → user_id），再 PascalCase（user_id → UserId）。
// 与 json tag（原列名）配合，两种映射路径（列名直映 / resultMap）均正确。
func goFieldName(column string) string {
	var snake strings.Builder
	for i, ch := range column {
		if ch >= 'A' && ch <= 'Z' {
			if i > 0 {
				snake.WriteByte('_')
			}
			snake.WriteRune(ch - 'A' + 'a')
		} else {
			snake.WriteRune(ch)
		}
	}
	return pascalCase(snake.String())
}

func fieldSpecs(meta *TableMeta, dialect string) []FieldSpec {
	specs := make([]FieldSpec, 0, len(meta.Columns))
	for _, c := range meta.Columns {
		var mapped GoType
		if dialect == "mysql" {
			mapped = mysqlType(c.DataType, c.FullType)
		} else {
			mapped = sqliteType(c.DataType)
		}
		goType := mapped.Base
		if c.Nullable && !c.IsPrimaryKey {
			goType = "*" + goType
		}
		specs = append(specs, FieldSpec{
			FieldName:  goFieldName(c.Name),
			GoType:     goType,
			GoTypeAttr: mapped.GoTypeAttr,
			Note:       mapped.Note,
			Column:     c,
		})
	}
	return specs
}

// generateEntity 生成 entity .go 文件内容（经 go/format 对齐）
func generateEntity(meta *TableMeta, dialect, pkg string) string {
	structName := pascalCase(meta.Name)
	specs := fieldSpecs(meta, dialect)

	var b strings.Builder
	fmt.Fprintf(&b, "// 由 higo-mapper 自动生成（表: %s），可按需手工调整。\n", meta.Name)
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	fmt.Fprintf(&b, "// %s 表 %s 的实体\n", structName, meta.Name)
	fmt.Fprintf(&b, "type %s struct {\n", structName)
	for _, s := range specs {
		if s.Note != "" {
			fmt.Fprintf(&b, "\t// %s\n", s.Note)
		}
		fmt.Fprintf(&b, "\t%s %s `json:\"%s\"`\n", s.FieldName, s.GoType, s.Column.Name)
	}
	b.WriteString("}\n")

	// gofmt 对齐（失败则原样返回，保证内容不丢）
	if formatted, err := format.Source([]byte(b.String())); err == nil {
		return string(formatted)
	}
	return b.String()
}

// generateXML 生成 mapper XML 骨架
func generateXML(meta *TableMeta, dialect, namespacePrefix string) string {
	structName := pascalCase(meta.Name)
	specs := fieldSpecs(meta, dialect)
	ns := fmt.Sprintf("%s.%sDao", namespacePrefix, structName)
	table := meta.Name
	pk := ""
	if key := meta.PrimaryKey(); key != nil {
		pk = key.Name
	} else if len(specs) > 0 {
		pk = specs[0].Column.Name
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<!-- 由 higo-mapper 自动生成（表: %s），可按需手工调整 -->\n", table)
	fmt.Fprintf(&b, "<mapper namespace=\"%s\">\n", ns)
	fmt.Fprintf(&b, "    <resultMap id=\"baseMap\" type=\"%s\">\n", structName)
	for _, s := range specs {
		goAttr := ""
		if s.GoTypeAttr != "" {
			goAttr = fmt.Sprintf(" goType=\"%s\"", s.GoTypeAttr)
		}
		tag := "result"
		if s.Column.IsPrimaryKey {
			tag = "id"
		}
		fmt.Fprintf(&b, "        <%s property=\"%s\" column=\"%s\"%s/>\n",
			tag, s.Column.Name, s.Column.Name, goAttr)
	}
	b.WriteString("    </resultMap>\n\n")

	fmt.Fprintf(&b, "    <select id=\"findById\" resultMap=\"baseMap\">\n"+
		"        SELECT * FROM %s WHERE %s = #{%s}\n    </select>\n\n", table, pk, pk)
	fmt.Fprintf(&b, "    <select id=\"findAll\" resultMap=\"baseMap\">\n"+
		"        SELECT * FROM %s\n    </select>\n\n", table)

	cols := make([]string, 0, len(specs))
	fields := make([]string, 0, len(specs))
	batchFields := make([]string, 0, len(specs))
	for _, s := range specs {
		cols = append(cols, s.Column.Name)
		fields = append(fields, fmt.Sprintf("#{%s}", s.Column.Name))
		batchFields = append(batchFields, fmt.Sprintf("#{u.%s}", s.Column.Name))
	}
	fmt.Fprintf(&b, "    <insert id=\"insert\">\n        INSERT INTO %s (%s) VALUES (%s)\n    </insert>\n\n",
		table, strings.Join(cols, ", "), strings.Join(fields, ", "))
	fmt.Fprintf(&b, "    <insert id=\"insertBatch\">\n        INSERT INTO %s (%s) VALUES\n"+
		"        <foreach collection=\"list\" item=\"u\" separator=\",\">(%s)</foreach>\n    </insert>\n\n",
		table, strings.Join(cols, ", "), strings.Join(batchFields, ", "))

	sets := make([]string, 0, len(specs))
	for _, s := range specs {
		if !s.Column.IsPrimaryKey {
			sets = append(sets, fmt.Sprintf("%s = #{%s}", s.Column.Name, s.Column.Name))
		}
	}
	fmt.Fprintf(&b, "    <update id=\"updateById\">\n        UPDATE %s SET %s WHERE %s = #{%s}\n    </update>\n\n",
		table, strings.Join(sets, ", "), pk, pk)
	fmt.Fprintf(&b, "    <delete id=\"deleteById\">\n        DELETE FROM %s WHERE %s = #{%s}\n    </delete>\n",
		table, pk, pk)
	b.WriteString("</mapper>\n")
	return b.String()
}
