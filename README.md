# higo-mapper

用 Go 实现的 [MyBatis](https://mybatis.org/mybatis-3/) 风格 ORM 框架。解析 MyBatis XML 映射文件，动态组装参数化 SQL，经 `database/sql` 执行并映射结果——泛型 API 提供编译期类型安全。

移植自 Rust 版 [hirust-mapper](https://github.com/dunpju/hirust-mapper)（特性对齐 v0.4），语义与之一致；`#[dao]` proc-macro 层暂未移植（见[架构](#架构)）。

## 特性

- **完整动态 SQL** — `<if>` / `<choose>` / `<when>` / `<otherwise>` / `<foreach>` / `<where>` / `<set>` / `<trim>` / `<bind>` / `<include>` / `<sql>`
- **两阶段 SQL** — `BuildSQL`（内联）与 `BuildBoundSQL`（参数化 `?` + 参数列表，防注入）并行提供
- **执行层** — 基于 `database/sql`，内置连接池、事务（begin/commit/rollback）、SimpleExecutor
- **流式查询** — `SelectForEach[T]`（回调式）与 `QueryRowsStream`（逐行拉取），大结果集低内存峰值
- **分页查询** — `SelectPage`（PageHelper 式）：自动 count（剥顶层 ORDER BY + 子查询包装，GROUP BY/UNION 语义正确）+ `LIMIT ? OFFSET ?`（三方言通用），返回 `Page[T]`（TotalPages/HasNext/HasPrev）
- **批量操作** — `InsertBatch`（多行 VALUES 单语句，占位符超 900 自动分块）与 `UpdateBatch`，整体原子（并入会话事务或自动开启专属事务，失败回滚）
- **QueryBuilder** — ThinkPHP 风格链式条件查询（`WhereEq/In/Like/Between`、排序、分页组合），标识符白名单校验防注入，产物 `BoundSql` 与 XML 语句走同一执行层（事务/日志/事件全复用）
- **实体生命周期** — 自动时间戳（insert/update/批量写路径填充缺失的 `created_at`/`updated_at`，datetime/millis/seconds 格式）+ 逻辑删除（`session.QueryBuilder` 预置未删除过滤）+ 乐观锁模式（文档化）；保守方案不改写 SQL，默认全关零干扰
- **二级缓存** — namespace 级查询缓存：`<cache/>` 标签开启，写操作整体失效 + `flushInterval` TTL + LRU 容量，缓存键含完整比对防哈希碰撞
- **读写分离** — `[[read_environments]]` 配置多从库轮询路由；读走从库，写与事务内读始终走主库
- **代码生成 CLI** — `cmd/higo-mapper` 连库反向生成 entity struct（json tag 映射列名）与 mapper XML 骨架（MySQL information_schema / SQLite PRAGMA 内省），PHP 存量库迁移利器
- **SQL 执行日志** — `sql_log` 开关控制，输出「耗时 + 参数内联的可读 SQL」，`log.Logger` 可替换，支持慢查询阈值
- **事件系统** — 类型化泛型 `On[E]` + `EventBus` 分发器 + `Subscriber` 批量订阅；内置 SQL 执行前/后生命周期事件，并发安全、无监听器零开销
- **类型处理** — TypeHandler 体系（int64/int32/float64/bool/string/time + json/decimal 扩展处理器），`any`（规范化）通用中间表示；ResultMap `goType` 接线到处理器注册表
- **ResultMap 嵌套映射** — `<association>` 一对一、`<collection>` 一对多分组、`<id>` 身份、`<selectKey>` 解析
- **条件增强** — 支持 `.size()` / `.isEmpty()` 方法调用与布尔字面量
- **热重载** — fsnotify 监控 XML 变更，去抖后原子替换（开发期零重启）
- **泛型 API** — `SelectOne[T]` / `SelectList[T]` / `SelectPage[T]` / `SelectBound[T]` / `MapperProxy[T]`，编译期类型安全
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
- `SelectOne` / `SelectList` 行为不变（多行时 `SelectOne` 返回 `ErrTooManyRows`；ResultMap 嵌套映射按**分组后的父对象数**计数）。

### 分页查询

`SelectPage` 自动执行 count + 物理分页，返回 `Page[T]`：

```go
page, err := higomapper.SelectPage[User](ctx, session, ns, "findAll", nil,
    higomapper.NewPageRequest(2, 10))
if err != nil {
    panic(err)
}
fmt.Printf("总 %d 行 / %d 页，本页 %d 条，有下一页: %v\n",
    page.Total, page.TotalPages(), len(page.Records), page.HasNext())
```

- 数据页：原 SQL 追加 `LIMIT ? OFFSET ?`（MySQL/PostgreSQL/SQLite 通用）；总数：剥顶层
  `ORDER BY` 后包 `SELECT COUNT(*) FROM (…) AS __higo_count_t`，`GROUP BY` / `UNION`
  场景计数语义正确（数分组数 / 合并行数）
- `total = 0` 时直接返回空页，省去第二次查询；ResultMap 嵌套映射可正常配合（分组在页内）
- 原语句**不得含顶层 `LIMIT`/`OFFSET`**（子查询内不受限），否则报 `ErrPagination` 错误；
  `PageNo`/`PageSize` 从 1 起校验、偏移量溢出显式报错
- `MapperProxy` 同名代理：`dao.SelectPage(ctx, "findAll", nil, higomapper.NewPageRequest(2, 10))`
- ResultMap 分组场景：`LIMIT` 作用于原始 join 行，页内合并后 `Records` 数可能小于
  `PageSize`（`Total` 按分组数计）——需严格每页 N 个父对象时改用 QueryBuilder 手写外层分页

### 批量操作

XML 侧用 `<foreach collection="list">` 拼多值 VALUES（MyBatis 集合参数默认命名 `list`）：

```xml
<insert id="insertBatch">
    INSERT INTO users (name, age) VALUES
    <foreach collection="list" item="u" separator=",">(#{u.name}, #{u.age})</foreach>
</insert>
```

```go
// 多行 VALUES 单语句；占位符超过 900 自动按比例分块，循环直至全部插入
n, err := higomapper.InsertBatch[User](ctx, session, ns, "insertBatch", users) // users []User

// 逐条执行同一语句（#{age}/#{id} 按实体字段引用）
n, err = higomapper.UpdateBatch[User](ctx, session, ns, "updateAgeById", updates)
```

- **整体原子**：会话已开启事务时并入（由调用方提交/回滚）；否则自动开启专属事务，
  任一失败立即回滚并返回错误
- 分块预算 900 兼容旧版 SQLite 的 999 变量上限（远低于 PG 65535 / MySQL 常见配额）
- 每块/每条独立产生 SQL 日志与 Before/After 事件；空列表返回 `(0, nil)` 不发 SQL；
  批量不执行 `<selectKey>`（返回受影响行数合计而非主键）
- 自动时间戳开启时对每个实体填充缺失的 `created_at`/`updated_at`

### QueryBuilder 链式查询

简单查询免 XML 的编程式 API（ThinkPHP 风格），与 XML 语句走同一执行层——事务路由、
SQL 日志、事件全复用：

```go
// SELECT id, name FROM users WHERE status = ? AND name LIKE ? AND age BETWEEN ? AND ?
//   AND deleted_at IS NULL ORDER BY id DESC LIMIT ?
bound, err := higomapper.NewQueryBuilder().
    Table("users").
    Field("id").Field("name").
    WhereEq("status", 1).
    WhereLike("name", "%张%").
    WhereBetween("age", 18, 60).
    WhereNull("deleted_at").
    OrderDesc("id").
    Limit(3).
    Build()

users, err := higomapper.SelectBound[User](ctx, session, bound) // 事务感知执行
cntBound, err := higomapper.NewQueryBuilder().Table("users").WhereEq("status", 1).BuildCount()
total, err := higomapper.CountBound(ctx, session, cntBound)

// 与分页组合（BuildPage 自带 LIMIT/OFFSET，与手动 Limit/Offset 互斥）
pageBound, err := higomapper.NewQueryBuilder().Table("users").WhereEq("status", 1).
    OrderDesc("id").BuildPage(higomapper.NewPageRequest(2, 10))
```

- **注入安全**：值一律 `?` 参数化；表名/列名/排序字段经白名单校验
  （`[A-Za-z_][A-Za-z0-9_]*`，允许 `.` 分段），排序方向仅 `ASC`/`DESC`
- `WhereRaw("(a = ? OR b = ?)", x, y)` 为逃生舱：片段原样拼接，注入风险自负
- 空集合语义：`WhereIn` 空 → 恒假 `1 = 0`；`WhereNotIn` 空 → 恒真 `1 = 1`（不约束）
- 全部操作符：`WhereEq/Ne/Gt/Lt/Ge/Le/Like/NotLike/In/NotIn/Between/Null/NotNull`
- 链中调用不中断，错误统一延迟到 `Build()` 返回（便于长链书写）

### JSON 与 Decimal 类型

ResultMap 声明 `goType` 即接线到 TypeHandler 处理器：

```xml
<resultMap id="productMap" type="Product">
    <id property="id" column="id"/>
    <result property="meta" column="meta" goType="json"/>
    <result property="price" column="price" goType="decimal"/>
</resultMap>
```

```go
type Product struct {
    ID    int64          `json:"id"`
    Meta  map[string]any `json:"meta"`  // JSON 列 → 解析后的对象/数组
    Price string         `json:"price"` // 金额以字符串承载，无 f64 精度中转
}
```

- `goType="json"`：读取严格校验（脏数据报 `ErrResultMapping` 错误，`"null"` → nil）；
  参数侧对象/数组自动序列化为 JSON 文本
- `goType="decimal"`：文本严格校验（拒绝 `"1.2.3"`/`"1e5"` 等），整数/浮点列分派转文本，
  绑定前同样校验；金额场景避免二进制浮点精度丢失（`0.1 + 0.2 ≠ 0.3`）
- **PostgreSQL 提示**：pgx 的 `json`/`jsonb` 列可能以非文本形式返回，建议 SQL 侧显式
  转换（`SELECT col::text …` / `CAST(? AS jsonb)`）保证文本往返

### 实体生命周期（自动时间戳 + 逻辑删除）

```toml
[settings.auto_timestamps]            # 默认关闭
enabled = true
format = "datetime"                   # datetime | millis | seconds
created_at_column = "created_at"      # 按库表习惯自定义
updated_at_column = "updated_at"

[settings.logical_delete]             # 默认关闭
enabled = true
column = "is_delete"
normal_value = 0
deleted_value = 1
```

编程式开启：`config.EnableAutoTimestamps()` / `config.EnableLogicalDelete()`（或
`WithAutoTimestamps` / `WithLogicalDelete` 整体替换）。

```go
// insert/update/InsertBatch/UpdateBatch 自动填充「缺失」的时间戳键（显式传值优先）
_, err = session.Insert(ctx, ns, "insert", map[string]any{"name": "张三"})
// UPDATE 仅刷新 updated_at，created_at 保持

// 逻辑删除：经 session 创建的 QueryBuilder 自动预置 is_delete = 0
bound, err := session.QueryBuilder("users"). // 已带未删除过滤
    WhereEq("status", 1).
    Build() // higomapper.NewQueryBuilder() 则不带（显式性）
```

保守方案说明：不改写 XML 语句的 SQL（JOIN/子查询零歧义）；乐观锁用文档化模式——XML 写
`UPDATE … SET version = version + 1 WHERE id = #{id} AND version = #{version}`，
返回受影响行数 `0` 即冲突。

### 二级缓存

```xml
<mapper namespace="app.DictDao">
    <cache flushInterval="60000" size="512"/>
    <select id="findAll">SELECT * FROM dicts</select>
</mapper>
```

- namespace 声明 `<cache/>` 即启用；`SelectList`/`SelectOne` 命中缓存不执行 SQL
  （无日志、无事件）
- 该 namespace 任何写操作成功后整体失效；`flushInterval` 兜底 TTL（毫秒），`size` LRU
  容量（默认 1024）
- 缓存键 = 最终 SQL + 参数规范化 JSON（map 键排序，参数形态等价的调用互相命中）
- 工厂级共享（跨 session）；读多写少的引用型数据适用；分页查询不走缓存

### 读写分离

```toml
[environment]                          # 主库（写 + 事务）
driver = "mysql"
url = "mysql://user:pass@master:3306/db"

[[read_environments]]                  # 从库（读查询轮询路由）
driver = "mysql"
url = "mysql://user:pass@replica1:3306/db"
[[read_environments]]
driver = "mysql"
url = "mysql://user:pass@replica2:3306/db"
```

- 无事务的读（`SelectOne` / `SelectList` / `SelectBound` / `CountBound` / 流式）轮询走从库
- 写操作与**事务内的一切读**始终走主库（事务读必须看到未提交写入）
- 从库列表为空则读全部走主库——既有行为零变化
- 编程式配置：`config.WithReadEnvironments([]higomapper.EnvironmentConfig{…})`

### 代码生成 CLI

```sh
# 连 MySQL 存量库，反向生成 entity + mapper XML
higo-mapper generate --url mysql://user:pass@localhost:3306/mydb \
    --out-dir src/models --xml-dir mappers --namespace app.dao

# 仓库内直接运行；只生成指定表 / 只看不写
go run ./cmd/higo-mapper generate --url sqlite:app.db --tables users,orders --dry-run
```

- 每表生成 `{table}.go`（struct 字段 json tag 精确映射列名、可空非主键列自动 `*T`
  指针、json/decimal 列映射 `any`/`string` 并附提示注释）与 `{Table}Dao.xml`
  （resultMap 含 `goType` + findById/findAll/insert/insertBatch/updateById/deleteById 骨架）
- 方言按 URL scheme 推断（mysql/mariadb/sqlite；postgres 暂不支持内省）；生成物是起点，
  按业务调整

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
                    # + 批量插入 + 分页 + QueryBuilder + 缓存 + 自动时间戳 + 逻辑删除
```

## 架构

| 包 | 职责 |
|-----|------|
| `core/` | XML 解析 + 动态 SQL 生成 + BoundSql（无 DB 依赖） |
| 根包 `higomapper` | 配置 / Session / Executor / TypeHandler / 分页 / QueryBuilder / 二级缓存 / 读写分离 / 生命周期 / 热重载 / ResultMap 映射 |
| `cmd/higo-mapper` | 代码生成 CLI（连库内省 → entity struct + mapper XML 骨架） |

> Rust 版的 `#[dao]` proc-macro（按方法签名生成类型化 DAO）在 Go 中对应 `go:generate` 代码生成器，**本轮未实现**，留待后续版本。

依赖：[database/sql](https://pkg.go.dev/database/sql)（执行层）、[go-sql-driver/mysql](https://github.com/go-sql-driver/mysql) / [pgx](https://github.com/jackc/pgx) v5 / [modernc.org/sqlite](https://gitlab.com/cznic/sqlite)（驱动）、[fsnotify](https://github.com/fsnotify/fsnotify)（热重载）、[BurntSushi/toml](https://github.com/BurntSushi/toml)（配置）、[doublestar](https://github.com/bmatcuk/doublestar) v4（`**` glob）。

## 测试

```sh
go test ./...    # 190 个测试（core 单测 + sqlite 内存库 e2e）
```

集成测试覆盖：CRUD 端到端、事务、流式、SQL 日志、事件、热重载、嵌套映射、
分页、批量（含分块/原子回滚）、QueryBuilder、JSON/Decimal 处理器、
生命周期（时间戳/逻辑删除）、二级缓存（命中/失效/共享）、读写分离（路由/事务强制主库）、
CLI 代码生成（内省/产物/落盘）。

## License

MIT
