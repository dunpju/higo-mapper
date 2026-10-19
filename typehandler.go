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
	"fmt"
	"strconv"
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
	case "double", "Double", "float", "Float", "decimal", "REAL":
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
