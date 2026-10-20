package higomapper

// SqlSessionFactory：SqlSession 工厂（应用级）。
//
// 长生命周期对象（对应 MyBatis 的 SqlSessionFactory），持有 Mapper 注册表、
// 数据库环境、类型别名与类型处理器注册表。从配置构建，通过 OpenSession
// 创建请求级的 SqlSession。
//
// # 示例
//
//	config, err := higomapper.LoadConfigFile("higo-mapper.toml")
//	factory, err := higomapper.NewSqlSessionFactory(ctx, config, ".")
//	session := factory.OpenSession()
//	defer session.Close()

import (
	"context"
	"fmt"
	"os"

	"github.com/dunpju/higo-mapper/core"
)

// SqlSessionFactory 应用级工厂（内部组件线程安全）
type SqlSessionFactory struct {
	environment       *Environment
	mapperRegistry    *MapperRegistry
	typeAliasRegistry *TypeAliasRegistry
	typeHandlers      *TypeHandlerRegistry
	sqlLog            SqlLogConfig
	bus               *EventBus
	readRouter        *ReadRouter
	lifecycle         LifecycleConfig
	cache             *QueryCache
	config            *Config
	baseDir           string
	// watcher 热重载监视器（nil 表示未启用热重载）
	watcher *MapperWatcher
}

// NewSqlSessionFactory 从配置构建 SqlSessionFactory。
//
// 完整流程：
//  1. 创建数据库连接池（Environment，含 Ping 校验）
//  2. 创建只读副本路由器（[[read_environments]]；失败整体报错）
//  3. 初始化 Mapper 注册表并加载所有 XML mapper 文件
//  4. 初始化类型别名 / 类型处理器注册表与查询缓存
//  5. 当 mapper_refresh_interval_ms > 0 时启动热重载（失败仅告警，不阻断）
//
// baseDir 用于解析 mapper 文件的相对 glob 路径，通常传入项目根目录。
func NewSqlSessionFactory(ctx context.Context, config *Config, baseDir string) (*SqlSessionFactory, error) {
	// 1. 创建数据库环境
	env, err := NewEnvironment(ctx, config.Environment)
	if err != nil {
		return nil, err
	}

	// 2. 只读副本（读写分离；连接失败则关闭主库后整体报错）
	router, err := NewReadRouter(ctx, config.ReadEnvironments)
	if err != nil {
		env.Close()
		return nil, err
	}

	// 3. 初始化并加载 Mapper 注册表
	registry := NewMapperRegistry()
	if _, err := registry.LoadFromConfig(config, baseDir); err != nil {
		env.Close()
		router.Close()
		return nil, err
	}

	// 4. 初始化类型别名 / 类型处理器注册表 / 查询缓存
	aliases := TypeAliasRegistryFromMap(config.TypeAliases)
	handlers := NewTypeHandlerRegistry()
	cache := NewQueryCache()
	for _, ns := range registry.Namespaces() {
		if m := registry.GetMapper(ns); m != nil && m.Cache != nil {
			cache.ConfigureNamespace(ns, m.Cache)
		}
	}

	// SQL 执行日志配置（从 settings 解析，默认关闭）
	sqlLog := SqlLogConfig{
		Enabled:         config.Settings.SqlLog,
		SlowThresholdMs: config.Settings.SqlLogSlowThresholdMs,
	}

	// 事件总线（SQL 执行前/后生命周期事件；无监听器时派发零开销）
	bus := NewEventBus()

	// 5. 热重载（当 refresh_interval > 0 时启动）
	var watcher *MapperWatcher
	if config.Settings.MapperRefreshIntervalMs > 0 {
		watchDirs := ExtractWatchDirs(config.Settings.MapperPaths, baseDir)
		w, err := StartMapperWatcher(registry, watchDirs, config.Settings.MapperRefreshIntervalMs)
		if err != nil {
			// 热重载失败不阻断工厂构建（ORM 仍可用，仅失去热重载能力）
			fmt.Fprintf(os.Stderr, "[higo-mapper][WARN] 热重载启动失败（已禁用）: %v\n", err)
		} else {
			watcher = w
			fmt.Fprintf(os.Stderr, "[higo-mapper] 热重载已启用: 间隔 %dms\n",
				config.Settings.MapperRefreshIntervalMs)
		}
	}

	return &SqlSessionFactory{
		environment:       env,
		mapperRegistry:    registry,
		typeAliasRegistry: aliases,
		typeHandlers:      handlers,
		sqlLog:            sqlLog,
		bus:               bus,
		readRouter:        router,
		lifecycle:         LifecycleConfig{config.Settings.AutoTimestamps, config.Settings.LogicalDelete},
		cache:             cache,
		config:            config,
		baseDir:           baseDir,
		watcher:           watcher,
	}, nil
}

