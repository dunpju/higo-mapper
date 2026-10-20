package higomapper

// v0.3 / v0.4 新特性 e2e：分页 / QueryBuilder / L2 缓存 / 自动时间戳 /
// 逻辑删除 / 读写分离 / 批量写 / TooManyRows 分组语义。

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mkdirAll / writeFile：测试脚手架缩写
func mkdirAll(dir string) error   { return os.MkdirAll(dir, 0o755) }
func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o644) }

// ─── 分页 ─────────────────────────────────────────────────────────

func TestSelectPageE2E(t *testing.T) {
	factory := setupFactory(t)
	s := factory.OpenSession()
	defer s.Close()
	// 播种 25 行
	for i := 0; i < 25; i++ {
		if _, err := s.Insert(context.Background(), "com.test.UserMapper", "insertUser", map[string]any{
			"name": nameOf(i), "score": float64(i), "active": int64(1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := SelectPage[testUser](context.Background(), s, "com.test.UserMapper", "findAll", nil,
		NewPageRequest(2, 10))
	if err != nil {
		t.Fatalf("SelectPage: %v", err)
	}
	if page.Total != 25 || page.TotalPages() != 3 {
		t.Fatalf("total=%d totalPages=%d, want 25/3", page.Total, page.TotalPages())
	}
	if len(page.Records) != 10 {
		t.Fatalf("第 2 页记录数 = %d, want 10", len(page.Records))
	}
	if !page.HasPrev() || !page.HasNext() {
		t.Fatal("第 2/3 页 HasPrev/HasNext 均应为真")
	}
	if page.Records[0].ID != 11 { // ORDER BY id 保证确定性
		t.Fatalf("第 2 页首条 id = %d, want 11", page.Records[0].ID)
	}

	// 末页不满页
	page, err = SelectPage[testUser](context.Background(), s, "com.test.UserMapper", "findAll", nil,
		NewPageRequest(3, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 5 || page.HasNext() {
		t.Fatalf("末页记录数 = %d hasNext=%v, want 5/false", len(page.Records), page.HasNext())
	}

	// 超范围页 → 空页
	page, err = SelectPage[testUser](context.Background(), s, "com.test.UserMapper", "findAll", nil,
		NewPageRequest(9, 10))
	if err != nil {
		t.Fatal(err)
	}
	if !page.IsEmpty() || page.Total != 25 {
		t.Fatalf("超范围页: empty=%v total=%d", page.IsEmpty(), page.Total)
	}

	// 带参数分页
	page, err = SelectPage[testUser](context.Background(), s, "com.test.UserMapper", "findByCond",
		map[string]any{"minScore": 5}, NewPageRequest(1, 4))
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 20 || len(page.Records) != 4 {
		t.Fatalf("条件分页: total=%d len=%d, want 20/4", page.Total, len(page.Records))
	}

	// 非法分页参数
	if _, err = SelectPage[testUser](context.Background(), s, "com.test.UserMapper", "findAll", nil,
		NewPageRequest(0, 10)); err == nil {
		t.Fatal("page_no=0 应报错")
	}
}

func TestSelectPageRejectsTopLevelLimit(t *testing.T) {
	factory := setupFactory(t)
	s := factory.OpenSession()
	defer s.Close()
	// 语句自带顶层 LIMIT → 报错
	if _, err := SelectPage[testUser](context.Background(), s, "com.test.UserMapper",
		"findLimited", nil, NewPageRequest(1, 10)); err == nil {
		t.Fatal("顶层 LIMIT 语句分页应报错")
	}
}

func nameOf(i int) string { return "u" + string(rune('a'+i%26)) + "_" + itoa(i/26) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ─── QueryBuilder ─────────────────────────────────────────────────

func TestQueryBuilderE2E(t *testing.T) {
	factory := setupFactory(t)
	s := factory.OpenSession()
	defer s.Close()
	seedUsers(t, s)

	bound, err := s.QueryBuilder("users").
		Field("id").Field("name").
		WhereIn("score", 95.5, 80.0).
		OrderDesc("id").
		Build()
	if err != nil {
		t.Fatal(err)
	}
	users, err := SelectBound[testUser](context.Background(), s, bound)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Name != "李四" || users[1].Name != "张三" {
		t.Fatalf("QueryBuilder 查询结果异常（应按 id 降序）: %+v", users)
	}

	// 空集 IN → 恒假
	bound, err = s.QueryBuilder("users").WhereIn("id").Build()
	if err != nil {
		t.Fatal(err)
	}
	users, err = SelectBound[testUser](context.Background(), s, bound)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatalf("空 IN 应回 0 行, got %d", len(users))
	}

	// CountBound
	countBound, err := s.QueryBuilder("users").WhereGe("score", 60.0).BuildCount()
	if err != nil {
		t.Fatal(err)
	}
	total, err := CountBound(context.Background(), s, countBound)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("count = %d, want 3", total)
	}
}

// ─── L2 查询缓存 ──────────────────────────────────────────────────

const cachedMapperXML = `<mapper namespace="com.test.CachedMapper">
    <cache flushInterval="60000" size="100"/>
    <select id="findAll">SELECT id, name, score, active FROM users ORDER BY id</select>
    <select id="findById">SELECT id, name, score, active FROM users WHERE id = #{id}</select>
    <insert id="insertUser">INSERT INTO users (name, score, active) VALUES (#{name}, #{score}, #{active})</insert>
</mapper>`

func setupCachedFactory(t *testing.T) *SqlSessionFactory {
	t.Helper()
	baseDir := t.TempDir()
	mappersDir := filepath.Join(baseDir, "mappers")
	if err := mkdirAll(mappersDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(mappersDir, "CachedMapper.xml"), cachedMapperXML); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig().
		WithEnvironment(EnvironmentConfig{
			Driver: "sqlite", URL: "sqlite::memory:",
			PoolMaxConnections: 1, PoolMinConnections: 1,
		}).
		WithMapperPaths([]string{"mappers/**/*.xml"})
	factory, err := NewSqlSessionFactory(context.Background(), cfg, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { factory.Close() })
	if _, err := factory.Environment().DB().Exec(
		"CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, score REAL, active INTEGER)"); err != nil {
		t.Fatal(err)
	}
	return factory
}

func TestQueryCacheE2E(t *testing.T) {
	factory := setupCachedFactory(t)

	// 事件计数：命中缓存不派发 SQL 事件
	var afterCount int64
	On(factory.EventBus(), func(*AfterSqlEvent) { atomic.AddInt64(&afterCount, 1) })

	s1 := factory.OpenSession()
	if _, err := s1.Insert(context.Background(), "com.test.CachedMapper", "insertUser",
		map[string]any{"name": "a", "score": 1.0, "active": 1}); err != nil {
		t.Fatal(err)
	}
	seedCount := atomic.LoadInt64(&afterCount)

	// 首查：执行 SQL（计数 +1）并写缓存
	users, err := SelectList[testUser](context.Background(), s1, "com.test.CachedMapper", "findAll", nil)
	if err != nil || len(users) != 1 {
		t.Fatalf("首查: %v %d", err, len(users))
	}
	first := atomic.LoadInt64(&afterCount)
	if first != seedCount+1 {
		t.Fatalf("首查应执行 SQL: before=%d after=%d", seedCount, first)
	}

	// 二查：命中缓存（计数不变、数据一致）
	users, err = SelectList[testUser](context.Background(), s1, "com.test.CachedMapper", "findAll", nil)
	if err != nil || len(users) != 1 {
		t.Fatalf("缓存查询: %v %d", err, len(users))
	}
	if atomic.LoadInt64(&afterCount) != first {
		t.Fatal("缓存命中不应执行 SQL")
	}

	// 新会话共享工厂级缓存
	s2 := factory.OpenSession()
	if _, err = SelectList[testUser](context.Background(), s2, "com.test.CachedMapper", "findAll", nil); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&afterCount) != first {
		t.Fatal("跨会话应共享缓存")
	}

	// 不同参数不误命中
	if _, err = SelectOne[testUser](context.Background(), s2, "com.test.CachedMapper", "findById",
		map[string]any{"id": 999}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&afterCount) != first+1 {
		t.Fatal("新参数应查库")
	}
	// 同参数 SelectOne 命中
	if _, err = SelectOne[testUser](context.Background(), s2, "com.test.CachedMapper", "findById",
		map[string]any{"id": 1}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&afterCount) != first+2 {
		t.Fatal("id=1 未曾缓存，应查库")
	}

	// 写语句 → namespace 整体失效
	if _, err = s2.Insert(context.Background(), "com.test.CachedMapper", "insertUser",
		map[string]any{"name": "b", "score": 2.0, "active": 0}); err != nil {
		t.Fatal(err)
	}
	users, err = SelectList[testUser](context.Background(), s2, "com.test.CachedMapper", "findAll", nil)
	if err != nil || len(users) != 2 {
		t.Fatalf("失效后重查: %v %d", err, len(users))
	}
	if atomic.LoadInt64(&afterCount) < first+4 {
		t.Fatal("失效后应重新执行 SQL")
	}

	// 无 <cache/> 的 namespace 不缓存（factory 默认 setup 即无 cache）
	s1.Close()
	s2.Close()
}

func TestQueryCacheSkippedWithoutCacheTag(t *testing.T) {
	factory := setupFactory(t) // userMapperXML 无 <cache/>
	var afterCount int64
	On(factory.EventBus(), func(*AfterSqlEvent) { atomic.AddInt64(&afterCount, 1) })
	s := factory.OpenSession()
	defer s.Close()
	seedUsers(t, s)
	base := atomic.LoadInt64(&afterCount)
	for i := 0; i < 3; i++ {
		if _, err := SelectList[testUser](context.Background(), s, "com.test.UserMapper", "findAll", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt64(&afterCount); got != base+3 {
		t.Fatalf("未声明 <cache/> 不应缓存: %d → %d", base, got)
	}
}

// ─── 自动时间戳 / 逻辑删除 ────────────────────────────────────────

const lifecycleMapperXML = `<mapper namespace="com.test.LifeMapper">
    <select id="findById">SELECT id, name, created_at, updated_at FROM life_users WHERE id = #{id}</select>
    <insert id="insertUser">INSERT INTO life_users (name, created_at, updated_at) VALUES (#{name}, #{created_at}, #{updated_at})</insert>
    <update id="touchName">UPDATE life_users SET name = #{name}, updated_at = #{updated_at} WHERE id = #{id}</update>
</mapper>`

type lifeUser struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func setupLifecycleFactory(t *testing.T, mutate func(*Config)) *SqlSessionFactory {
	t.Helper()
	baseDir := t.TempDir()
	mappersDir := filepath.Join(baseDir, "mappers")
	if err := mkdirAll(mappersDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(mappersDir, "LifeMapper.xml"), lifecycleMapperXML); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig().
		WithEnvironment(EnvironmentConfig{
			Driver: "sqlite", URL: "sqlite::memory:",
			PoolMaxConnections: 1, PoolMinConnections: 1,
		}).
		WithMapperPaths([]string{"mappers/**/*.xml"})
	if mutate != nil {
		mutate(cfg)
	}
	factory, err := NewSqlSessionFactory(context.Background(), cfg, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { factory.Close() })
	if _, err := factory.Environment().DB().Exec(
		"CREATE TABLE life_users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, created_at TEXT, updated_at TEXT, is_delete INTEGER DEFAULT 0)"); err != nil {
		t.Fatal(err)
	}
	return factory
}

func TestAutoTimestampsE2E(t *testing.T) {
	factory := setupLifecycleFactory(t, func(c *Config) {
		c.EnableAutoTimestamps()
	})
	s := factory.OpenSession()
	defer s.Close()
	ctx := context.Background()

	// insert：created_at + updated_at 均自动填充
	id, err := s.Insert(ctx, "com.test.LifeMapper", "insertUser", map[string]any{"name": "n1"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := SelectOne[lifeUser](ctx, s, "com.test.LifeMapper", "findById", map[string]any{"id": *id})
	if err != nil || u == nil {
		t.Fatalf("查询: %v %v", u, err)
	}
	if u.CreatedAt == "" || u.UpdatedAt == "" {
		t.Fatalf("自动时间戳未填充: %+v", u)
	}
	if !timestampLooksValid(u.CreatedAt) || !timestampLooksValid(u.UpdatedAt) {
		t.Fatalf("时间戳格式异常: %+v", u)
	}

	// update：updated_at 变化、created_at 不动
	if _, err = s.Update(ctx, "com.test.LifeMapper", "touchName",
		map[string]any{"id": *id, "name": "n2"}); err != nil {
		t.Fatal(err)
	}
	u2, err := SelectOne[lifeUser](ctx, s, "com.test.LifeMapper", "findById", map[string]any{"id": *id})
	if err != nil || u2 == nil {
		t.Fatal(err)
	}
	if u2.Name != "n2" {
		t.Fatalf("更新未生效: %+v", u2)
	}
	if u2.CreatedAt != u.CreatedAt {
		t.Fatalf("created_at 不应被 update 改写: %s → %s", u.CreatedAt, u2.CreatedAt)
	}

	// 显式传值优先
	id2, err := s.Insert(ctx, "com.test.LifeMapper", "insertUser",
		map[string]any{"name": "n3", "created_at": "2020-01-01 00:00:00"})
	if err != nil {
		t.Fatal(err)
	}
	u3, err := SelectOne[lifeUser](ctx, s, "com.test.LifeMapper", "findById", map[string]any{"id": *id2})
	if err != nil || u3 == nil {
		t.Fatal(err)
	}
	if u3.CreatedAt != "2020-01-01 00:00:00" {
		t.Fatalf("显式 created_at 应优先: %+v", u3)
	}
}

func timestampLooksValid(s string) bool {
	if len(s) != 19 || s[4] != '-' || s[10] != ' ' {
		return false
	}
	_, err := time.Parse("2006-01-02 15:04:05", s)
	return err == nil
}

func TestAutoTimestampsDisabledByDefault(t *testing.T) {
	factory := setupLifecycleFactory(t, nil)
	s := factory.OpenSession()
	defer s.Close()
	ctx := context.Background()
	// 默认关闭：不填充，显式传值必须完整给出（缺失键产生 MISSING 标记）
	id, err := s.Insert(ctx, "com.test.LifeMapper", "insertUser",
		map[string]any{"name": "x", "created_at": "", "updated_at": ""})
	if err != nil {
		t.Fatal(err)
	}
	u, err := SelectOne[lifeUser](ctx, s, "com.test.LifeMapper", "findById", map[string]any{"id": *id})
	if err != nil || u == nil {
		t.Fatal(err)
	}
	if u.CreatedAt != "" || u.UpdatedAt != "" {
		t.Fatalf("默认关闭不应填充: %+v", u)
	}
}

func TestLogicalDeletePresetE2E(t *testing.T) {
	factory := setupLifecycleFactory(t, func(c *Config) {
		c.EnableLogicalDelete()
	})
	if _, err := factory.Environment().DB().Exec(
		"INSERT INTO life_users (name, is_delete) VALUES ('alive', 0), ('dead', 1)"); err != nil {
		t.Fatal(err)
	}
	s := factory.OpenSession()
	defer s.Close()

	bound, err := s.QueryBuilder("life_users").WhereLike("name", "%al%").Build()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bound.SQL, "is_delete = ?") {
		t.Fatalf("应预置逻辑删除过滤: %s", bound.SQL)
	}
	rows, err := SelectBound[lifeUser](context.Background(), s, bound)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "alive" {
		t.Fatalf("已删除行不应出现: %+v", rows)
	}
	// 参数为标记正常值
	if bound.Parameters[0] != int64(0) {
		t.Fatalf("过滤值 = %v, want 0", bound.Parameters[0])
	}
}

// ─── 读写分离 ─────────────────────────────────────────────────────

func TestReadWriteSplitE2E(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	mastersPath := filepath.Join(baseDir, "mappers")
	if err := mkdirAll(mastersPath); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(mastersPath, "UserMapper.xml"), userMapperXML); err != nil {
		t.Fatal(err)
	}

	masterDB, replica1, replica2 := filepath.Join(baseDir, "master.db"),
		filepath.Join(baseDir, "r1.db"), filepath.Join(baseDir, "r2.db")
	// 主库：空表（写路径专用）；副本：各自预置标记行
	openRaw := func(path string) *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, score REAL, active INTEGER)"); err != nil {
			t.Fatal(err)
		}
		return db
	}
	mdb := openRaw(masterDB)
	defer mdb.Close()
	r1 := openRaw(replica1)
	defer r1.Close()
	if _, err := r1.Exec("INSERT INTO users (name, score, active) VALUES ('replica-1', 1, 1)"); err != nil {
		t.Fatal(err)
	}
	r2 := openRaw(replica2)
	defer r2.Close()
	if _, err := r2.Exec("INSERT INTO users (name, score, active) VALUES ('replica-2', 2, 1)"); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig().
		WithEnvironment(EnvironmentConfig{
			Driver: "sqlite", URL: "sqlite:" + masterDB,
			PoolMaxConnections: 2, PoolMinConnections: 1,
		}).
		WithReadEnvironments([]EnvironmentConfig{
			{Driver: "sqlite", URL: "sqlite:" + replica1, PoolMaxConnections: 1, PoolMinConnections: 1},
			{Driver: "sqlite", URL: "sqlite:" + replica2, PoolMaxConnections: 1, PoolMinConnections: 1},
		}).
		WithMapperPaths([]string{"mappers/**/*.xml"})
	factory, err := NewSqlSessionFactory(ctx, cfg, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { factory.Close() })
	if factory.ReadRouter().Len() != 2 {
		t.Fatalf("副本数 = %d, want 2", factory.ReadRouter().Len())
	}

	s := factory.OpenSession()
	defer s.Close()

	// 读：轮询命中两个副本各一次
	names := map[string]bool{}
	for i := 0; i < 2; i++ {
		users, err := SelectList[testUser](ctx, s, "com.test.UserMapper", "findAll", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 1 {
			t.Fatalf("副本应各有 1 行, got %d", len(users))
		}
		names[users[0].Name] = true
	}
	if !names["replica-1"] || !names["replica-2"] {
		t.Fatalf("轮询未覆盖全部副本: %v", names)
	}

	// 写：进主库（副本不可见）
	if _, err := s.Insert(ctx, "com.test.UserMapper", "insertUser",
		map[string]any{"name": "master-row", "score": 9.0, "active": 1}); err != nil {
		t.Fatal(err)
	}
	var masterCount int
	if err := mdb.QueryRow("SELECT COUNT(*) FROM users").Scan(&masterCount); err != nil {
		t.Fatal(err)
	}
	if masterCount != 1 {
		t.Fatalf("主库行数 = %d, want 1（写应走主库）", masterCount)
	}
	var r1Count int
	if err := r1.QueryRow("SELECT COUNT(*) FROM users").Scan(&r1Count); err != nil {
		t.Fatal(err)
	}
	if r1Count != 1 {
		t.Fatalf("副本被写入: %d", r1Count)
	}

	// 事务内读：强制主库
	if err := s.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	users, err := SelectList[testUser](ctx, s, "com.test.UserMapper", "findAll", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Name != "master-row" {
		t.Fatalf("事务读应走主库: %+v", users)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
}

// ─── 批量写 ───────────────────────────────────────────────────────

const batchMapperXML = `<mapper namespace="com.test.BatchMapper">
    <insert id="insertBatch">INSERT INTO batch_users (name, score) VALUES
        <foreach collection="list" item="u" separator=",">(#{u.name}, #{u.score})</foreach>
    </insert>
    <update id="updateScoreById">UPDATE batch_users SET score = #{score} WHERE id = #{id}</update>
    <select id="countAll">SELECT COUNT(*) AS cnt FROM batch_users</select>
</mapper>`

type batchUser struct {
	Name  string  `json:"name"`
	Score float64 `json:"score"`
	ID    int64   `json:"id"`
}

func setupBatchFactory(t *testing.T) *SqlSessionFactory {
	t.Helper()
	baseDir := t.TempDir()
	mappersDir := filepath.Join(baseDir, "mappers")
	if err := mkdirAll(mappersDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(mappersDir, "BatchMapper.xml"), batchMapperXML); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig().
		WithEnvironment(EnvironmentConfig{
			Driver: "sqlite", URL: "sqlite::memory:",
			PoolMaxConnections: 2, PoolMinConnections: 1,
		}).
		WithMapperPaths([]string{"mappers/**/*.xml"})
	factory, err := NewSqlSessionFactory(context.Background(), cfg, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { factory.Close() })
	if _, err := factory.Environment().DB().Exec(
		"CREATE TABLE batch_users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, score REAL CHECK (score >= 0))"); err != nil {
		t.Fatal(err)
	}
	return factory
}

func TestInsertBatchE2E(t *testing.T) {
	factory := setupBatchFactory(t)
	s := factory.OpenSession()
	defer s.Close()
	ctx := context.Background()

	// 1100 实体 × 2 占位符 = 2200 → 超预算 900，须分块（450×2=900 恰好适配）
	var entities []batchUser
	for i := 0; i < 1100; i++ {
		entities = append(entities, batchUser{Name: nameOf(i), Score: float64(i % 100)})
	}
	n, err := InsertBatch[batchUser](ctx, s, "com.test.BatchMapper", "insertBatch", entities)
	if err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	if n != 1100 {
		t.Fatalf("受影响行数 = %d, want 1100", n)
	}
	var count int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM batch_users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1100 {
		t.Fatalf("落库行数 = %d, want 1100", count)
	}

	// 空列表
	if n, err = InsertBatch[batchUser](ctx, s, "com.test.BatchMapper", "insertBatch", nil); err != nil || n != 0 {
		t.Fatalf("空列表: %d %v", n, err)
	}
}

func TestInsertBatchAtomicRollback(t *testing.T) {
	factory := setupBatchFactory(t)
	s := factory.OpenSession()
	defer s.Close()
	ctx := context.Background()

	// 第二块包含 NOT NULL 违例（name 为零值→NULL 不违例；用 CHECK 触发）
	// 先插 450+450（两块），随后一块里混入负分违例
	var entities []batchUser
	for i := 0; i < 450; i++ {
		entities = append(entities, batchUser{Name: nameOf(i), Score: 1})
	}
	bad := batchUser{Name: "bad", Score: -1}
	entities = append(entities, bad)
	if _, err := InsertBatch[batchUser](ctx, s, "com.test.BatchMapper", "insertBatch", entities); err == nil {
		t.Fatal("含违例的批量应失败")
	}
	var count int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM batch_users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("失败后应整体回滚, 残留 %d 行", count)
	}
}

func TestUpdateBatchE2E(t *testing.T) {
	factory := setupBatchFactory(t)
	s := factory.OpenSession()
	defer s.Close()
	ctx := context.Background()

	// 播种 3 行（取自增 id）
	seed := []batchUser{{Name: "a", Score: 1}, {Name: "b", Score: 2}, {Name: "c", Score: 3}}
	if _, err := InsertBatch[batchUser](ctx, s, "com.test.BatchMapper", "insertBatch", seed); err != nil {
		t.Fatal(err)
	}
	rows, err := s.DB().Query("SELECT id FROM batch_users ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	var entities []batchUser
	for i, id := range ids {
		entities = append(entities, batchUser{ID: id, Name: seed[i].Name, Score: float64(10 + i)})
	}
	n, err := UpdateBatch[batchUser](ctx, s, "com.test.BatchMapper", "updateScoreById", entities)
	if err != nil {
		t.Fatalf("UpdateBatch: %v", err)
	}
	if n != 3 {
		t.Fatalf("受影响行数 = %d, want 3", n)
	}
	var total float64
	if err := s.DB().QueryRow("SELECT SUM(score) FROM batch_users").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 33 { // 10+11+12
		t.Fatalf("SUM(score) = %v, want 33", total)
	}

	// 原子回滚：最后一个实体违反 CHECK
	entities = append(entities[:0],
		batchUser{ID: ids[0], Score: 100},
		batchUser{ID: ids[1], Score: -5},
	)
	if _, err = UpdateBatch[batchUser](ctx, s, "com.test.BatchMapper", "updateScoreById", entities); err == nil {
		t.Fatal("违例批量更新应失败")
	}
	var score0 float64
	if err := s.DB().QueryRow("SELECT score FROM batch_users WHERE id = ?", ids[0]).Scan(&score0); err != nil {
		t.Fatal(err)
	}
	if score0 != 10 {
		t.Fatalf("回滚后首行 score = %v, want 10", score0)
	}
}

func TestBatchInSessionTx(t *testing.T) {
	factory := setupBatchFactory(t)
	s := factory.OpenSession()
	defer s.Close()
	ctx := context.Background()

	// 会话事务内的批量并入事务：Rollback 后全部撤销
	if err := s.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	entities := []batchUser{{Name: "x", Score: 1}, {Name: "y", Score: 2}}
	if _, err := InsertBatch[batchUser](ctx, s, "com.test.BatchMapper", "insertBatch", entities); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM batch_users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("会话事务回滚后不应残留, got %d", count)
	}
}

// ─── TooManyRows 分组语义（v0.4）──────────────────────────────────

func TestSelectOneTooManyRowsGroupedSemantics(t *testing.T) {
	factory := setupFactory(t)
	s := factory.OpenSession()
	defer s.Close()
	ctx := context.Background()
	// 单父对象 + 2 子行：分组后 1 个父 → 不算 TooManyRows
	if _, err := s.Insert(ctx, "com.test.UserMapper", "insertUser",
		map[string]any{"name": "p", "score": 1.0, "active": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := SelectOne[testUser](ctx, s, "com.test.UserMapper", "findById", map[string]any{"id": 1}); err != nil {
		t.Fatalf("单行 SelectOne: %v", err)
	}
	// findAll 有多行 → TooManyRows（行数 = 3 播种后）
	seedUsers(t, s)
	_, err := SelectOne[testUser](ctx, s, "com.test.UserMapper", "findAll", nil)
	if err == nil {
		t.Fatal("多行 SelectOne 应报 TooManyRows")
	}
	if e, ok := err.(*Error); !ok || e.Kind != ErrTooManyRows {
		t.Fatalf("错误类别异常: %v", err)
	}
}
