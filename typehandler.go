package higomapper

// 类型处理器模块。
//
// 在通用中间表示（nil / bool / int64 / float64 / string / []any / map[string]any）
// 与数据库列值之间双向转换。每个处理器负责一种逻辑类型。
//
// 「参数写入方向」的通用入口是 ParameterHandler（按中间表示分派），
// 而 TypeHandler 主要在「结果读取方向」发挥作用——尤其当 ResultMap
// 声明了特定 goType 时，通过 TypeHandlerRegistry 查找对应处理器。

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypeHandler 类型处理器接口
type TypeHandler interface {
	// TypeName 处理器标识的类型名（如 "i64"、"string"），用于注册表查找
	TypeName() string
	// GetResult 从数据库行扫描出的列值（driver 原始值）转换为通用中间表示
	GetResult(value any) (any, error)
	// SetParameter 将通用中间表示转换为驱动可接受的参数值
	SetParameter(value any) (any, error)
}

// ─── 标准实现 ─────────────────────────────────────────────────────

// Int64Handler i64 类型处理器（覆盖 SmallInt / Integer / BigInt）
type Int64Handler struct{}

// TypeName 实现 TypeHandler
func (Int64Handler) TypeName() string { return "i64" }

// GetResult 实现 TypeHandler：数值/数值串 → int64
func (Int64Handler) GetResult(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case float64:
		return int64(t), nil
	case bool:
		if t {
			return int64(1), nil
		}
		return int64(0), nil
	case string:
		if t == "" {
			return nil, nil
		}
		i, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return nil, newResultMappingError(err, "读取 i64 列 '%s' 失败", t)
		}
		return i, nil
	}
	return nil, newResultMappingError(nil, "i64 处理器无法读取 %T", value)
}

// SetParameter 实现 TypeHandler
func (Int64Handler) SetParameter(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case int64:
		return t, nil
	case float64:
		return int64(t), nil
	case bool:
		if t {
			return int64(1), nil
		}
		return int64(0), nil
	}
	return nil, newResultMappingError(nil, "i64 处理器无法绑定 %T（仅接受数字或 null）", value)
}

// Int32Handler i32 类型处理器
type Int32Handler struct{}

// TypeName 实现 TypeHandler
func (Int32Handler) TypeName() string { return "i32" }

// GetResult 实现 TypeHandler：数值 → int64（窄化为 int32 语义）
func (Int32Handler) GetResult(value any) (any, error) {
	v, err := Int64Handler{}.GetResult(value)
	if err != nil {
		return nil, err
	}
	if i, ok := v.(int64); ok {
		return int64(int32(i)), nil
	}
	return v, nil
}

// SetParameter 实现 TypeHandler
func (Int32Handler) SetParameter(value any) (any, error) {
	v, err := Int64Handler{}.SetParameter(value)
	if err != nil {
		return nil, err
	}
	if i, ok := v.(int64); ok {
		return int64(int32(i)), nil
	}
	return v, nil
}

// Float64Handler f64 类型处理器（浮点列）
type Float64Handler struct{}

// TypeName 实现 TypeHandler
func (Float64Handler) TypeName() string { return "f64" }

// GetResult 实现 TypeHandler：数值/数值串 → float64
func (Float64Handler) GetResult(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case float64:
		return t, nil
	case int64:
		return float64(t), nil
	case string:
		if t == "" {
			return nil, nil
		}
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return nil, newResultMappingError(err, "读取 f64 列 '%s' 失败", t)
		}
		return f, nil
	}
	return nil, newResultMappingError(nil, "f64 处理器无法读取 %T", value)
}

// SetParameter 实现 TypeHandler
func (Float64Handler) SetParameter(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case float64:
		return t, nil
	case int64:
		return float64(t), nil
	}
	return nil, newResultMappingError(nil, "f64 处理器无法绑定 %T（仅接受数字或 null）", value)
}

// BoolHandler bool 类型处理器（0/1 整数存储）
type BoolHandler struct{}

// TypeName 实现 TypeHandler
func (BoolHandler) TypeName() string { return "bool" }

// GetResult 实现 TypeHandler：0/1/布尔/文本 → bool
func (BoolHandler) GetResult(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case bool:
		return t, nil
	case int64:
		return t != 0, nil
	case float64:
		return t != 0, nil
	case string:
		if t == "" {
			return nil, nil
		}
		b, err := strconv.ParseBool(t)
		if err != nil {
			return nil, newResultMappingError(err, "读取 bool 列 '%s' 失败", t)
		}
		return b, nil
	}
	return nil, newResultMappingError(nil, "bool 处理器无法读取 %T", value)
}

