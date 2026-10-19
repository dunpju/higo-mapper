package higomapper

// SimpleExecutor：基础的 SQL 执行器。
//
// 串联 ParameterHandler（绑定）→ database/sql 执行 → ResultSetHandler（映射）。
// 所有方法以 Querier 接口接收执行目标，因此同一套逻辑可作用于
// 连接池（*sql.DB）或事务（*sql.Tx）。

import (
	"context"
	"database/sql"
	"time"

	"github.com/dunpju/higo-mapper/core"
)

// Querier SQL 执行目标抽象。
//
// *sql.DB / *sql.Tx / *sql.Conn 均满足本接口，
// 从而同一执行器可无差别作用于连接池与事务。
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// SimpleExecutor 基础执行器。
//
// 无状态（除类型处理器注册表、SQL 日志配置与事件总线），可被多个 Session 共享。
type SimpleExecutor struct {
	// TypeHandlers 类型处理器注册表（ResultMap goType 转换用）
	TypeHandlers *TypeHandlerRegistry
	// SQLLog SQL 日志配置（开关 / 慢查询阈值）
	SQLLog SqlLogConfig
	// Bus 事件总线（派发 SQL 执行前/后生命周期事件）
	Bus *EventBus
}

// NewSimpleExecutor 使用默认内置类型处理器创建执行器
func NewSimpleExecutor() *SimpleExecutor {
	return &SimpleExecutor{
		TypeHandlers: DefaultTypeHandlerRegistry(),
		SQLLog:       SqlLogConfig{},
		Bus:          NewEventBus(),
	}
}

// WithSqlLog 设置 SQL 日志配置（开启执行日志 / 慢查询阈值）
func (e *SimpleExecutor) WithSqlLog(cfg SqlLogConfig) *SimpleExecutor {
	e.SQLLog = cfg
	return e
}

// WithEventBus 设置事件总线（用于派发 SQL 执行前/后生命周期事件）
func (e *SimpleExecutor) WithEventBus(bus *EventBus) *SimpleExecutor {
	e.Bus = bus
	return e
}

// needTiming 是否需要计时：仅当 SQL 日志开启 或 有 AfterSqlEvent 监听器
// （全关时零开销）
func (e *SimpleExecutor) needTiming() bool {
	return e.SQLLog.Enabled || HasListeners[*AfterSqlEvent](e.Bus)
}

// QueryRows 执行查询，返回原始行（未映射为 T，已物化为 map 行）。
//
// 派发 Before/After SQL 事件并记录执行日志。
func (e *SimpleExecutor) QueryRows(ctx context.Context, q Querier, bound *core.BoundSql) ([]map[string]any, error) {
	args, err := BindArguments(bound)
	if err != nil {
		return nil, err
	}
	DispatchIf(e.Bus, func() *BeforeSqlEvent {
		return &BeforeSqlEvent{RawSQL: bound.SQL, Params: bound.Parameters, Kind: SqlSelect}
	})
	var start time.Time
	timed := e.needTiming()
	if timed {
		start = time.Now()
	}
	rows, err := q.QueryContext(ctx, bound.SQL, args...)
	elapsed := time.Duration(0)
	if timed {
		elapsed = time.Since(start)
	}
	if err != nil {
		LogExecution(e.SQLLog, bound, elapsed)
		DispatchIf(e.Bus, func() *AfterSqlEvent {
			return &AfterSqlEvent{
				RawSQL: bound.SQL, Params: bound.Parameters, Kind: SqlSelect,
				Elapsed: elapsed, Outcome: SqlOutcome{Kind: OutcomeFailed, Err: err.Error()},
			}
		})
		return nil, newQueryError(err, bound.SQL)
	}
	defer rows.Close()
	maps, mapErr := RowsToMaps(rows)
	LogExecution(e.SQLLog, bound, elapsed)
	DispatchIf(e.Bus, func() *AfterSqlEvent {
		outcome := SqlOutcome{Kind: OutcomeFetched, Rows: int64(len(maps))}
		if mapErr != nil {
			outcome = SqlOutcome{Kind: OutcomeFailed, Err: mapErr.Error()}
		}
		return &AfterSqlEvent{
			RawSQL: bound.SQL, Params: bound.Parameters, Kind: SqlSelect,
			Elapsed: elapsed, Outcome: outcome,
		}
	})
	if mapErr != nil {
		return nil, mapErr
	}
	return maps, nil
}

// QueryRowsStream 流式查询：返回逐行拉取的 *sql.Rows
// （按需拉取，避免 QueryRows 一次性物化整表）。
//
// 不派发 SQL 事件（行数在流结束前未知）；调用方负责 rows.Close()。
func (e *SimpleExecutor) QueryRowsStream(ctx context.Context, q Querier, bound *core.BoundSql) (*sql.Rows, error) {
	args, err := BindArguments(bound)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, bound.SQL, args...)
	if err != nil {
		return nil, newQueryError(err, bound.SQL)
	}
	return rows, nil
}

// Execute 执行更新（INSERT/UPDATE/DELETE），返回 sql.Result
// （含受影响行数与生成主键，视驱动支持而定）。
//
// 派发 Before/After SQL 事件并记录执行日志。
func (e *SimpleExecutor) Execute(ctx context.Context, q Querier, bound *core.BoundSql) (sql.Result, error) {
	args, err := BindArguments(bound)
	if err != nil {
		return nil, err
	}
	kind := ClassifySQL(bound.SQL)
	DispatchIf(e.Bus, func() *BeforeSqlEvent {
		return &BeforeSqlEvent{RawSQL: bound.SQL, Params: bound.Parameters, Kind: kind}
	})
	var start time.Time
	timed := e.needTiming()
	if timed {
		start = time.Now()
	}
	result, err := q.ExecContext(ctx, bound.SQL, args...)
	elapsed := time.Duration(0)
	if timed {
		elapsed = time.Since(start)
	}
	LogExecution(e.SQLLog, bound, elapsed)
	DispatchIf(e.Bus, func() *AfterSqlEvent {
		outcome := SqlOutcome{}
		if err != nil {
			outcome = SqlOutcome{Kind: OutcomeFailed, Err: err.Error()}
		} else if n, rowsErr := result.RowsAffected(); rowsErr == nil {
			outcome = SqlOutcome{Kind: OutcomeAffected, Rows: n}
		}
		return &AfterSqlEvent{
			RawSQL: bound.SQL, Params: bound.Parameters, Kind: kind,
			Elapsed: elapsed, Outcome: outcome,
		}
	})
	if err != nil {
		return nil, newSqlExecutionError(err, bound.SQL)
	}
	return result, nil
}

// ExecuteRowsAffected 独立辅助：执行绑定并返回受影响行数（无需 SimpleExecutor 实例）。
//
// sqlLog 控制 SQL 执行日志；bus 用于派发执行前/后生命周期事件。
func ExecuteRowsAffected(
	ctx context.Context,
	q Querier,
	bound *core.BoundSql,
	sqlLog SqlLogConfig,
	bus *EventBus,
) (int64, error) {
	result, err := (&SimpleExecutor{SQLLog: sqlLog, Bus: bus}).Execute(ctx, q, bound)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, newSqlExecutionError(err, bound.SQL)
	}
	return n, nil
}
