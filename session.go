package higomapper

// SqlSession：请求级会话。
//
// 每个请求（或逻辑工作单元）持有一个 Session，共享工厂的连接池与注册表。
// 提供完整的 CRUD 接口与事务管理（Begin/Commit/Rollback）。
//
// 执行流程：查找 Mapper → 生成 BoundSql → SimpleExecutor 执行 → ResultSetHandler 映射。
//
// # 关于并发
//
// Session 是请求级对象（单 goroutine 顺序使用；事务模式下需独占事务连接）。
// 需要并发时为每个 goroutine 各开一个 Session（经工厂 OpenSession）。
//
// # 关于泛型
//
// Go 方法不能引入新的类型参数，因此泛型查询以包级函数提供：
//
//	user, err := higomapper.SelectOne[User](ctx, session, "ns", "findById", params)
//	users, err := higomapper.SelectList[User](ctx, session, "ns", "findAll", nil)
//
// 亦可经命名空间代理（固定 T）链式调用：
//
//	dao, err := higomapper.MapperOf[User](session, "ns")
//	user, err := dao.SelectOne(ctx, "findById", params)

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/dunpju/higo-mapper/core"
)

// SqlSession 请求级会话
type SqlSession struct {
	environment       *Environment
	mapperRegistry    *MapperRegistry
	typeAliasRegistry *TypeAliasRegistry
	typeHandlers      *TypeHandlerRegistry
	sqlLog            SqlLogConfig
	bus               *EventBus
	executor          *SimpleExecutor
	tx                *sql.Tx
	closed            bool
}

// newSqlSession 由 SqlSessionFactory 调用的构造函数
func newSqlSession(
	env *Environment,
	registry *MapperRegistry,
	aliases *TypeAliasRegistry,
	handlers *TypeHandlerRegistry,
	sqlLog SqlLogConfig,
	bus *EventBus,
) *SqlSession {
	return &SqlSession{
		environment:       env,
		mapperRegistry:    registry,
		typeAliasRegistry: aliases,
		typeHandlers:      handlers,
		sqlLog:            sqlLog,
		bus:               bus,
		executor: (&SimpleExecutor{TypeHandlers: handlers}).
			WithSqlLog(sqlLog).
			WithEventBus(bus),
	}
}

// ─── 访问器 ───────────────────────────────────────────────────────

// Environment 数据库环境
func (s *SqlSession) Environment() *Environment { return s.environment }

// DB 连接池
func (s *SqlSession) DB() *sql.DB { return s.environment.DB() }

// MapperRegistry Mapper 注册表（内部自带线程安全，写操作如 InsertMapper 也经本引用）
func (s *SqlSession) MapperRegistry() *MapperRegistry { return s.mapperRegistry }

// TypeAliasRegistry 类型别名注册表
func (s *SqlSession) TypeAliasRegistry() *TypeAliasRegistry { return s.typeAliasRegistry }

// TypeHandlerRegistry 类型处理器注册表
func (s *SqlSession) TypeHandlerRegistry() *TypeHandlerRegistry { return s.typeHandlers }

// Executor 执行器
func (s *SqlSession) Executor() *SimpleExecutor { return s.executor }

// EventBus 事件总线（用于注册 SQL 执行前/后事件监听器）
func (s *SqlSession) EventBus() *EventBus { return s.bus }

// InTransaction 是否处于事务中
func (s *SqlSession) InTransaction() bool { return s.tx != nil }

// IsClosed session 是否已关闭
func (s *SqlSession) IsClosed() bool { return s.closed }

// checkOpen 会话可用性检查
func (s *SqlSession) checkOpen() error {
	if s.closed {
		return newSessionClosedError()
	}
	return nil
}

// querier 按事务状态选择执行目标
func (s *SqlSession) querier() Querier {
	if s.tx != nil {
		return s.tx
	}
	return s.environment.DB()
}

// ─── Mapper 查找与 SQL 生成 ───────────────────────────────────────

// GetMapper 按 namespace 查找 Mapper；不存在返回错误
func (s *SqlSession) GetMapper(namespace string) (*core.Mapper, error) {
	if m := s.mapperRegistry.GetMapper(namespace); m != nil {
		return m, nil
	}
	return nil, newMapperNotFoundError(namespace)
}

// BuildBoundSql 两阶段绑定：生成 BoundSql（#{} → ?，${} → 内联）
func (s *SqlSession) BuildBoundSql(namespace, statementID string, params any) (*core.BoundSql, error) {
	mapper, err := s.GetMapper(namespace)
	if err != nil {
		return nil, err
	}
	p, err := paramsFrom(params)
	if err != nil {
		return nil, err
	}
	return mapper.BuildBoundSQL(statementID, p)
}