// SetParameter 实现 TypeHandler
func (BoolHandler) SetParameter(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case bool:
		return t, nil
	case int64:
		return t != 0, nil
	case float64:
		return t != 0, nil
	}
	return nil, newResultMappingError(nil, "bool 处理器无法绑定 %T（仅接受布尔或 null）", value)
}

// StringHandler string 类型处理器
type StringHandler struct{}

// TypeName 实现 TypeHandler
func (StringHandler) TypeName() string { return "string" }

// GetResult 实现 TypeHandler：任意标量 → 字本
func (StringHandler) GetResult(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	case bool:
		return strconv.FormatBool(t), nil
	}
	return nil, newResultMappingError(nil, "string 处理器无法读取 %T", value)
}

// SetParameter 实现 TypeHandler
func (StringHandler) SetParameter(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case string:
		return t, nil
	case int64, float64, bool:
		v, err := StringHandler{}.GetResult(t)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, newResultMappingError(nil, "string 处理器无法绑定 %T", value)
}

// TimeHandler time.Time 类型处理器（时间戳/日期时间文本）
type TimeHandler struct{}

// TypeName 实现 TypeHandler
func (TimeHandler) TypeName() string { return "time" }

// 时间文本候选格式（按序尝试）
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
}

// GetResult 实现 TypeHandler：time.Time 透传，文本按常见格式解析
func (TimeHandler) GetResult(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case time.Time:
		return t, nil
	case string:
		if t == "" {
			return nil, nil
		}
		for _, layout := range timeLayouts {
			if ts, err := time.ParseInLocation(layout, t, time.Local); err == nil {
				return ts, nil
			}
		}
		return nil, newResultMappingError(nil, "time 处理器无法解析 '%s'", t)
	case int64:
		return time.Unix(t, 0), nil
	}
	return nil, newResultMappingError(nil, "time 处理器无法读取 %T", value)
}

// SetParameter 实现 TypeHandler：time.Time 透传，其余先经 Int64/串处理
func (TimeHandler) SetParameter(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case time.Time:
		return t, nil
	case string:
		return t, nil
	}
	return nil, newResultMappingError(nil, "time 处理器无法绑定 %T", value)
}

// ─── JSON / Decimal（v0.4 扩展）────────────────────────────────────

// JsonHandler json 类型处理器：JSON 列 ↔ 通用中间表示（map/slice/标量）。
//
// 读取方向：文本严格解析为 JSON（"null" → nil，非法 JSON 报错，不静默降级）。
// 绑定方向：字符串按已成形 JSON 原样透传（如 "null" / '{"a":1}'），其余值
// （map/slice/数字/布尔）序列化为 JSON 文本。
//
// PostgreSQL 注意：pgx 的 json/jsonb 列在纯文本协议下可能返回非文本类型，
// 建议 SQL 侧 CAST（SELECT col::text … / CAST(? AS jsonb)）保证文本往返。
type JsonHandler struct{}

// TypeName 实现 TypeHandler
func (JsonHandler) TypeName() string { return "json" }

// GetResult 实现 TypeHandler：文本 → 严格 JSON 解析（保持数字精度语义）
func (JsonHandler) GetResult(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case string:
		return parseStrictJSON(t)
	case []byte:
		return parseStrictJSON(string(t))
	case map[string]any, []any: // 驱动已解码（如 lib/pq json 原生）
		return t, nil
	}
	return nil, newResultMappingError(nil, "json 处理器无法读取 %T", value)
}

// SetParameter 实现 TypeHandler：字符串透传 / 其余序列化 / null → NULL
func (JsonHandler) SetParameter(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	}
	return marshalJSONString(value)
}

// parseStrictJSON 严格 JSON 解析（UseNumber 保持数字精度语义）
func parseStrictJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, newResultMappingError(err, "非法 JSON 文本: %s", truncateForError(s))
	}
	// 尾随内容（如 "{}}")同样非法
	if dec.More() {
		return nil, newResultMappingError(nil, "JSON 文本含多余内容: %s", truncateForError(s))
	}
	return normalizeJSON(v), nil
}

// marshalJSONString 序列化为 JSON 文本（保持数字精度语义）
func marshalJSONString(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", newSerializationError(err, "JSON 序列化失败: %v", value)
	}
	return string(b), nil
}

