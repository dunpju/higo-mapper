package higomapper

// SqlSession：请求级会话。
//
// 每个请求（或逻辑工作单元）持有一个 Session，共享工厂的连接池、注册表、
// 查询缓存与生命周期配置。提供完整的 CRUD 接口与事务管理（Begin/Commit/Rollback）。
//
// 执行流程：查找 Mapper → 生成 BoundSql → SimpleExecutor 执行 → ResultSetHandler 映射。
//
// # 读写分离（v0.3+）
//
// 工厂配置了 [[read_environments]] 时：非事务读（select_* / stream）轮询路由到
// 只读副本；写语句与事务内的一切操作始终走主库。
//
// # 查询缓存（v0.3+）
//
// mapper 声明 <cache/> 的 namespace：select_one / select_list 命中缓存时
// 直接返回（不执行 SQL、不派发事件、不打日志）；该 namespace 任一写语句
// 成功后整 namespace 失效。分页与流式查询不缓存。
//
// # 自动时间戳（v0.3+）
//
// [settings.auto_timestamps] 开启后：insert 填充 created_at + updated_at、
// update 填充 updated_at（仅当参数表缺失对应键；显式传值优先）。
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
//	page, err := higomapper.SelectPage[User](ctx, session, "ns", "findAll", nil, req)
//
// 亦可经命名空间代理（固定 T）链式调用：
//
//	dao, err := higomapper.MapperOf[User](session, "ns")
//	user, err := dao.SelectOne(ctx, "findById", params)

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	readRouter        *ReadRouter
	lifecycle         LifecycleConfig
	cache             *QueryCache
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
	readRouter *ReadRouter,
	lifecycle LifecycleConfig,
	cache *QueryCache,
) *SqlSession {
	return &SqlSession{
		environment:       env,
		mapperRegistry:    registry,
		typeAliasRegistry: aliases,
		typeHandlers:      handlers,
		sqlLog:            sqlLog,
		bus:               bus,
		readRouter:        readRouter,
		lifecycle:         lifecycle,
		cache:             cache,
		executor: (&SimpleExecutor{TypeHandlers: handlers}).
			WithSqlLog(sqlLog).
			WithEventBus(bus),
	}
}

// ─── 访问器 ───────────────────────────────────────────────────────

// Environment 主库环境
func (s *SqlSession) Environment() *Environment { return s.environment }

// DB 主库连接池
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

// Lifecycle 实体生命周期配置（自动时间戳 / 逻辑删除）
func (s *SqlSession) Lifecycle() LifecycleConfig { return s.lifecycle }

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

// querier 写路径执行目标：事务连接或主库（写永远不走副本）
func (s *SqlSession) querier() Querier {
	if s.tx != nil {
		return s.tx
	}
	return s.environment.DB()
}

// readQuerier 读路径执行目标：事务连接 > 只读副本（轮询）> 主库
func (s *SqlSession) readQuerier() Querier {
	if s.tx != nil {
		return s.tx
	}
	if s.readRouter != nil {
		if env := s.readRouter.Pick(); env != nil {
			return env.DB()
		}
	}
	return s.environment.DB()
}

