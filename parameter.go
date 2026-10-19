package higomapper

// 参数处理器。
//
// 负责将 BoundSql 的参数列表（通用中间表示）转换为 database/sql 驱动
// 可接受的参数值，并在执行前校验占位符数量一致。
//
// 中间表示 → 驱动参数的映射：
//
//	nil            → nil（SQL NULL）
//	bool           → bool
//	int64          → int64
//	float64        → float64
//	string         → string
//	time.Time      → time.Time 透传
//	[]any/map[string]any → JSON 字符串（保持信息，可在结果映射时反序列化）

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/dunpju/higo-mapper/core"
)

// NormalizeValue 递归规范化任意 Go 值为通用中间表示：
//
//	int/int8..int32/uint 系 → int64
//	float32 → float64
//	json.Number → int64（整型文本）或 float64
//	数组/切片 → []any（逐元素规范化）
//	结构体/映射 → map[string]any（经 JSON 往返，字段名取 json tag）
//	time.Time 透传；其余基本类型原样返回
//
// 已是规范形态（nil/bool/int64/float64/string/[]any/map[string]any）时快速返回。
func NormalizeValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case bool, int64, float64, string, time.Time:
		return t
	case int:
		return int64(t)
	case int8:
		return int64(t)
	case int16:
		return int64(t)
	case int32:
		return int64(t)
	case uint:
		return int64(t)
	case uint8:
		return int64(t)
	case uint16:
		return int64(t)
	case uint32:
		return int64(t)
	case uint64:
		return int64(t)
	case float32:
		return float64(t)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case []byte:
		return string(t)
	}
	// 容器/结构体：JSON 往返（UseNumber 保持数字精度语义）
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.Struct:
		b, err := marshalUseNumber(v)
		if err != nil {
			return v // 序列化失败时透传，由驱动层兜底
		}
		var out any
		if err := unmarshalUseNumber(b, &out); err != nil {
			return v
		}
		return out
	}
	return v
}

// marshalUseNumber JSON 序列化（数字保持精度语义由解码侧 UseNumber 保证）
func marshalUseNumber(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// unmarshalUseNumber JSON 反序列化（数字解码为 json.Number 后规范化）
func unmarshalUseNumber(b []byte, out *any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return err
	}
	*out = normalizeJSON(*out)
	return nil
}

// normalizeJSON 递归把 json.Number 转为 int64/float64
func normalizeJSON(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case []any:
		for i := range t {
			t[i] = normalizeJSON(t[i])
		}
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeJSON(val)
		}
		return t
	}
	return v
}

// BindValue 单个中间表示值 → 驱动参数。
//
// 数组/对象序列化为 JSON 字符串（保持信息，可在结果映射时反序列化）。
func BindValue(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case bool, int64, float64, string, time.Time:
		return t, nil
	case []any, map[string]any:
		b, err := json.Marshal(t)
		if err != nil {
			return nil, newSerializationError(err, "绑定参数 %v 失败", t)
		}
		return string(b), nil
	}
	// 兜底：规范化后再试一次
	n := NormalizeValue(value)
	switch t := n.(type) {
	case bool, int64, float64, string, time.Time, nil:
		return t, nil
	case []any, map[string]any:
		b, err := json.Marshal(t)
		if err != nil {
			return nil, newSerializationError(err, "绑定参数 %v 失败", t)
		}
		return string(b), nil
	}
	return nil, newSerializationError(nil, "无法绑定参数类型 %T", value)
}

// BindArguments 将 BoundSql 的所有参数转换为驱动参数列表（与 ? 一一对应）。
func BindArguments(bound *core.BoundSql) ([]any, error) {
	args := make([]any, 0, len(bound.Parameters))
	for _, v := range bound.Parameters {
		arg, err := BindValue(v)
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
	}
	return args, nil
}

// ValidatePlaceholderCount 验证 BoundSql 的参数数量与 SQL 中 ? 占位符数量一致。
//
// 不一致通常意味着参数绑定会错位（缺失参数产生 MISSING 标记而非 ?）。
func ValidatePlaceholderCount(bound *core.BoundSql) error {
	placeholderCount := strings.Count(bound.SQL, "?")
	if placeholderCount != len(bound.Parameters) {
		return newSerializationError(nil,
			"参数数量不匹配: SQL 含 %d 个 ? 占位符, 但提供 %d 个参数（可能存在缺失参数）",
			placeholderCount, len(bound.Parameters))
	}
	return nil
}

// formatValue 用于日志/错误信息的值描述
func formatValue(v any) string {
	return fmt.Sprintf("%v", v)
}