// DecimalHandler decimal 类型处理器：金额/精确小数以规范字符串承载。
//
// 读取方向按列值类型分派：文本严格校验后透传（规范字符串）、整数转文本、
// 浮点取最短十进制表示。绑定方向：字符串须通过严格校验（拒绝 "1.2.3"、
// "abc"、"1e5" 等），数字转换为文本，null 透传，其余报错。
//
// 以字符串而非 float64 承载可避免二进制浮点精度丢失（0.1 + 0.2 ≠ 0.3）。
type DecimalHandler struct{}

// TypeName 实现 TypeHandler
func (DecimalHandler) TypeName() string { return "decimal" }

// GetResult 实现 TypeHandler：文本严格校验 / 数值转规范字符串
func (DecimalHandler) GetResult(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case string:
		if !isValidDecimal(t) {
			return nil, newResultMappingError(nil, "非法 decimal 文本 '%s'", truncateForError(t))
		}
		return t, nil
	case []byte:
		s := string(t)
		if !isValidDecimal(s) {
			return nil, newResultMappingError(nil, "非法 decimal 文本 '%s'", truncateForError(s))
		}
		return s, nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	}
	return nil, newResultMappingError(nil, "decimal 处理器无法读取 %T", value)
}

// SetParameter 实现 TypeHandler
func (DecimalHandler) SetParameter(value any) (any, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case string:
		if !isValidDecimal(t) {
			return nil, newSerializationError(nil, "非法 decimal 参数 '%s'（须为纯数字，如 \"12.34\"）", t)
		}
		return t, nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	}
	return nil, newSerializationError(nil, "decimal 处理器无法绑定 %T", value)
}

// isValidDecimal 十进制字面量严格校验：[+-]?(\d+(\.\d*)?|\.\d+)，
// 不允许空白、指数、下划线、多小数点。
func isValidDecimal(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[i] == '+' || s[i] == '-' {
		i++
	}
	digits, dot := 0, false
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digits > 0
}

// ─── 注册表 ───────────────────────────────────────────────────────

// TypeHandlerRegistry 类型处理器注册表：按类型名查找处理器
type TypeHandlerRegistry struct {
	mu       sync.RWMutex
	handlers map[string]TypeHandler
}

// NewTypeHandlerRegistry 创建含全部标准处理器的注册表
func NewTypeHandlerRegistry() *TypeHandlerRegistry {
	r := &TypeHandlerRegistry{handlers: map[string]TypeHandler{}}
	r.Register(Int64Handler{})
	r.Register(Int32Handler{})
	r.Register(Float64Handler{})
	r.Register(BoolHandler{})
	r.Register(StringHandler{})
	r.Register(TimeHandler{})
	r.Register(JsonHandler{})
	r.Register(DecimalHandler{})
	return r
}

// defaultTypeHandlerRegistry 包级默认注册表（含标准处理器）
var defaultTypeHandlerRegistry = NewTypeHandlerRegistry()

// DefaultTypeHandlerRegistry 取包级默认注册表
func DefaultTypeHandlerRegistry() *TypeHandlerRegistry { return defaultTypeHandlerRegistry }

// Register 注册/替换一个处理器（按其 TypeName 键）
func (r *TypeHandlerRegistry) Register(h TypeHandler) {
	r.mu.Lock()
	r.handlers[h.TypeName()] = h
	r.mu.Unlock()
}

// Lookup 按类型名查找处理器；支持若干别名（javaType 风格短名）
func (r *TypeHandlerRegistry) Lookup(typeName string) (TypeHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if h, ok := r.handlers[typeName]; ok {
		return h, true
	}
	// 常见别名 → 标准名
	switch typeName {
	case "long", "Long", "bigint", "BIGINT", "INT", "Integer", "integer":
		h, ok := r.handlers["i64"]
		return h, ok
	case "int", "Int":
		h, ok := r.handlers["i32"]
		return h, ok
	case "double", "Double", "float", "Float", "REAL":
		h, ok := r.handlers["f64"]
		return h, ok
	case "Boolean", "TINYINT", "BIT":
		h, ok := r.handlers["bool"]
		return h, ok
	case "String", "VARCHAR", "TEXT", "varchar", "text":
		h, ok := r.handlers["string"]
		return h, ok
	case "DateTime", "datetime", "timestamp", "TIMESTAMP", "DATE", "date":
		h, ok := r.handlers["time"]
		return h, ok
	}
	return nil, false
}

// Names 已注册的类型名列表
func (r *TypeHandlerRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		names = append(names, k)
	}
	return names
}

// String 注册表的简要描述
func (r *TypeHandlerRegistry) String() string {
	return fmt.Sprintf("TypeHandlerRegistry(%d)", len(r.Names()))
}