// paramsFrom 将任意参数（struct/map/Params/标量）转为 core.Params。
//
//   - nil → 空参数表
//   - core.Params / map[string]any → 逐值规范化
//   - 其他（struct / 标量）→ JSON 往返（UseNumber 保持数字语义）；
//     非对象结果包装在 "_param" 键下，供 #{_param} 引用
func paramsFrom(params any) (core.Params, error) {
	switch t := params.(type) {
	case nil:
		return core.Params{}, nil
	case core.Params:
		return t, nil
	case map[string]any:
		out := make(core.Params, len(t))
		for k, v := range t {
			out[k] = NormalizeValue(v)
		}
		return out, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, newSerializationError(err, "参数序列化失败")
	}
	var decoded any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return nil, newSerializationError(err, "参数序列化失败")
	}
	decoded = normalizeJSON(decoded)
	if m, ok := decoded.(map[string]any); ok {
		return core.Params(m), nil
	}
	return core.Params{"_param": decoded}, nil
}

// resultMapOf 查询语句关联的 ResultMap（无关联返回 nil）
func resultMapOf(mapper *core.Mapper, statementID string) (*core.ResultMap, error) {
	stmt, ok := mapper.Statements[statementID]
	if !ok {
		return nil, core.NewStatementNotFound(statementID)
	}
	if stmt.ResultMap == "" {
		return nil, nil
	}
	rm, ok := mapper.ResultMaps[stmt.ResultMap]
	if !ok {
		return nil, nil
	}
	return rm, nil
}

// ─── 查询接口（包级泛型）─────────────────────────────────────────

// SelectOne 查询单行（期望 0 或 1 行；多于 1 行报 TooManyRows 错误）。
//
// 语句声明了 resultMap 时走嵌套映射（association/collection），否则按列名直接映射。
func SelectOne[T any](ctx context.Context, s *SqlSession, namespace, statementID string, params any) (*T, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	mapper, err := s.GetMapper(namespace)
	if err != nil {
		return nil, err
	}
	rm, err := resultMapOf(mapper, statementID)
	if err != nil {
		return nil, err
	}
	p, err := paramsFrom(params)
	if err != nil {
		return nil, err
	}
	bound, err := mapper.BuildBoundSQL(statementID, p)
	if err != nil {
		return nil, err
	}
	rows, err := s.Executor().QueryRows(ctx, s.querier(), bound)
	if err != nil {
		return nil, err
	}
	if rm != nil {
		return MapRowWithResultMap[T](rows, rm, s.TypeHandlerRegistry())
	}
	switch len(rows) {
	case 0:
		return nil, nil
	case 1:
		t, err := MapRow[T](rows[0])
		if err != nil {
			return nil, err
		}
		return &t, nil
	default:
		return nil, TooManyRowsError(len(rows))
	}
}

// SelectList 查询多行。
//
// 语句声明了 resultMap 时走嵌套映射（含 collection 分组），否则按列名直接映射。
func SelectList[T any](ctx context.Context, s *SqlSession, namespace, statementID string, params any) ([]T, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	mapper, err := s.GetMapper(namespace)
	if err != nil {
		return nil, err
	}
	rm, err := resultMapOf(mapper, statementID)
	if err != nil {
		return nil, err
	}
	p, err := paramsFrom(params)
	if err != nil {
		return nil, err
	}
	bound, err := mapper.BuildBoundSQL(statementID, p)
	if err != nil {
		return nil, err
	}
	rows, err := s.Executor().QueryRows(ctx, s.querier(), bound)
	if err != nil {
		return nil, err
	}
	if rm != nil {
		return MapRowsWithResultMap[T](rows, rm, s.TypeHandlerRegistry())
	}
	return MapRows[T](rows)
}

// SelectForEach 流式查询：逐行拉取并经回调处理，避免 SelectList 一次性物化整表，
// 适合大结果集的低内存峰值场景。
//
// 仅支持普通列映射，不支持 ResultMap 嵌套分组（分组需聚集全部行，与流式语义冲突），
// 也不派发 SQL 生命周期事件（行数在流结束前未知）。回调返回错误可提前终止并向上传递。
func SelectForEach[T any](
	ctx context.Context,
	s *SqlSession,
	namespace, statementID string,
	params any,
	fn func(*T) error,
) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	bound, err := s.BuildBoundSql(namespace, statementID, params)
	if err != nil {
		return err
	}
	rows, err := s.Executor().QueryRowsStream(ctx, s.querier(), bound)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return newResultMappingError(err, "读取列信息失败")
	}
	for rows.Next() {
		row, err := scanRow(rows, cols)
		if err != nil {
			return err
		}
		item, err := MapRow[T](row)
		if err != nil {
			return err
		}
		if err := fn(&item); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return newResultMappingError(err, "遍历结果集失败")
	}
	return nil
}

// ─── 写入接口 ─────────────────────────────────────────────────────

