# higo-mapper

用 Go 实现的 [MyBatis](https://mybatis.org/mybatis-3/) 风格 ORM 框架。解析 MyBatis XML 映射文件，动态组装参数化 SQL，经 `database/sql` 执行并映射结果——泛型 API 提供编译期类型安全。

移植自 Rust 版 [hirust-mapper](https://github.com/dunpju/hirust-mapper)，语义与之一致。

## 特性

- **完整动态 SQL** — `<if>` / `<choose>` / `<when>` / `<otherwise>` / `<foreach>` / `<where>` / `<set>` / `<trim>` / `<bind>` / `<include>` / `<sql>`
- **两阶段 SQL** — `BuildSQL`（内联）与 `BuildBoundSQL`（参数化 `?` + 参数列表，防注入）并行提供
- **执行层** — 基于 `database/sql`，内置连接池、事务（begin/commit/rollback）、SimpleExecutor
- **流式查询** — `SelectForEach[T]`（回调式）与 `QueryRowsStream`（逐行拉取），大结果集低内存峰值
- **SQL 执行日志** — `sql_log` 开关控制，输出「耗时 + 参数内联的可读 SQL」，`log.Logger` 可替换，支持慢查询阈值
- **事件系统** — 类型化泛型 `On[E]` + `EventBus` 分发器 + `Subscriber` 批量订阅；内置 SQL 执行前/后生命周期事件，并发安全、无监听器零开销
- **类型处理** — TypeHandler 体系（int64/int32/float64/bool/string/time），`any`（规范化）通用中间表示
- **ResultMap 嵌套映射** — `<association>` 一对一、`<collection>` 一对多分组、`<id>` 身份、`<selectKey>` 解析
- **条件增强** — 支持 `.size()` / `.isEmpty()` 方法调用与布尔字面量
- **热重载** — fsnotify 监控 XML 变更，去抖后原子替换（开发期零重启）
- **泛型 API** — `SelectOne[T]` / `SelectList[T]` / `MapperProxy[T]`，编译期类型安全
- **多数据库** — mysql / postgres / sqlite（纯 Go 驱动，无 cgo，Windows 友好）

## 快速开始

### 添加依赖

```sh
go get github.com/dunpju/higo-mapper
```

### 运行时 API

```go
package main

import (
    "context"

    higomapper "github.com/dunpju/higo-mapper"
)

type User struct {
    ID   int64   `json:"id"`
    Name string  `json:"name"`
    Age  int64   `json:"age"`
}

func main() {
    config := higomapper.DefaultConfig().
        WithEnvironment(higomapper.EnvironmentConfig{
            Driver: "sqlite", URL: "sqlite::memory:",
            PoolMaxConnections: 10, PoolMinConnections: 1,
        }).
        WithMapperPaths([]string{"mappers/**/*.xml"})

    factory, err := higomapper.NewSqlSessionFactory(context.Background(), config, ".")
    if err != nil {
        panic(err)
    }
    defer factory.Close()

    session := factory.OpenSession()
    defer session.Close()

    const ns = "app.UserDao"
    ctx := context.Background()

    // 插入（参数为 map；struct 参数以 json tag 为键）
    id, err := session.Insert(ctx, ns, "insert", map[string]any{
        "name": "张三", "age": 30,
    })

    // 查询：泛型 API，编译期类型安全
    user, err := higomapper.SelectOne[User](ctx, session, ns, "findById",
        map[string]any{"id": *id})

    // 事务（Commit/Rollback 会终结会话，与 Rust 版按值消费 self 的语义一致）
    tx := factory.OpenSession()
    if err := tx.Begin(ctx); err != nil {
        panic(err)
    }
    if _, err := tx.Insert(ctx, ns, "insert", map[string]any{"name": "李四", "age": 25}); err != nil {
        panic(err)
    }
    if err := tx.Commit(); err != nil {
        panic(err)
    }
}
```

### 泛型 DAO 代理（`MapperProxy`）

```go
dao, err := higomapper.MapperOf[User](session, "app.UserDao")

users, err := dao.SelectList(ctx, "findAll", nil)
user, err := dao.SelectOne(ctx, "findById", map[string]any{"id": int64(1)})
n, err := dao.Update(ctx, "setAge", map[string]any{"id": 1, "age": 31})
```

### 流式查询（大结果集）

对于大结果集，用流式接口逐行拉取，避免 `SelectList` 一次性物化整表导致的内存峰值：

```go
// 回调式：逐行处理；返回 Err 可提前终止（错误向上透传）
err := higomapper.SelectForEach[User](ctx, session, ns, "findAll", nil, func(u *User) error {
    println(u.Name)
    return nil
})

// executor 级：直接持有 map 行流
bound, err := session.BuildBoundSql(ns, "findAll", nil)
rows, err := session.Executor().QueryRowsStream(ctx, session.Querier(), bound)
```

- 流式仅支持普通列映射，**不支持 ResultMap 嵌套分组**（分组需聚集全部行，与流式语义冲突）。
- 流式查询不派发 SQL 事件（与 Rust 版语义一致）。
- 回调返回错误会向上传递并终止流；空结果集不触发回调。
- `SelectOne` / `SelectList` 行为不变（多行时 `SelectOne` 返回 `ErrTooManyRows`）。

## Mapper XML 格式

```xml
<mapper namespace="app.UserDao">
    <resultMap id="userMap" type="User">
        <id property="id" column="id"/>
        <result property="name" column="user_name"/>
        <association property="dept" javaType="Dept">
            <id property="id" column="dept_id"/>
            <result property="name" column="dept_name"/>
        </association>
        <collection property="roles" ofType="Role">
            <id property="id" column="role_id"/>
        </collection>
    </resultMap>

    <select id="findById" resultMap="userMap">
        SELECT * FROM users WHERE 1=1
        <if test="id != null">AND id = #{id}</if>
        <if test="ids != null and ids.size() > 0">
            AND id IN (<foreach collection="ids" item="x" separator=",">#{x}</foreach>)
        </if>
    </select>

    <insert id="insertWithKey">
        <selectKey keyProperty="id" resultType="i64" order="AFTER">
            SELECT LAST_INSERT_ID()
        </selectKey>
        INSERT INTO users (name) VALUES (#{name})
    </insert>
</mapper>
```

- `#{param}` → 参数化 `?` 占位符（防注入，推荐）
- `${param}` → 原样内联（动态表名/排序列等）
- 条件支持 `and`/`or`、`= != > < >= <=`、`.size()`/`.isEmpty()`、`== true/false`
- 参数缺失时 SQL 中渲染为 `/* MISSING:#param */` 标记且不产生 `?`（便于定位漏传参数；传 `nil` 则正常以 NULL 绑定）

## 配置文件（`higo-mapper.toml`）

```toml
[environment]
driver = "mysql"                       # mysql | postgres | sqlite
url = "mysql://user:pass@host:3306/db"
pool_max_connections = 10
pool_min_connections = 2

[settings]
mapper_paths = ["mappers/**/*.xml"]
mapper_refresh_interval_ms = 3000      # 热重载间隔，0 = 禁用
sql_log = true                         # SQL 执行日志开关（默认 false）
sql_log_slow_threshold_ms = 0          # 慢查询阈值(ms)：仅记录耗时≥此值的 SQL；0 = 全部

[type_aliases]
"int" = "int32"
"long" = "int64"
```

URL 形式统一转换：

| URL | 驱动 DSN |
|-----|----------|
| `mysql://u:p@host:3306/db?x=y` | `u:p@tcp(host:3306)/db?x=y` |
| `postgres://u:p@host:5432/db` | 原样传给 pgx（`?` 自动重写为 `$n`） |
| `sqlite::memory:` | `file::memory:?cache=shared`（连接池共享内存库） |
| `sqlite:///path/app.db` | `file:/path/app.db` |

## 配置优先级与环境变量

配置来源分三层，优先级 **环境变量 > 编程设置 > TOML 默认值**。环境变量只在**设置时**覆盖对应字段，
未设置则保留编程/TOML 值——适合容器/CI 部署时无需改 TOML 即可覆盖连接、日志等。

```go
config, err := higomapper.LoadConfigFile("higo-mapper.toml") // 1. TOML
if err != nil {
    config = higomapper.DefaultConfig().WithURL("programmatic-url") // 2. 编程
}
config, err = config.WithEnvOverrides()                          // 3. env（最高）
```

也可一站式加载：`higomapper.LoadLayeredConfig("higo-mapper.toml")`（= TOML + env）。

支持的环境变量：

| 变量 | 覆盖字段 | 示例 |
|------|----------|------|
| `HIGO_MAPPER_DRIVER` | `environment.driver` | `postgres` |
| `HIGO_MAPPER_URL`（或 `DATABASE_URL`） | `environment.url` | `postgres://u:p@h/db` |
| `HIGO_MAPPER_POOL_MAX` / `_POOL_MIN` | 连接池大小 | `20` / `2` |
| `HIGO_MAPPER_PATHS` | `settings.mapper_paths` | `a/*.xml,b/**/*.xml` |
| `HIGO_MAPPER_REFRESH_MS` | 热重载间隔 | `3000` |
| `HIGO_MAPPER_SQL_LOG` | SQL 日志开关 | `true` |
| `HIGO_MAPPER_SQL_LOG_SLOW_MS` | 慢查询阈值 | `100` |
| `HIGO_MAPPER_TYPE_ALIASES` | 类型别名（合并） | `int=int32,long=int64` |

```sh
# 12-factor 部署示例：仅用环境变量覆盖连接与慢查询阈值
DATABASE_URL=postgres://prod:secret@db:5432/app \
HIGO_MAPPER_SQL_LOG=true \
HIGO_MAPPER_SQL_LOG_SLOW_MS=200 \
    ./your_app
```

非法值（如非数字、非布尔）返回 `ErrConfig` 错误，不静默吞错。

## SQL 执行日志

在 `[settings]` 中设置 `sql_log = true`（或编程式 `WithSqlLog(true)`），即可对每次 SQL 执行输出「耗时 + 参数内联的可读 SQL」：

```text
15:32:03 Consume Time: 44 ms
 Execute SQL:: SELECT `examId`,`examName` FROM exam WHERE (`examId` IN (69902) AND `isDelete` = 0)
```

- 默认输出到 stderr；可用 `higomapper.SetLogger(log.New(os.Stdout, "", log.LstdFlags))` 替换（如接 slog 适配器）。
- 参数按 `?` 顺序内联（字符串加引号、`NULL`/布尔/数字原样）。
- XML 中多行/缩进书写的 SQL，连续空白在日志输出时折叠为单个空格，保证每条日志单行且紧凑。
- `sql_log_slow_threshold_ms > 0` 时只记录达到阈值的慢查询；`0` 记录全部。
- 关闭 `sql_log` 且无 `AfterSqlEvent` 监听器时，执行点不做计时之外的任何额外工作。

## 事件系统

类型化的事件监听与订阅（灵感来自 ThinkPHP 模型事件，按 Go 泛型实现）：

- **`On[E]`** — `bus.On(func(e *E){...})` 即订阅任意事件类型（含自定义业务事件）。
- **`Subscriber`** 接口 — 在一个实现里批量注册多个事件（对应 ThinkPHP 的「事件订阅」）。
- **`EventBus`** — 并发安全的类型擦除分发器；派发时先快照监听器列表、**释放锁后再回调**（监听器内可安全重入订阅/派发）；**无监听器时经原子读零开销跳过**。

内置 ORM 生命周期事件，在 SQL 执行点自动派发：`BeforeSqlEvent`（执行前）/ `AfterSqlEvent`（含耗时与 `SqlOutcome` 结果摘要）。

```go
factory, _ := higomapper.NewSqlSessionFactory(ctx, config, ".")

// 1) 自定义事件 + 监听
type LoginEvent struct{ User string }
higomapper.On(factory.EventBus(), func(e *LoginEvent) {
    println(e.User, "登录")
})

// 2) SQL 生命周期监听（审计/慢查询统计）
count := 0
higomapper.On(factory.EventBus(), func(e *higomapper.AfterSqlEvent) {
    count++
    if !e.Outcome.IsOK() {
        println("执行失败:", e.Outcome.Err)
    }
})

session := factory.OpenSession()
_, _ = session.Insert(ctx, "app.U", "insert", user)        // 触发 Before/After SQL 事件
higomapper.Dispatch(factory.EventBus(), LoginEvent{User: "张三"}) // 派发自定义事件
```

> 监听器为**同步回调**（在执行点内联调用）；耗时工作请在监听器内起 goroutine。
> 流式查询暂不派发 SQL 事件（按行流式的「耗时」语义不明确）。

## 热重载

`[settings] mapper_refresh_interval_ms > 0`（或 `WithHotReload(ms)`）即启用：fsnotify 递归监视 mapper 目录，
变更事件去抖（最小 50ms 安静期）后批量重解析并原子替换（写锁），失败仅告警不阻断。开发期改 XML 零重启。

```go
config := higomapper.DefaultConfig().
    WithMapperPaths([]string{"mappers/**/*.xml"}).
    WithHotReload(3000)
```

## 示例

仓库内含可运行示例（sqlite 内存库，无需外部数据库）：

```sh
go run ./example    # CRUD + 动态 SQL + 流式 + 事务 + 事件 + 日志
```

## 架构

| 包 | 职责 |
|-----|------|
| `core/` | XML 解析 + 动态 SQL 生成 + BoundSql（无 DB 依赖） |
| 根包 `higomapper` | 配置 / Session / Executor / TypeHandler / 热重载 / ResultMap 映射 |

> Rust 版的 `#[dao]` proc-macro（按方法签名生成类型化 DAO）在 Go 中对应 `go:generate` 代码生成器，**本轮未实现**，留待后续版本。

依赖：[database/sql](https://pkg.go.dev/database/sql)（执行层）、[go-sql-driver/mysql](https://github.com/go-sql-driver/mysql) / [pgx](https://github.com/jackc/pgx) v5 / [modernc.org/sqlite](https://gitlab.com/cznic/sqlite)（驱动）、[fsnotify](https://github.com/fsnotify/fsnotify)（热重载）、[BurntSushi/toml](https://github.com/BurntSushi/toml)（配置）、[doublestar](https://github.com/bmatcuk/doublestar) v4（`**` glob）。

## 测试

```sh
go test ./...    # 131 个测试（core 单测 + sqlite 内存库 e2e：CRUD/事务/流式/嵌套映射/事件/SQL日志/热重载/配置）
```

## License

MIT