// QueryBuilder 创建绑定本会话生命周期配置的链式查询构造器。
//
// 逻辑删除开启时自动预置 is_delete = <normal_value> 过滤（XML 语句不受影响，
// 由作者显式书写——保守方案不改写 SQL）。
func (s *SqlSession) QueryBuilder(table string) *QueryBuilder {
	b := NewQueryBuilder().Table(table)
	if ld := s.lifecycle.LogicalDelete; ld.Enabled {
		b = b.WhereEq(ld.Column, ld.NormalValue)
	}
	return b
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

// buildWriteBound 写路径共用：生成 BoundSql 并应用自动时间戳。
//
// created 为真（insert）填充 created_at + updated_at，否则（update）仅 updated_at；
// 均只填充缺失键（调用方显式传值优先）。
func (s *SqlSession) buildWriteBound(namespace, statementID string, params any, created bool) (*core.BoundSql, error) {
	mapper, err := s.GetMapper(namespace)
	if err != nil {
		return nil, err
	}
	p, err := paramsFrom(params)
	if err != nil {
		return nil, err
	}
	s.lifecycle.ApplyTimestamps(p, created)
	return mapper.BuildBoundSQL(statementID, p)
}

// invalidateCache namespace 整体缓存失效（任一写语句成功后调用）
func (s *SqlSession) invalidateCache(namespace string) {
	if s.cache != nil {
		s.cache.InvalidateNamespace(namespace)
	}
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

// normalizeEntity 实体值规范化：对象 → map（列名 → 通用值），标量原样。
//
// 与 paramsFrom 的差异：标量不被包装为 {"_param": v}——批量写语句以
// foreach 逐项引用实体字段（#{u.name}），标量实体直接按 #{u} 引用。
func normalizeEntity(v any) (any, error) {
	p, err := paramsFrom(v)
	if err != nil {
		return nil, err
	}
	if len(p) == 1 {
		if s, ok := p["_param"]; ok {
			return s, nil
		}
	}
	return map[string]any(p), nil
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

// fetchValues 查询语句 → 通用行数据（统一读路径）。
//
// 返回值：rows 为「ResultMap 分组后的父对象」或「原始行」（无 resultMap），
// rm 标记是否发生了分组。缓存集成点：
//
//   - mapper 声明 <cache/> 且 useCache 时先查缓存（命中不执行 SQL/事件/日志）
//   - 未命中执行查询后按分组形态写缓存（同一份数据可服务不同目标类型 T）
//
// SelectOne 的 TooManyRows 判定基于分组后的父对象数（v0.4 语义）。
func (s *SqlSession) fetchValues(
	ctx context.Context,
	mapper *core.Mapper,
	namespace, statementID string,
	params any,
	useCache bool,
) ([]map[string]any, *core.ResultMap, error) {
	rm, err := resultMapOf(mapper, statementID)
	if err != nil {
		return nil, nil, err
	}
	p, err := paramsFrom(params)
	if err != nil {
		return nil, nil, err
	}
	bound, err := mapper.BuildBoundSQL(statementID, p)
	if err != nil {
		return nil, nil, err
	}

	cacheable := useCache && mapper.Cache != nil && s.cache != nil
	if cacheable {
		s.cache.ConfigureNamespace(namespace, mapper.Cache)
		if rows, hit := s.cache.Get(namespace, bound.SQL, bound.Parameters); hit {
			return rows, rm, nil
		}
	}

	rows, err := s.Executor().QueryRows(ctx, s.readQuerier(), bound)
	if err != nil {
		return nil, nil, err
	}
	if rm != nil {
		rows = groupRowsWithResultMap(rows, rm, s.TypeHandlerRegistry())
	}
	if cacheable {
		s.cache.Put(namespace, bound.SQL, bound.Parameters, rows)
	}
	return rows, rm, nil
}

// ─── 查询接口（包级泛型）─────────────────────────────────────────

// SelectOne 查询单行（期望 0 或 1 行；多于 1 行报 TooManyRows 错误）。
//
// 语句声明了 resultMap 时走嵌套映射（association/collection），TooManyRows
// 按分组后的父对象数判定（join 展开的多行属于同一父对象不算多行）。
// mapper 声明 <cache/> 时走 L2 查询缓存。
func SelectOne[T any](ctx context.Context, s *SqlSession, namespace, statementID string, params any) (*T, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	mapper, err := s.GetMapper(namespace)
	if err != nil {
		return nil, err
	}
	rows, _, err := s.fetchValues(ctx, mapper, namespace, statementID, params, true)
	if err != nil {
		return nil, err
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
// mapper 声明 <cache/> 时走 L2 查询缓存。
func SelectList[T any](ctx context.Context, s *SqlSession, namespace, statementID string, params any) ([]T, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	mapper, err := s.GetMapper(namespace)
	if err != nil {
		return nil, err
	}
	rows, _, err := s.fetchValues(ctx, mapper, namespace, statementID, params, true)
	if err != nil {
		return nil, err
	}
	return MapRows[T](rows)
}

// SelectPage 分页查询（PageHelper 风格）：count 页 + 数据页两条 SQL。
//
//   - 总数：SELECT COUNT(*) FROM (剥离顶层 ORDER BY 的原 SQL) AS __higo_count_t
//     （子查询包装使 GROUP BY / UNION 的 count 语义正确）
//   - 数据页：原 SQL 追加 LIMIT ? OFFSET ?；ResultMap 分组发生在页内
//   - total = 0 时跳过数据查询直接返回空页
//   - 分页不查缓存；原语句含顶层 LIMIT/OFFSET 时报错（与追加式分页冲突）
//
// 注意：resultMap 分组场景下 LIMIT 作用于原始 join 行，页内分组合并后
// Records 数可能小于 PageSize，而 total 按 count 子查询（分组数）计——
// 需要严格每页 N 个父对象时请改用 QueryBuilder 手写分组外层分页。
func SelectPage[T any](
	ctx context.Context,
	s *SqlSession,
	namespace, statementID string,
	params any,
	page PageRequest,
) (Page[T], error) {
	if err := s.checkOpen(); err != nil {
		return EmptyPage[T](page), err
	}
	offset, err := page.ValidatedOffset()
	if err != nil {
		return EmptyPage[T](page), err
	}
	mapper, err := s.GetMapper(namespace)
	if err != nil {
		return EmptyPage[T](page), err
	}
	rm, err := resultMapOf(mapper, statementID)
	if err != nil {
		return EmptyPage[T](page), err
	}
	p, err := paramsFrom(params)
	if err != nil {
		return EmptyPage[T](page), err
	}
	bound, err := mapper.BuildBoundSQL(statementID, p)
	if err != nil {
		return EmptyPage[T](page), err
	}
	if err := ensureNoTopLevelLimit(bound.SQL); err != nil {
		return EmptyPage[T](page), err
	}

	// count 页
	countRows, err := s.Executor().QueryRows(ctx, s.readQuerier(), buildCountSQL(bound))
	if err != nil {
		return EmptyPage[T](page), err
	}
	total, err := firstColumnUint64(countRows)
	if err != nil {
		return EmptyPage[T](page), err
	}
	if total == 0 {
		return EmptyPage[T](page), nil
	}

	// 数据页（追加 LIMIT ? OFFSET ?）
	appendLimitOffset(bound, page.PageSize, offset)
	rows, err := s.Executor().QueryRows(ctx, s.readQuerier(), bound)
	if err != nil {
		return EmptyPage[T](page), err
	}
	if rm != nil {
		rows = groupRowsWithResultMap(rows, rm, s.TypeHandlerRegistry())
	}
	records, err := MapRows[T](rows)
	if err != nil {
		return EmptyPage[T](page), err
	}
	return NewPage(records, total, page.PageNo, page.PageSize), nil
}

// firstColumnUint64 取首行首列并转 uint64（COUNT 结果读取）
func firstColumnUint64(rows []map[string]any) (uint64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	for _, v := range rows[0] { // COUNT(*) 单列单行
		if v == nil {
			return 0, nil
		}
		switch t := v.(type) {
		case int64:
			return uint64(t), nil
		case float64:
			return uint64(t), nil
		case string:
			var n uint64
			if _, err := fmt.Sscanf(t, "%d", &n); err != nil {
				return 0, newResultMappingError(err, "COUNT 结果无法解析: '%s'", t)
			}
			return n, nil
		}
		return 0, newResultMappingError(nil, "COUNT 结果类型异常: %T", v)
	}
	return 0, nil
}

// SelectBound 执行 QueryBuilder 产物的 BoundSql 并映射为 []T。
//
// 与 XML 语句走同一执行层（读路由 / SQL 日志 / 事件），不经缓存。
func SelectBound[T any](ctx context.Context, s *SqlSession, bound *core.BoundSql) ([]T, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := s.Executor().QueryRows(ctx, s.readQuerier(), bound)
	if err != nil {
		return nil, err
	}
	return MapRows[T](rows)
}

// CountBound 执行 COUNT 类 BoundSql（如 QueryBuilder.BuildCount 产物），返回首行首列。
func CountBound(ctx context.Context, s *SqlSession, bound *core.BoundSql) (uint64, error) {
	if err := s.checkOpen(); err != nil {
		return 0, err
	}
	rows, err := s.Executor().QueryRows(ctx, s.readQuerier(), bound)
	if err != nil {
		return 0, err
	}
	return firstColumnUint64(rows)
}

// SelectForEach 流式查询：逐行拉取并经回调处理，避免 SelectList 一次性物化整表，
// 适合大结果集的低内存峰值场景。
//
// 仅支持普通列映射，不支持 ResultMap 嵌套分组（分组需聚集全部行，与流式语义冲突），
// 也不派发 SQL 生命周期事件（行数在流结束前未知）。流式读经副本路由但不走缓存。
// 回调返回错误可提前终止并向上传递。
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
	rows, err := s.Executor().QueryRowsStream(ctx, s.readQuerier(), bound)
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

// insertBatchPlaceholderBudget 单语句占位符预算（MySQL 上限 65535，
// 取 900 覆盖三方言的保守值）
const insertBatchPlaceholderBudget = 900

// insertBatchInitialTake 首块实体数上界（限制首块构建成本）
const insertBatchInitialTake = 512

// Insert 插入，返回生成的主键（*int64；驱动不支持时为 nil，如 postgres/pgx）。
//
// 主键经 sql.Result.LastInsertId() 取回（mysql / sqlite 驱动支持）。
// 自动时间戳开启时填充 created_at + updated_at（缺失键）。
func (s *SqlSession) Insert(ctx context.Context, namespace, statementID string, params any) (*int64, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	bound, err := s.buildWriteBound(namespace, statementID, params, true)
	if err != nil {
		return nil, err
	}
	result, err := s.Executor().Execute(ctx, s.querier(), bound)
	if err != nil {
		return nil, err
	}
	s.invalidateCache(namespace)
	id, idErr := result.LastInsertId()
	if idErr != nil {
		// 驱动不透传 last_insert_id（如 pgx）→ 与 Rust 版一致返回 nil
		return nil, nil
	}
	return &id, nil
}

// Update 更新（返回受影响行数）。自动时间戳开启时填充 updated_at（缺失键）。
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
	bound, err := s.buildWriteBound(namespace, statementID, params, false)
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
	s.invalidateCache(namespace)
	return n, nil
}

// InsertBatch 批量插入：实体按块经 foreach 语句拼成多行 VALUES。
//
// XML 语句须以 <foreach collection="list" item="u" separator=","> 组织多行
// VALUES（每块替换 list 参数重建 SQL）。占位符预算 900：块内占位符超预算时
// 按 take * 900 / 占位数 自适应收缩重建，直至适配或降到单实体（单实体仍超
// 预算时照常执行——由数据库裁定）。
//
// 原子性：会话已有事务则并入（调用方提交）；否则自动开独立事务，任一块
// 失败整体回滚。返回累计受影响行数；空列表返回 (0, nil)。
// 自动时间戳开启时对每个实体填充 created_at + updated_at（缺失键）。
func InsertBatch[T any](ctx context.Context, s *SqlSession, namespace, statementID string, entities []T) (int64, error) {
	if err := s.checkOpen(); err != nil {
		return 0, err
	}
	if len(entities) == 0 {
		return 0, nil
	}
	mapper, err := s.GetMapper(namespace)
	if err != nil {
		return 0, err
	}
	values := make([]any, len(entities))
	for i := range entities {
		v, err := normalizeEntity(entities[i])
		if err != nil {
			return 0, err
		}
		values[i] = v
	}
	s.lifecycle.ApplyTimestampsToValues(values, true)

	// 会话已有事务 → 并入；否则开独立事务保证原子性
	var tx *sql.Tx
	ownTx := false
	if s.tx != nil {
		tx = s.tx
	} else {
		tx, err = s.DB().BeginTx(ctx, nil)
		if err != nil {
			return 0, newTransactionError(err, "开启批量插入事务失败")
		}
		ownTx = true
	}

	var total int64
	committed := false
	defer func() {
		if ownTx && !committed {
			_ = tx.Rollback()
		}
	}()

	take := insertBatchInitialTake
	for start := 0; start < len(values); {
		if take > len(values)-start {
			take = len(values) - start
		}
		chunk := values[start : start+take]
		bound, err := mapper.BuildBoundSQL(statementID, core.Params{"list": chunk})
		if err != nil {
			return 0, err
		}
		// 占位符超预算：自适应收缩块大小重建
		for bound.ParamCount() > insertBatchPlaceholderBudget && take > 1 {
			take = max(take*insertBatchPlaceholderBudget/bound.ParamCount(), 1)
			if take == 1 {
				break
			}
			chunk = values[start : start+take]
			bound, err = mapper.BuildBoundSQL(statementID, core.Params{"list": chunk})
			if err != nil {
				return 0, err
			}
		}
		result, err := s.Executor().Execute(ctx, tx, bound)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, newSqlExecutionError(err, bound.SQL)
		}
		total += n
		start += take
	}

	if ownTx {
		if err := tx.Commit(); err != nil {
			return 0, newTransactionError(err, "批量插入提交失败")
		}
		committed = true
	}
	s.invalidateCache(namespace)
	return total, nil
}

// UpdateBatch 批量更新：逐实体生成 BoundSql，在同一事务内顺序执行。
//
// 原子性语义与 InsertBatch 一致（会话事务并入 / 独立事务整体回滚）。
// 自动时间戳开启时对每个实体填充 updated_at（缺失键）。
func UpdateBatch[T any](ctx context.Context, s *SqlSession, namespace, statementID string, entities []T) (int64, error) {
	if err := s.checkOpen(); err != nil {
		return 0, err
	}
	if len(entities) == 0 {
		return 0, nil
	}
	mapper, err := s.GetMapper(namespace)
	if err != nil {
		return 0, err
	}
	values := make([]any, len(entities))
	for i := range entities {
		v, err := normalizeEntity(entities[i])
		if err != nil {
			return 0, err
		}
		values[i] = v
	}
	s.lifecycle.ApplyTimestampsToValues(values, false)

	var tx *sql.Tx
	ownTx := false
	if s.tx != nil {
		tx = s.tx
	} else {
		tx, err = s.DB().BeginTx(ctx, nil)
		if err != nil {
			return 0, newTransactionError(err, "开启批量更新事务失败")
		}
		ownTx = true
	}

	var total int64
	committed := false
	defer func() {
		if ownTx && !committed {
			_ = tx.Rollback()
		}
	}()

	for _, v := range values {
		p, err := paramsFrom(v)
		if err != nil {
			return 0, err
		}
		bound, err := mapper.BuildBoundSQL(statementID, p)
		if err != nil {
			return 0, err
		}
		result, err := s.Executor().Execute(ctx, tx, bound)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, newSqlExecutionError(err, bound.SQL)
		}
		total += n
	}

	if ownTx {
		if err := tx.Commit(); err != nil {
			return 0, newTransactionError(err, "批量更新提交失败")
		}
		committed = true
	}
	s.invalidateCache(namespace)
	return total, nil
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

// SelectPage 分页查询
func (p *MapperProxy[T]) SelectPage(ctx context.Context, statementID string, params any, page PageRequest) (Page[T], error) {
	return SelectPage[T](ctx, p.session, p.namespace, statementID, params, page)
}

// SelectForEach 流式查询
func (p *MapperProxy[T]) SelectForEach(ctx context.Context, statementID string, params any, fn func(*T) error) error {
	return SelectForEach[T](ctx, p.session, p.namespace, statementID, params, fn)
}

// Insert 插入（返回生成的主键，若驱动支持）
func (p *MapperProxy[T]) Insert(ctx context.Context, statementID string, params any) (*int64, error) {
	return p.session.Insert(ctx, p.namespace, statementID, params)
}

// InsertBatch 批量插入
func (p *MapperProxy[T]) InsertBatch(ctx context.Context, statementID string, entities []T) (int64, error) {
	return InsertBatch[T](ctx, p.session, p.namespace, statementID, entities)
}

// UpdateBatch 批量更新
func (p *MapperProxy[T]) UpdateBatch(ctx context.Context, statementID string, entities []T) (int64, error) {
	return UpdateBatch[T](ctx, p.session, p.namespace, statementID, entities)
}

// Update 更新（返回受影响行数）
func (p *MapperProxy[T]) Update(ctx context.Context, statementID string, params any) (int64, error) {
	return p.session.Update(ctx, p.namespace, statementID, params)
}

// Delete 删除（返回受影响行数）
func (p *MapperProxy[T]) Delete(ctx context.Context, statementID string, params any) (int64, error) {
	return p.session.Delete(ctx, p.namespace, statementID, params)
}