// Insert 插入，返回生成的主键（*int64；驱动不支持时为 nil，如 postgres/pgx）。
//
// 主键经 sql.Result.LastInsertId() 取回（mysql / sqlite 驱动支持）。
func (s *SqlSession) Insert(ctx context.Context, namespace, statementID string, params any) (*int64, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	bound, err := s.BuildBoundSql(namespace, statementID, params)
	if err != nil {
		return nil, err
	}
	result, err := s.Executor().Execute(ctx, s.querier(), bound)
	if err != nil {
		return nil, err
	}
	id, idErr := result.LastInsertId()
	if idErr != nil {
		// 驱动不透传 last_insert_id（如 pgx）→ 与 Rust 版一致返回 nil
		return nil, nil
	}
	return &id, nil
}

// Update 更新（返回受影响行数）
func (s *SqlSession) Update(ctx context.Context, namespace, statementID string, params any) (int64, error) {
	return s.executeWrite(ctx, namespace, statementID, params)
}

// Delete 删除（返回受影响行数）
func (s *SqlSession) Delete(ctx context.Context, namespace, statementID string, params any) (int64, error) {
	return s.executeWrite(ctx, namespace, statementID, params)
}

// executeWrite Update/Delete 共用路径
func (s *SqlSession) executeWrite(ctx context.Context, namespace, statementID string, params any) (int64, error) {
	if err := s.checkOpen(); err != nil {
		return 0, err
	}
	bound, err := s.BuildBoundSql(namespace, statementID, params)
	if err != nil {
		return 0, err
	}
	result, err := s.Executor().Execute(ctx, s.querier(), bound)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, newSqlExecutionError(err, bound.SQL)
	}
	return n, nil
}

// ─── 事务管理 ─────────────────────────────────────────────────────

// Begin 开启事务
func (s *SqlSession) Begin(ctx context.Context) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	if s.tx != nil {
		return newTransactionError(nil, "事务已开启，请先 Commit 或 Rollback")
	}
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return newTransactionError(err, "开启事务失败")
	}
	s.tx = tx
	return nil
}

// Commit 提交事务并结束会话（与 Rust 版 commit(self) 消费语义一致）
func (s *SqlSession) Commit() error {
	if s.tx != nil {
		tx := s.tx
		s.tx = nil
		if err := tx.Commit(); err != nil {
			return newTransactionError(err, "提交失败")
		}
	}
	s.closed = true
	return nil
}

// Rollback 回滚事务并结束会话
func (s *SqlSession) Rollback() error {
	if s.tx != nil {
		tx := s.tx
		s.tx = nil
		if err := tx.Rollback(); err != nil {
			return newTransactionError(err, "回滚失败")
		}
	}
	s.closed = true
	return nil
}

// Close 关闭 session（未提交的事务将回滚）
func (s *SqlSession) Close() error {
	if s.tx != nil {
		tx := s.tx
		s.tx = nil
		_ = tx.Rollback() // 关闭时回滚未提交事务
	}
	s.closed = true
	return nil
}

// ─── 命名空间代理 ─────────────────────────────────────────────────

// MapperProxy 命名空间代理：绑定到特定 namespace 与结果类型 T（链式调用风格）。
//
// 经包级函数 MapperOf[T] 创建（Go 方法不能引入新的类型参数）。
type MapperProxy[T any] struct {
	session   *SqlSession
	namespace string
}

// MapperOf 获取绑定到 namespace 与结果类型 T 的代理（同时验证 namespace 存在）
func MapperOf[T any](s *SqlSession, namespace string) (*MapperProxy[T], error) {
	if _, err := s.GetMapper(namespace); err != nil {
		return nil, err
	}
	return &MapperProxy[T]{session: s, namespace: namespace}, nil
}

// Namespace 代理绑定的命名空间
func (p *MapperProxy[T]) Namespace() string { return p.namespace }

// SelectOne 查询单行
func (p *MapperProxy[T]) SelectOne(ctx context.Context, statementID string, params any) (*T, error) {
	return SelectOne[T](ctx, p.session, p.namespace, statementID, params)
}

// SelectList 查询多行
func (p *MapperProxy[T]) SelectList(ctx context.Context, statementID string, params any) ([]T, error) {
	return SelectList[T](ctx, p.session, p.namespace, statementID, params)
}

// SelectForEach 流式查询
func (p *MapperProxy[T]) SelectForEach(ctx context.Context, statementID string, params any, fn func(*T) error) error {
	return SelectForEach[T](ctx, p.session, p.namespace, statementID, params, fn)
}

// Insert 插入（返回生成的主键，若驱动支持）
func (p *MapperProxy[T]) Insert(ctx context.Context, statementID string, params any) (*int64, error) {
	return p.session.Insert(ctx, p.namespace, statementID, params)
}

// Update 更新（返回受影响行数）
func (p *MapperProxy[T]) Update(ctx context.Context, statementID string, params any) (int64, error) {
	return p.session.Update(ctx, p.namespace, statementID, params)
}

// Delete 删除（返回受影响行数）
func (p *MapperProxy[T]) Delete(ctx context.Context, statementID string, params any) (int64, error) {
	return p.session.Delete(ctx, p.namespace, statementID, params)
}
