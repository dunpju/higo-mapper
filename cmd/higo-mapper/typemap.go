// 数据库列类型 → Go 类型映射。
//
// 仅映射到 higo-mapper 运行时内建支持的类型（int/int64/float64/bool/string/any），
// 并在适当列上附加 goType="json|decimal" 属性提示。未覆盖的类型保守回退为
// string（注释说明可手工替换）。

package main

import "strings"

// GoType 单列的 Go 类型映射结果
type GoType struct {
	// Base Go 类型名（不含指针包装）
	Base string
	// GoTypeAttr 非空时在 XML resultMap 上附加 goType="…"
	GoTypeAttr string
	// Note 生成注释（说明保守映射，可手工调整）
	Note string
}

// mysqlType MySQL DATA_TYPE（information_schema 的小写规范名）→ Go 类型
func mysqlType(dataType, columnType string) GoType {
	// tinyint(1) 惯用作布尔
	if dataType == "tinyint" && strings.HasPrefix(columnType, "tinyint(1)") {
		return GoType{Base: "bool"}
	}
	switch dataType {
	case "tinyint", "smallint", "year", "int", "mediumint":
		return GoType{Base: "int"}
	case "bigint":
		return GoType{Base: "int64"}
	case "float", "double":
		return GoType{Base: "float64"}
	case "decimal", "numeric":
		return GoType{
			Base:       "string",
			GoTypeAttr: "decimal",
			Note:       "金额列：以字符串承载避免浮点误差，需要精确计算请引入 decimal 库",
		}
	case "json":
		return GoType{Base: "any", GoTypeAttr: "json"}
	case "bit":
		return GoType{Base: "bool"}
	case "datetime", "timestamp", "date", "time":
		return GoType{Base: "string", Note: "时间列映射为字符串；可按需改为 time.Time（goType=\"time\"）"}
	}
	// varchar/char/text 系列/blob 系列/enum/set 及未知类型 → string
	note := ""
	switch dataType {
	case "blob", "tinyblob", "mediumblob", "longblob", "binary", "varbinary":
		note = "二进制列按文本读取（UTF-8）；需要原始字节请手工调整"
	}
	return GoType{Base: "string", Note: note}
}

// sqliteType SQLite 类型亲和（type affinity，大写规范）→ Go 类型
func sqliteType(declType string) GoType {
	t := strings.ToUpper(strings.TrimSpace(declType))
	if t == "" {
		// 无类型列（NULL 亲和）→ 保守 string
		return GoType{Base: "string"}
	}
	if strings.Contains(t, "INT") {
		return GoType{Base: "int64"}
	}
	if strings.Contains(t, "CHAR") || strings.Contains(t, "CLOB") || strings.Contains(t, "TEXT") {
		return GoType{Base: "string"}
	}
	if strings.Contains(t, "BLOB") {
		return GoType{Base: "string", Note: "BLOB 列按文本读取（UTF-8）"}
	}
	if strings.Contains(t, "REAL") || strings.Contains(t, "FLOA") || strings.Contains(t, "DOUB") {
		return GoType{Base: "float64"}
	}
	// NUMERIC 亲和（含 decimal 声明）
	attr := ""
	if strings.Contains(t, "DEC") || strings.Contains(t, "NUM") {
		attr = "decimal"
	}
	return GoType{Base: "string", GoTypeAttr: attr, Note: "NUMERIC 亲和列保守映射为字符串"}
}
