package higomapper

// 实体生命周期（保守方案）。
//
// 提供两类自动行为，均不改写 SQL（零歧义风险）：
//
//   - 自动时间戳：session 层在 insert/update/批量写路径上，对参数表缺失的
//     created_at / updated_at 键自动填充当前时间（已存在的键不动——调用方显式
//     传值永远优先）。XML 侧照常书写 #{created_at} / #{updated_at} 列。
//   - 逻辑删除：配置列名与标记值后，SqlSession.QueryBuilder 创建的构造器自动
//     预置 is_delete = 0 过滤；XML 语句由作者显式写条件（保守方案不注入 SQL）。
//
// # 配置（higo-mapper.toml）
//
//	[settings.auto_timestamps]
//	enabled = true
//	format = "datetime"              # datetime | millis | seconds
//	created_at_column = "created_at" # 列名可按库表习惯自定义
//	updated_at_column = "updated_at"
//
//	[settings.logical_delete]
//	enabled = true
//	column = "is_delete"
//	normal_value = 0
//	deleted_value = 1
//
// # 乐观锁（文档化模式，非 API）
//
// XML 显式书写版本条件即可获得乐观锁语义（受影响行数 = 0 即冲突）：
//
//	<update id="updateWithVersion">
//	    UPDATE users SET age = #{age}, version = version + 1
//	    WHERE id = #{id} AND version = #{version}
//	</update>
//
// 调用方检查 session.Update(...) 返回值：0 表示版本冲突，需重试或报错。

import (
	"strings"
	"time"
)

// TimestampFormat 时间戳填充格式
type TimestampFormat int

const (
	// FormatDatetime YYYY-MM-DD HH:MM:SS（UTC，适配 DATETIME 列）
	FormatDatetime TimestampFormat = iota
	// FormatMillis Unix 毫秒（int64，适配 INTEGER 列）
	FormatMillis
	// FormatSeconds Unix 秒（int64，适配 INTEGER 列）
	FormatSeconds
)

// ParseTimestampFormat 从配置字符串解析时间戳格式（大小写不敏感）；未知返回 false
func ParseTimestampFormat(s string) (TimestampFormat, bool) {
	switch lowerTrim(s) {
	case "datetime":
		return FormatDatetime, true
	case "millis":
		return FormatMillis, true
	case "seconds":
		return FormatSeconds, true
	}
	return FormatDatetime, false
}

func lowerTrim(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// AutoTimestampsConfig 自动时间戳配置（[settings.auto_timestamps]）
type AutoTimestampsConfig struct {
	// Enabled 总开关（默认 false：不填充，既有行为零变化）
	Enabled bool
	// Format 填充格式（默认 datetime 字符串）
	Format TimestampFormat
	// CreatedAtColumn 创建时间列名（默认 created_at）
	CreatedAtColumn string
	// UpdatedAtColumn 更新时间列名（默认 updated_at）
	UpdatedAtColumn string
}

// DefaultAutoTimestampsConfig 默认自动时间戳配置（关闭状态 + 默认列名）
func DefaultAutoTimestampsConfig() AutoTimestampsConfig {
	return AutoTimestampsConfig{
		Enabled:         false,
		Format:          FormatDatetime,
		CreatedAtColumn: "created_at",
		UpdatedAtColumn: "updated_at",
	}
}

// LogicalDeleteConfig 逻辑删除配置（[settings.logical_delete]）
type LogicalDeleteConfig struct {
	// Enabled 总开关（默认 false）
	Enabled bool
	// Column 标记列名（默认 is_delete）
	Column string
	// NormalValue 未删除标记值（默认 0）
	NormalValue int64
	// DeletedValue 已删除标记值（默认 1）
	DeletedValue int64
}

// DefaultLogicalDeleteConfig 默认逻辑删除配置（关闭状态 + 默认列名/标记值）
func DefaultLogicalDeleteConfig() LogicalDeleteConfig {
	return LogicalDeleteConfig{
		Enabled:      false,
		Column:       "is_delete",
		NormalValue:  0,
		DeletedValue: 1,
	}
}

// LifecycleConfig 实体生命周期综合配置（session 持有，执行路径使用）
type LifecycleConfig struct {
	// AutoTimestamps 自动时间戳
	AutoTimestamps AutoTimestampsConfig
	// LogicalDelete 逻辑删除
	LogicalDelete LogicalDeleteConfig
}

// DefaultLifecycleConfig 全关闭的默认配置
func DefaultLifecycleConfig() LifecycleConfig {
	return LifecycleConfig{
		AutoTimestamps: DefaultAutoTimestampsConfig(),
		LogicalDelete:  DefaultLogicalDeleteConfig(),
	}
}

// AnyEnabled 是否有任何生命周期行为启用（全关时执行路径零开销）
func (c *LifecycleConfig) AnyEnabled() bool {
	return c.AutoTimestamps.Enabled || c.LogicalDelete.Enabled
}

// nowValue 当前时间按配置格式生成填充值
func (c *LifecycleConfig) nowValue() any {
	now := time.Now().UTC()
	switch c.AutoTimestamps.Format {
	case FormatMillis:
		return now.UnixMilli()
	case FormatSeconds:
		return now.Unix()
	default:
		return FormatUTCDatetime(uint64(now.Unix()))
	}
}

// ApplyTimestamps 对参数表应用自动时间戳（仅填充缺失键）。
//
// created 为真填充 created_at + updated_at（insert 语义），否则仅 updated_at
// （update 语义）。总开关关闭时原样返回。
func (c *LifecycleConfig) ApplyTimestamps(params map[string]any, created bool) {
	cfg := &c.AutoTimestamps
	if !cfg.Enabled {
		return
	}
	now := c.nowValue()
	if created {
		if _, ok := params[cfg.CreatedAtColumn]; !ok {
			params[cfg.CreatedAtColumn] = now
		}
	}
	if _, ok := params[cfg.UpdatedAtColumn]; !ok {
		params[cfg.UpdatedAtColumn] = now
	}
}

// ApplyTimestampsToValues 对批量实体值（[]any，元素须为对象）应用自动时间戳。
//
// 非对象元素（标量实体）静默跳过——其参数不经由列名引用，无处可填。
func (c *LifecycleConfig) ApplyTimestampsToValues(values []any, created bool) {
	if !c.AutoTimestamps.Enabled {
		return
	}
	for i, v := range values {
		if m, ok := v.(map[string]any); ok {
			c.ApplyTimestamps(m, created)
			values[i] = m
		}
	}
}

// FormatUTCDatetime UTC 日期时间格式化（YYYY-MM-DD HH:MM:SS，精度为秒）
func FormatUTCDatetime(secs uint64) string {
	return time.Unix(int64(secs), 0).UTC().Format("2006-01-02 15:04:05")
}
