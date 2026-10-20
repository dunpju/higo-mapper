package higomapper

// 结果集处理器。
//
// 将 database/sql 查询行映射为通用中间表示（map[string]any，列为键），
// 再经 JSON 往返反序列化为目标类型 T。
//
// 列值按驱动实际返回类型规范化：
//
//	nil        → nil
//	[]byte     → string（UTF-8 文本）
//	其余       → 原样（bool / int64 / float64 / string / time.Time）

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/dunpju/higo-mapper/core"
)

// ResultSetHandler 结果集处理器：行 → map[string]any → T
type ResultSetHandler struct {
	// TypeHandlers 类型处理器注册表（ResultMap 声明 goType 时按列应用）
	TypeHandlers *TypeHandlerRegistry
}

// NewResultSetHandler 创建带默认类型处理器的 ResultSetHandler
func NewResultSetHandler() *ResultSetHandler {
	return &ResultSetHandler{TypeHandlers: DefaultTypeHandlerRegistry()}
}

// WithHandlers 使用指定的类型处理器注册表
func (h *ResultSetHandler) WithHandlers(r *TypeHandlerRegistry) *ResultSetHandler {
	h.TypeHandlers = r
	return h
}

// normalizeColumn 驱动列值 → 通用中间表示
func normalizeColumn(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case float32:
		return float64(t)
	case bool, int64, float64, string:
		return t
	}
	return v
}

// scanRow 将当前行扫描为 map[string]any（列名 → 规范化值）
func scanRow(rows *sql.Rows, cols []string) (map[string]any, error) {
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, newResultMappingError(err, "扫描行失败")
	}
	row := make(map[string]any, len(cols))
	for i, c := range cols {
		row[c] = normalizeColumn(values[i])
	}
	return row, nil
}

// RowsToMaps 物化整个结果集为 []map[string]any（调用方负责 rows.Close）
func RowsToMaps(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, newResultMappingError(err, "读取列信息失败")
	}
	var out []map[string]any
	for rows.Next() {
		row, err := scanRow(rows, cols)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, newResultMappingError(err, "遍历结果集失败")
	}
	return out, nil
}

// RowToMap 单行 → map（用于列名与字段名不一致的场景，调用方可自行构造对象）
func RowToMap(rows *sql.Rows) (map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, newResultMappingError(err, "读取列信息失败")
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, newResultMappingError(err, "遍历结果集失败")
		}
		return nil, nil
	}
	return scanRow(rows, cols)
}

// MapRow 将一行（map）反序列化为目标类型 T（JSON 往返）
func MapRow[T any](row map[string]any) (T, error) {
	var zero T
	b, err := json.Marshal(row)
	if err != nil {
		return zero, newResultMappingError(err, "序列化行失败")
	}
	var t T
	if err := json.Unmarshal(b, &t); err != nil {
		return zero, newResultMappingError(err, "反序列化行失败: %v", err)
	}
	return t, nil
}

