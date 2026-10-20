package higomapper

import "fmt"

// ErrorKind 运行时错误类别（对应 Rust 版 MapperError 的 11 种变体）
type ErrorKind int

const (
	// ErrConfig 配置错误
	ErrConfig ErrorKind = iota
	// ErrDatabaseConnection 数据库连接失败
	ErrDatabaseConnection
	// ErrSqlExecution SQL 执行失败
	ErrSqlExecution
	// ErrQuery 查询失败
	ErrQuery
	// ErrTransaction 事务操作失败
	ErrTransaction
	// ErrPagination 分页参数 / 分页语句错误
	ErrPagination
	// ErrSerialization 参数序列化失败
	ErrSerialization
	// ErrResultMapping 结果映射失败
	ErrResultMapping
	// ErrTooManyRows SelectOne 查到多行
	ErrTooManyRows
	// ErrMapperNotFound Mapper 不存在
	ErrMapperNotFound
	// ErrSessionClosed 会话已关闭
	ErrSessionClosed
	// ErrFileWatcher 热重载文件监视失败
	ErrFileWatcher
)

// String 错误类别名称
func (k ErrorKind) String() string {
	switch k {
	case ErrConfig:
		return "Config"
	case ErrDatabaseConnection:
		return "DatabaseConnection"
	case ErrSqlExecution:
		return "SqlExecution"
	case ErrQuery:
		return "Query"
	case ErrTransaction:
		return "Transaction"
	case ErrPagination:
		return "Pagination"
	case ErrSerialization:
		return "Serialization"
	case ErrResultMapping:
		return "ResultMapping"
	case ErrTooManyRows:
		return "TooManyRows"
	case ErrMapperNotFound:
		return "MapperNotFound"
	case ErrSessionClosed:
		return "SessionClosed"
	case ErrFileWatcher:
		return "FileWatcher"
	}
	return "Unknown"
}

// Error 运行时结构化错误（Kind + Message + 可选底层原因）
type Error struct {
	// Kind 错误类别
	Kind ErrorKind
	// Message 已格式化的完整错误描述
	Message string
	// Err 底层原因（可为 nil）
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// Unwrap 暴露底层原因（支持 errors.Is / errors.As）
func (e *Error) Unwrap() error { return e.Err }

// ─── 构造器（私有，保证错误消息统一）──────────────────────────────

func newConfigError(format string, args ...any) *Error {
	return &Error{Kind: ErrConfig, Message: fmt.Sprintf(format, args...)}
}

func newConfigErrorWrap(err error, format string, args ...any) *Error {
	return &Error{Kind: ErrConfig, Message: fmt.Sprintf(format, args...), Err: err}
}

func newDatabaseConnectionError(err error, url string) *Error {
	return &Error{
		Kind:    ErrDatabaseConnection,
		Message: fmt.Sprintf("无法连接到数据库: %s", url),
		Err:     err,
	}
}

func newSqlExecutionError(err error, sqlText string) *Error {
	return &Error{
		Kind:    ErrSqlExecution,
		Message: fmt.Sprintf("SQL执行失败: %s", sqlText),
		Err:     err,
	}
}

func newQueryError(err error, sqlText string) *Error {
	return &Error{
		Kind:    ErrQuery,
		Message: fmt.Sprintf("查询失败: %s", sqlText),
		Err:     err,
	}
}

func newTransactionError(err error, format string, args ...any) *Error {
	return &Error{
		Kind:    ErrTransaction,
		Message: fmt.Sprintf(format, args...),
		Err:     err,
	}
}

func newPaginationError(format string, args ...any) *Error {
	return &Error{Kind: ErrPagination, Message: fmt.Sprintf(format, args...)}
}

func newSerializationError(err error, format string, args ...any) *Error {
	return &Error{
		Kind:    ErrSerialization,
		Message: fmt.Sprintf(format, args...),
		Err:     err,
	}
}

func newResultMappingError(err error, format string, args ...any) *Error {
	return &Error{
		Kind:    ErrResultMapping,
		Message: fmt.Sprintf(format, args...),
		Err:     err,
	}
}

// TooManyRowsError SelectOne 期望单行却返回多行时返回。
//
// 注意 payload 消息中的行数可能为实际截断值。
func TooManyRowsError(got int) *Error {
	return &Error{
		Kind:    ErrTooManyRows,
		Message: fmt.Sprintf("期望单行结果，实际得到 %d 行", got),
	}
}

func newMapperNotFoundError(namespace string) *Error {
	return &Error{
		Kind:    ErrMapperNotFound,
		Message: fmt.Sprintf("Mapper '%s' 不存在", namespace),
	}
}

func newSessionClosedError() *Error {
	return &Error{Kind: ErrSessionClosed, Message: "会话已关闭"}
}

func newFileWatcherError(err error, format string, args ...any) *Error {
	return &Error{
		Kind:    ErrFileWatcher,
		Message: fmt.Sprintf(format, args...),
		Err:     err,
	}
}
