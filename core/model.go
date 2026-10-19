// Package core 提供 MyBatis XML 动态 SQL 解析与生成能力（无数据库依赖）。
//
// 解析后的 Mapper 可通过 BuildSQL / BuildBoundSQL 按参数生成最终 SQL：
// 前者为内联模式（参数直接拼入 SQL），后者为参数化模式（#{} → ? + 参数列表）。
package core

import "fmt"

// Mapper 表示一个已解析的 MyBatis 映射文件
type Mapper struct {
	// Namespace 命名空间
	Namespace string
	// Statements 语句 ID → SQL 语句
	Statements map[string]*SqlStatement
	// ResultMaps 结果映射 ID → ResultMap
	ResultMaps map[string]*ResultMap
	// SqlFragments SQL 片段 ID → 节点序列
	SqlFragments map[string][]SqlNode
}

// NewMapper 创建空 Mapper
func NewMapper() *Mapper {
	return &Mapper{
		Statements:   make(map[string]*SqlStatement),
		ResultMaps:   make(map[string]*ResultMap),
		SqlFragments: make(map[string][]SqlNode),
	}
}

// ─── 错误类型 ─────────────────────────────────────────────────────

// ErrorKind core 层错误类别
type ErrorKind int

const (
	// ErrParse XML 解析错误
	ErrParse ErrorKind = iota
	// ErrMissingParam 参数缺失
	ErrMissingParam
	// ErrMissingFragment SQL 片段引用不存在
	ErrMissingFragment
	// ErrInvalidCondition 条件表达式无效
	ErrInvalidCondition
	// ErrStatementNotFound 语句不存在
	ErrStatementNotFound
	// ErrSqlGeneration SQL 生成错误
	ErrSqlGeneration
)

// Error core 层结构化错误
type Error struct {
	Kind ErrorKind
	// Message 已格式化的完整错误描述
	Message string
}

func (e *Error) Error() string { return e.Message }

func newParseError(format string, args ...any) *Error {
	return &Error{Kind: ErrParse, Message: "XML解析错误: " + fmt.Sprintf(format, args...)}
}

func newMissingParamError(param, context string) *Error {
	return &Error{Kind: ErrMissingParam, Message: fmt.Sprintf("参数 '%s' 不存在 (%s)", param, context)}
}

func newMissingFragmentError(refID string) *Error {
	return &Error{Kind: ErrMissingFragment, Message: fmt.Sprintf("SQL片段 '%s' 不存在", refID)}
}

func newInvalidConditionError(expr, reason string) *Error {
	return &Error{Kind: ErrInvalidCondition, Message: fmt.Sprintf("无效条件 '%s': %s", expr, reason)}
}

func newStatementNotFoundError(id string) *Error {
	return &Error{Kind: ErrStatementNotFound, Message: fmt.Sprintf("语句 '%s' 不存在", id)}
}

// NewStatementNotFound 公开构造：语句不存在错误（runtime 层复用）
func NewStatementNotFound(id string) *Error { return newStatementNotFoundError(id) }

func newSqlGenerationError(format string, args ...any) *Error {
	return &Error{Kind: ErrSqlGeneration, Message: "SQL生成错误: " + fmt.Sprintf(format, args...)}
}

// ─── 语句模型 ─────────────────────────────────────────────────────

// StatementType SQL 语句类型
type StatementType int

const (
	// StatementSelect 查询
	StatementSelect StatementType = iota
	// StatementInsert 插入
	StatementInsert
	// StatementUpdate 更新
	StatementUpdate
	// StatementDelete 删除
	StatementDelete
)

// String 语句类型名称
func (t StatementType) String() string {
	switch t {
	case StatementSelect:
		return "SELECT"
	case StatementInsert:
		return "INSERT"
	case StatementUpdate:
		return "UPDATE"
	case StatementDelete:
		return "DELETE"
	}
	return "UNKNOWN"
}

// SqlStatement SQL 语句模型
type SqlStatement struct {
	// ID 语句 ID
	ID string
	// StmtType 语句类型
	StmtType StatementType
	// ParameterType 参数类型（parameterType 属性）
	ParameterType string
	// ResultType 返回值类型（resultType 属性）
	ResultType string
	// ResultMap 结果映射 ID（resultMap 属性）
	ResultMap string
	// SQL 原始文本（仅顶层文本拼接，不含标签结构）
	SQL string
	// DynamicSQL 动态 SQL 节点根（nil 表示纯静态语句）
	DynamicSQL SqlNode
	// Parameters 语句中出现的参数名列表（#{param} 与 ${param}）
	Parameters []string
	// SelectKey 主键回填配置（仅 INSERT/UPDATE，可为 nil）
	SelectKey *SelectKey
}

// SelectKeyOrder selectKey 的执行时机
type SelectKeyOrder int

