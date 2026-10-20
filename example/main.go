// higo-mapper 使用示例：sqlite 内存库上的完整 CRUD / 事务 / 事件 / 日志 / 流式查询。
//
// 运行（项目根目录）：
//
//	go run ./example
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	higomapper "github.com/dunpju/higo-mapper"
)

// User 用户模型（json tag 与查询列名对应）
type User struct {
	ID    int64   `json:"id"`
	Name  string  `json:"name"`
	Age   int64   `json:"age"`
	Email *string `json:"email"`
}

// Product 商品模型（price 以字符串承载金额；created_at/updated_at 自动填充）
type Product struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Price     string `json:"price"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func main() {
	// SQL 执行日志输出到 stderr
	higomapper.SetLogger(log.New(os.Stderr, "", log.Ltime))

	ctx := context.Background()

	// 分层配置：TOML 文件 → 环境变量覆盖（HIGO_MAPPER_* 优先级最高）。
	// 编程式回退链上顺带开启自动时间戳与逻辑删除（v0.3+ 特性演示）
	config, err := higomapper.LoadConfigFile("example/higo-mapper.toml")
	if err != nil {
		config = higomapper.DefaultConfig(). // 无配置文件时用默认 + 编程式
							WithEnvironment(higomapper.EnvironmentConfig{
				Driver: "sqlite", URL: "sqlite::memory:",
				PoolMaxConnections: 1, PoolMinConnections: 1,
			}).
			WithMapperPaths([]string{"example/mappers/**/*.xml"}).
			EnableAutoTimestamps(). // insert/update 自动填充 created_at/updated_at
			EnableLogicalDelete()   // session.QueryBuilder 预置 is_delete = 0 过滤
	}
	config, err = config.WithEnvOverrides()
	if err != nil {
		fatal("应用环境变量覆盖失败: %v", err)
	}

	// 构建工厂（连接池 + mapper 加载 + 事件总线）
	factory, err := higomapper.NewSqlSessionFactory(ctx, config, ".")
	if err != nil {
		fatal("构建工厂失败: %v", err)
	}
	defer factory.Close()

	// SQL 生命周期事件监听（观察者：审计/慢查询统计等）
	beforeCount, afterCount := 0, 0
	higomapper.On(factory.EventBus(), func(e *higomapper.BeforeSqlEvent) {
		beforeCount++
		fmt.Printf("[事件] 执行前 %v: %s\n", e.Kind, e.RawSQL)
	})
	higomapper.On(factory.EventBus(), func(e *higomapper.AfterSqlEvent) {
		afterCount++
		if !e.Outcome.IsOK() {
			fmt.Printf("[事件] 执行失败: %s\n", e.Outcome.Err)
		}
	})

	session := factory.OpenSession()
	defer session.Close()

	// 建表（is_delete 列配合逻辑删除配置；created_at 由自动时间戳填充）
	if _, err := session.DB().ExecContext(ctx,
		`CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			age INTEGER,
			email TEXT,
			created_at TEXT,
			is_delete INTEGER DEFAULT 0
		)`); err != nil {
		fatal("建表失败: %v", err)
	}

	const ns = "com.example.dao.UserDao"

	// ─── 插入 ───────────────────────────────────────────────
	id, err := session.Insert(ctx, ns, "insert", map[string]any{
		"name": "张三", "age": 28, "email": "zhangsan@example.com",
	})
	must(err)
	fmt.Printf("\n插入张三，自增主键 = %d\n", *id)

	// struct 参数（json tag 即参数名）
	_, err = session.Insert(ctx, ns, "insert", User{Name: "李四", Age: 35})
	must(err)

	// 批量插入（foreach）。注意：缺失的参数键会被渲染成 /* MISSING:.. */ 标记，
	// 因此需显式传 nil 让列以 NULL 绑定。
	_, err = session.Insert(ctx, ns, "batchInsert", map[string]any{
		"list": []any{
			map[string]any{"name": "王五", "age": 22, "email": nil},
			map[string]any{"name": "赵六", "age": 41, "email": "zhaoliu@example.com"},
		},
	})
	must(err)
	fmt.Printf("批量插入完成\n")

	// ─── 查询 ───────────────────────────────────────────────
	user, err := higomapper.SelectOne[User](ctx, session, ns, "findById", map[string]any{"id": *id})
	must(err)
	fmt.Printf("\nfindById → #%d %s（%d 岁，%s）\n", user.ID, user.Name, user.Age, emailOf(user.Email))

	users, err := higomapper.SelectList[User](ctx, session, ns, "findAll", nil)
	must(err)
	fmt.Printf("\nfindAll → %d 行:\n", len(users))
	for _, u := range users {
		fmt.Printf("  #%d %s（%d 岁，%s）\n", u.ID, u.Name, u.Age, emailOf(u.Email))
	}

	// 动态 SQL：<if> 拼接 WHERE
	adults, err := higomapper.SelectList[User](ctx, session, ns, "findAll", map[string]any{"minAge": int64(30)})
	must(err)
	fmt.Printf("\nfindAll(minAge=30) → %d 行（仅 30 岁及以上）\n", len(adults))

	// ${} 内联：动态排序列
	sorted, err := higomapper.SelectList[User](ctx, session, ns, "findAllOrderBy", map[string]any{"col": "age DESC"})
	must(err)
	fmt.Printf("\n按年龄倒序第一行 → %s\n", sorted[0].Name)

	// ─── 流式查询（大结果集低内存）──────────────────────────
	names := 0
	err = higomapper.SelectForEach[map[string]any](ctx, session, ns, "findNames", nil, func(row *map[string]any) error {
		names++
		return nil
	})
	must(err)
	fmt.Printf("\n流式遍历 name 列 → %d 行\n", names)

	// ─── 更新 / 删除 ────────────────────────────────────────
	n, err := session.Update(ctx, ns, "update", map[string]any{"id": int64(2), "age": int64(36)})
	must(err)
	fmt.Printf("\n更新李四年龄 → 受影响 %d 行\n", n)

	n, err = session.Delete(ctx, ns, "deleteById", map[string]any{"id": int64(4)})
	must(err)
	fmt.Printf("删除赵六 → 受影响 %d 行\n", n)

	// ─── 事务 ───────────────────────────────────────────────
	// 与 Rust 一致：Commit/Rollback 会终结会话（Rust 中按值消费 self）
	must(session.Begin(ctx))
	_, err = session.Insert(ctx, ns, "insert", map[string]any{"name": "临时", "age": 1, "email": nil})
	must(err)
	must(session.Rollback()) // 回滚 → 「临时」不落库

	verify := factory.OpenSession()
	cnt, err := higomapper.SelectList[User](ctx, verify, ns, "findAll", nil)
	must(err)
	fmt.Printf("\n事务回滚后仍有 %d 行（临时记录未写入）\n", len(cnt))

	// ─── BoundSql：参数化 SQL 预览 ─────────────────────────
	bound, err := verify.BuildBoundSql(ns, "findById", map[string]any{"id": int64(1)})
	must(err)
	fmt.Printf("\nBoundSql: %s\n  参数: %v\n", bound.SQL, bound.Parameters)

	// ═══ v0.3 / v0.4 新特性 ═══════════════════════════════

	// ─── 批量插入（InsertBatch：占位符预算自动分块 + 原子提交）────
	var newUsers []User
	for i := 0; i < 7; i++ {
		newUsers = append(newUsers, User{Name: fmt.Sprintf("批量-%d", i+1), Age: int64(20 + i)})
	}
	n, err = higomapper.InsertBatch[User](ctx, verify, ns, "batchInsert", newUsers)
	must(err)
	fmt.Printf("\nInsertBatch 插入 %d 行（超占位符预算时自动分块）\n", n)

	// ─── 分页（count 子查询 + LIMIT/OFFSET 追加）─────────────
	page, err := higomapper.SelectPage[User](ctx, verify, ns, "findAll", nil,
		higomapper.NewPageRequest(2, 3))
	must(err)
	fmt.Printf("\n分页 findAll 第 %d/%d 页（每页 %d）→ 当前 %d 行 / 共 %d 行\n",
		page.PageNo, page.TotalPages(), page.PageSize, len(page.Records), page.Total)

	// ─── QueryBuilder：免 XML 的链式查询（值全参数化）─────────
	qbBound, err := verify.QueryBuilder("users").
		Field("id").Field("name").Field("age").
		WhereBetween("age", 20, 40).
		OrderDesc("id").
		Limit(3).
		Build()
	must(err)
	young, err := higomapper.SelectBound[User](ctx, verify, qbBound)
	must(err)
	youngNames := make([]string, 0, len(young))
	for _, u := range young {
		youngNames = append(youngNames, u.Name)
	}
	cntBound, err := verify.QueryBuilder("users").WhereBetween("age", 20, 40).BuildCount()
	must(err)
	totalYoung, err := higomapper.CountBound(ctx, verify, cntBound)
	must(err)
	fmt.Printf("\nQueryBuilder 20-40 岁共 %d 人，按 id 倒序前 3: %v\n", totalYoung, youngNames)

	// ─── L2 缓存 + 自动时间戳 + 逻辑删除（ProductDao）────────
	const pns = "com.example.dao.ProductDao"
	if _, err := verify.DB().ExecContext(ctx,
		`CREATE TABLE products (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			price TEXT,
			created_at TEXT,
			updated_at TEXT,
			is_delete INTEGER DEFAULT 0
		)`); err != nil {
		fatal("建表失败: %v", err)
	}

	// 自动时间戳：insert 缺失的 created_at/updated_at 由 session 填充
	for _, p := range []struct{ name, price string }{{"键盘", "199.00"}, {"鼠标", "89.00"}} {
		_, err = verify.Insert(ctx, pns, "insert", map[string]any{"name": p.name, "price": p.price})
		must(err)
	}
	prods, err := higomapper.SelectList[Product](ctx, verify, pns, "findAll", nil)
	must(err)
	first := prods[0]
	// 首查已执行并写入缓存；此处展示自动时间戳
	fmt.Printf("\n自动时间戳：%s ¥%s @ %s（created_at = updated_at）\n", first.Name, first.Price, first.CreatedAt)

	// 缓存：同参查询命中 <cache/> 时不再执行 SQL（事件数不增长）
	eventsBefore := afterCount
	_, err = higomapper.SelectList[Product](ctx, verify, pns, "findAll", nil)
	must(err)
	_, err = higomapper.SelectList[Product](ctx, verify, pns, "findAll", nil)
	must(err)
	fmt.Printf("再查 findAll 两次，事件仅 +%d（两次均命中 <cache/>，未执行 SQL）\n",
		afterCount-eventsBefore)

	// 写语句 → namespace 缓存整体失效 + updated_at 自动刷新
	// （等 1.1s 跨过秒级时间戳，让 updated_at 与 created_at 可区分）
	time.Sleep(1100 * time.Millisecond)
	n, err = verify.Update(ctx, pns, "updatePrice", map[string]any{"id": first.ID, "price": "179.00"})
	must(err)
	prods, err = higomapper.SelectList[Product](ctx, verify, pns, "findAll", nil)
	must(err)
	for _, p := range prods {
		fmt.Printf("缓存失效后重查：%s ¥%s（updated_at = %s）\n", p.Name, p.Price, p.UpdatedAt)
	}

	// 逻辑删除：软删鼠标后，QueryBuilder 自动过滤已删除行
	if _, err := verify.DB().ExecContext(ctx,
		"UPDATE products SET is_delete = 1 WHERE name = '鼠标'"); err != nil {
		fatal("软删失败: %v", err)
	}
	pBound, err := verify.QueryBuilder("products").
		Field("id").Field("name").
		WhereLike("name", "%").
		OrderAsc("id").
		Build()
	must(err)
	alive, err := higomapper.SelectBound[Product](ctx, verify, pBound)
	must(err)
	fmt.Printf("\n逻辑删除：软删鼠标后查询到 %d 行（is_delete = 0 过滤已预置）\n", len(alive))

	fmt.Printf("\n事件统计：Before=%d After=%d\n", beforeCount, afterCount)
	fmt.Println("\n✅ 示例执行完毕")
	_ = time.Now()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// emailOf 可空邮箱的可读打印
func emailOf(e *string) string {
	if e == nil {
		return "<NULL>"
	}
	return *e
}

func fatal(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
	os.Exit(1)
}