// NewSqlSessionFactoryFromParts 从已有组件构造（高级用法 / 测试；不启动热重载）
func NewSqlSessionFactoryFromParts(
	env *Environment,
	registry *MapperRegistry,
	aliases *TypeAliasRegistry,
	handlers *TypeHandlerRegistry,
	config *Config,
	baseDir string,
) *SqlSessionFactory {
	cache := NewQueryCache()
	for _, ns := range registry.Namespaces() {
		if m := registry.GetMapper(ns); m != nil && m.Cache != nil {
			cache.ConfigureNamespace(ns, m.Cache)
		}
	}
	return &SqlSessionFactory{
		environment:       env,
		mapperRegistry:    registry,
		typeAliasRegistry: aliases,
		typeHandlers:      handlers,
		sqlLog: SqlLogConfig{
			Enabled:         config.Settings.SqlLog,
			SlowThresholdMs: config.Settings.SqlLogSlowThresholdMs,
		},
		bus:        NewEventBus(),
		readRouter: &ReadRouter{},
		lifecycle:  LifecycleConfig{config.Settings.AutoTimestamps, config.Settings.LogicalDelete},
		cache:      cache,
		config:     config,
		baseDir:    baseDir,
		watcher:    nil,
	}
}

// EventBus 事件总线（注册 SQL 执行前/后事件监听器，所有 session 共享）
func (f *SqlSessionFactory) EventBus() *EventBus { return f.bus }

// Environment 数据库环境
func (f *SqlSessionFactory) Environment() *Environment { return f.environment }

// MapperRegistry Mapper 注册表（内部自带线程安全，写操作如 InsertMapper 也经本引用）
func (f *SqlSessionFactory) MapperRegistry() *MapperRegistry { return f.mapperRegistry }

// TypeAliasRegistry 类型别名注册表
func (f *SqlSessionFactory) TypeAliasRegistry() *TypeAliasRegistry { return f.typeAliasRegistry }

// TypeHandlerRegistry 类型处理器注册表
func (f *SqlSessionFactory) TypeHandlerRegistry() *TypeHandlerRegistry { return f.typeHandlers }

// ReadRouter 只读副本路由器（未配置副本时 Len() == 0）
func (f *SqlSessionFactory) ReadRouter() *ReadRouter { return f.readRouter }

// Lifecycle 实体生命周期配置（自动时间戳 / 逻辑删除）
func (f *SqlSessionFactory) Lifecycle() LifecycleConfig { return f.lifecycle }

// QueryCache 工厂级共享查询缓存（namespace 粒度 L2）
func (f *SqlSessionFactory) QueryCache() *QueryCache { return f.cache }

// Config 配置
func (f *SqlSessionFactory) Config() *Config { return f.config }

// BaseDir 基准目录
func (f *SqlSessionFactory) BaseDir() string { return f.baseDir }

// MapperCount 已注册的 Mapper 数量
func (f *SqlSessionFactory) MapperCount() int { return f.mapperRegistry.Len() }

// Namespaces 所有已注册的 namespace
func (f *SqlSessionFactory) Namespaces() []string { return f.mapperRegistry.Namespaces() }

// HotReloadEnabled 热重载是否启用
func (f *SqlSessionFactory) HotReloadEnabled() bool {
	return f.watcher != nil && f.watcher.IsRunning()
}

// OpenSession 打开一个新的 SqlSession（请求级，共享工厂的连接池、注册表、
// 查询缓存与生命周期配置）
func (f *SqlSessionFactory) OpenSession() *SqlSession {
	return newSqlSession(f.environment, f.mapperRegistry, f.typeAliasRegistry,
		f.typeHandlers, f.sqlLog, f.bus, f.readRouter, f.lifecycle, f.cache)
}

// Close 关闭工厂，释放连接池、只读副本与热重载资源
func (f *SqlSessionFactory) Close() error {
	if f.watcher != nil {
		f.watcher.Close()
		f.watcher = nil
	}
	if f.readRouter != nil {
		f.readRouter.Close()
	}
	return f.environment.Close()
}

// ─── 常用类型别名（core 层再导出，应用免双包导入）──────────────────

// Params 通用参数表（列名/属性名 → 值）
type Params = core.Params

// BoundSql 绑定后的 SQL（含 ? 占位符的 SQL + 有序参数列表）
type BoundSql = core.BoundSql

// Mapper 已解析的 MyBatis 映射文件
type Mapper = core.Mapper

// ResultMap 结果映射
type ResultMap = core.ResultMap

// SqlStatement SQL 语句模型
type SqlStatement = core.SqlStatement

// ParseMapper 解析 XML 字符串为 Mapper（core 便捷函数再导出）
var ParseMapper = core.ParseMapper