const (
	// SelectKeyAfter 在主 SQL 之后执行（默认）
	SelectKeyAfter SelectKeyOrder = iota
	// SelectKeyBefore 在主 SQL 之前执行
	SelectKeyBefore
)

// SelectKey 主键回填配置
type SelectKey struct {
	// KeyProperty 主键属性名（参数对象上的字段）
	KeyProperty string
	// ResultType 主键类型
	ResultType string
	// Order 执行时机
	Order SelectKeyOrder
	// SQL 取主键的 SQL
	SQL string
}

// ─── 结果映射模型 ─────────────────────────────────────────────────

// ResultMap 结果映射
type ResultMap struct {
	// ID 结果映射 ID
	ID string
	// TypeName 类型（type 属性）
	TypeName string
	// ResultColumns 结果列映射（含 <id> 与 <result>）
	ResultColumns []ResultColumn
	// Associations 一对一嵌套关联（<association>）
	Associations []NestedMapping
	// Collections 一对多嵌套集合（<collection>）
	Collections []NestedMapping
}

// ResultColumn 结果列映射
type ResultColumn struct {
	// Property 属性名
	Property string
	// Column 列名
	Column string
	// JavaType javaType 属性
	JavaType string
	// JdbcType jdbcType 属性
	JdbcType string
	// GoType 目标语言类型（goType / rustType 属性，二者等价）
	GoType string
	// IsID 是否为 <id>（标记身份，用于 collection 分组）
	IsID bool
}

// NestedMapping 嵌套映射（<association> / <collection> 共用）
type NestedMapping struct {
	// Property 属性名
	Property string
	// Column 外键列（用于关联判空 / 分组）
	Column string
	// NestedType association: javaType；collection: ofType
	NestedType string
	// Select 嵌套查询 ID（select 属性，延迟加载；本实现暂不支持）
	Select string
	// ResultColumns 嵌套结果列（含 <id> 与 <result>）
	ResultColumns []ResultColumn
	// Associations 更深层嵌套关联
	Associations []NestedMapping
	// Collections 更深层嵌套集合
	Collections []NestedMapping
}

// ─── 动态 SQL 节点 ────────────────────────────────────────────────

// SqlNode 动态 SQL 节点接口。
//
// 对应 Rust 版的 DynamicSqlNode 枚举，具体实现：
// TextNode / IfNode / ChooseNode / ForeachNode / TrimNode /
// BindNode / IncludeNode / WhereNode / SetNode / MixedNode。
type SqlNode interface{ sqlNode() }

// TextNode 文本节点（含 #{}/${} 占位符的原始 SQL 文本）
type TextNode struct{ Content string }

// IfNode 条件节点（<if test="...">）
type IfNode struct {
	Test     string
	Contents []SqlNode
}

// WhenNode choose 的分支（<when test="...">）
type WhenNode struct {
	Test     string
	Contents []SqlNode
}

// ChooseNode 多路分支（<choose>，按 when 顺序取首个命中，否则 otherwise）
type ChooseNode struct {
	Whens     []WhenNode
	Otherwise []SqlNode
}

// ForeachNode 集合遍历（<foreach>）
type ForeachNode struct {
	Collection string
	Item       string
	Index      string // 可为空
	Open       string
	Separator  string
	Close      string
	Contents   []SqlNode
}

// TrimNode 通用修剪（<trim>；空字符串表示属性未设置）
type TrimNode struct {
	Prefix          string
	PrefixOverrides string
	Suffix          string
	SuffixOverrides string
	Contents        []SqlNode
}

// BindNode 变量绑定（<bind name="..." value="...">）
type BindNode struct {
	Name  string
	Value string
}

// IncludeNode SQL 片段引用（<include refid="...">）
type IncludeNode struct{ RefID string }

// WhereNode WHERE 修剪（<where>；默认剥离开头的 AND |OR ）
type WhereNode struct {
	PrefixOverrides string
	SuffixOverrides string
	Contents        []SqlNode
}

// SetNode SET 修剪（<set>；默认剥离结尾的逗号）
type SetNode struct {
	PrefixOverrides string
	SuffixOverrides string
	Contents        []SqlNode
}

// MixedNode 混合内容容器：多个动态 SQL 节点的序列
type MixedNode struct{ Contents []SqlNode }

func (*TextNode) sqlNode()    {}
func (*IfNode) sqlNode()      {}
func (*ChooseNode) sqlNode()  {}
func (*ForeachNode) sqlNode() {}
func (*TrimNode) sqlNode()    {}
func (*BindNode) sqlNode()    {}
func (*IncludeNode) sqlNode() {}
func (*WhereNode) sqlNode()   {}
func (*SetNode) sqlNode()     {}
func (*MixedNode) sqlNode()   {}