// MapRows 将多行反序列化为 []T
func MapRows[T any](rows []map[string]any) ([]T, error) {
	out := make([]T, 0, len(rows))
	for _, row := range rows {
		t, err := MapRow[T](row)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// ─── ResultMap 嵌套映射（association 一对一 + collection 一对多）─────

// columnValue 按列映射读取单格值：
// 列不存在或值为 null → nil；声明了 goType 且有对应处理器时做类型转换。
func columnValue(row map[string]any, col core.ResultColumn, handlers *TypeHandlerRegistry) any {
	v, ok := row[col.Column]
	if !ok || v == nil {
		return nil
	}
	if col.GoType != "" && handlers != nil {
		if h, found := handlers.Lookup(col.GoType); found {
			if nv, err := h.GetResult(v); err == nil {
				return nv
			}
		}
	}
	return v
}

// nestedValue 嵌套映射中按列名裸读（列不存在返回 nil）
func nestedValue(row map[string]any, column string) any {
	if v, ok := row[column]; ok {
		return v
	}
	return nil
}

// buildNestedObject 构建嵌套对象（association / collection 子项共用）。
// 若所有结果列为 null，返回 nil（表示无关联对象）。
func buildNestedObject(row map[string]any, mapping *core.NestedMapping) map[string]any {
	obj := make(map[string]any, len(mapping.ResultColumns))
	anyNonNull := false
	for _, col := range mapping.ResultColumns {
		v := nestedValue(row, col.Column)
		if v != nil {
			anyNonNull = true
		}
		obj[col.Property] = v
	}
	if anyNonNull {
		return obj
	}
	return nil
}

// buildParentObject 构建单个父对象（含顶层列 + association + collection 首元素）。
//
// idValues 为调用方已读取的 <id> 列值（按 result_columns 中 is_id 出现顺序），
// 传入后避免重复读取；为空或耗尽时回退为按列名读取（供单行路径复用）。
func buildParentObject(
	row map[string]any,
	resultMap *core.ResultMap,
	handlers *TypeHandlerRegistry,
	idValues []any,
) map[string]any {
	obj := make(map[string]any,
		len(resultMap.ResultColumns)+len(resultMap.Associations)+len(resultMap.Collections))
	idIdx := 0

	// 顶层 id/result 列（id 列复用预读取值）
	for _, col := range resultMap.ResultColumns {
		var v any
		if col.IsID && idIdx < len(idValues) {
			v = idValues[idIdx]
			idIdx++
		} else {
			v = columnValue(row, col, handlers)
		}
		obj[col.Property] = v
	}

	// association（一对一）：列为空则 null
	for _, assoc := range resultMap.Associations {
		obj[assoc.Property] = buildNestedObject(row, &assoc)
	}

	// collection（一对多）：首行放入数组，后续行追加
	for i := range resultMap.Collections {
		coll := &resultMap.Collections[i]
		child := buildNestedObject(row, coll)
		if child == nil {
			obj[coll.Property] = []any{}
		} else {
			obj[coll.Property] = []any{child}
		}
	}

	return obj
}

// idKey 分组键的单元格文本形式（与 Rust 版 Value::to_string 语义对齐：
// null → null；字符串加引号转义；其余按默认格式）
func idKey(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return fmt.Sprintf("%v", v)
}

// groupRowsWithResultMap 将扁平 join 行按 <id> 列分组为父对象列表
// （association 一对一 + collection 一对多）。
//
//   - <id> 列决定父对象分组：相同 id 的行合并，collection 追加子项
//   - association：从扁平 join 行的列构建嵌套对象（列为空 → null）
//   - collection：按父 id 分组，每行贡献一个子项
//   - 无 <id> 列时每行独立成父对象
//
// 该中间表示同时服务于：select_one/select_list 的 T 映射与 L2 查询缓存
// （缓存存分组后的父对象，同一份数据可服务不同目标类型）。
func groupRowsWithResultMap(
	rows []map[string]any,
	resultMap *core.ResultMap,
	handlers *TypeHandlerRegistry,
) []map[string]any {
	if len(rows) == 0 {
		return nil
	}

	var idCols []string
	for _, c := range resultMap.ResultColumns {
		if c.IsID {
			idCols = append(idCols, c.Column)
		}
	}

	// 上界 = 行数（每行至多产生一个父对象）；分组键 → 父对象下标
	parents := make([]map[string]any, 0, len(rows))
	keyIndex := make(map[string]int, len(rows))

	for rowIdx, row := range rows {
		// id 列每行只读取一次：分组键与父对象构建共用
		idValues := make([]any, len(idCols))
		for i, c := range idCols {
			idValues[i] = nestedValue(row, c)
		}
		var key string
		if len(idCols) == 0 {
			key = fmt.Sprintf("__row_%d", rowIdx)
		} else if len(idValues) == 1 {
			key = idKey(idValues[0]) // 常见单 id 列：免 join
		} else {
			// 单元分隔符拼接，避免值内逗号冲突
			parts := make([]string, len(idValues))
			for i, v := range idValues {
				parts[i] = idKey(v)
			}
			key = strings.Join(parts, "\x1f")
		}

		if idx, exists := keyIndex[key]; exists {
			// 已存在父：仅追加 collection 子项
			for i := range resultMap.Collections {
				coll := &resultMap.Collections[i]
				child := buildNestedObject(row, coll)
				if child == nil {
					continue
				}
				if arr, ok := parents[idx][coll.Property].([]any); ok {
					parents[idx][coll.Property] = append(arr, child)
				}
			}
		} else {
			keyIndex[key] = len(parents)
			parents = append(parents, buildParentObject(row, resultMap, handlers, idValues))
		}
	}
	return parents
}

// MapRowsWithResultMap 使用 ResultMap 将多行映射为 []T
// （支持 association 一对一 + collection 一对多分组）。
func MapRowsWithResultMap[T any](
	rows []map[string]any,
	resultMap *core.ResultMap,
	handlers *TypeHandlerRegistry,
) ([]T, error) {
	parents := groupRowsWithResultMap(rows, resultMap, handlers)
	if len(parents) == 0 {
		return nil, nil
	}
	out := make([]T, 0, len(parents))
	for _, p := range parents {
		t, err := MapRow[T](p)
		if err != nil {
			return nil, newResultMappingError(err, "ResultMap 反序列化失败")
		}
		out = append(out, t)
	}
	return out, nil
}

// MapRowWithResultMap 使用 ResultMap 映射单行（select_one 路径；多于一个父对象报错）。
//
// v0.4 语义：先分组再计数——join 展开产生的多行属于同一父对象时不算
// TooManyRows，只有分组后仍多于 1 个父对象才报错。
func MapRowWithResultMap[T any](
	rows []map[string]any,
	resultMap *core.ResultMap,
	handlers *TypeHandlerRegistry,
) (*T, error) {
	parents := groupRowsWithResultMap(rows, resultMap, handlers)
	switch len(parents) {
	case 0:
		return nil, nil
	case 1:
		t, err := MapRow[T](parents[0])
		if err != nil {
			return nil, newResultMappingError(err, "ResultMap 反序列化失败")
		}
		return &t, nil
	default:
		return nil, TooManyRowsError(len(parents))
	}
}
