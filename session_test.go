package higomapper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// ─── e2e：sqlite 内存库 ───────────────────────────────────────────

// testUser 测试用户结构（json tag 与查询列名对应）
type testUser struct {
	ID     int64   `json:"id"`
	Name   string  `json:"name"`
	Score  float64 `json:"score"`
	Active int64   `json:"active"`
}

const userMapperXML = `<mapper namespace="com.test.UserMapper">
    <select id="findById">SELECT id, name, score, active FROM users WHERE id = #{id}</select>
    <select id="findAll">SELECT id, name, score, active FROM users ORDER BY id</select>
    <select id="count">SELECT count(*) AS cnt FROM users</select>
    <select id="findByCond">
        SELECT id, name, score, active FROM users
        <where>
            <if test="name != null">AND name = #{name}</if>
            <if test="minScore != null">AND score &gt;= #{minScore}</if>
        </where>
        ORDER BY id
    </select>
    <insert id="insertUser">INSERT INTO users (name, score, active) VALUES (#{name}, #{score}, #{active})</insert>
    <update id="updateScore">
        UPDATE users
        <set>
            <if test="score != null">score = #{score},</if>
        </set>
        WHERE id = #{id}
    </update>
    <delete id="deleteById">DELETE FROM users WHERE id = #{id}</delete>
</mapper>`

// setupFactory 建立内存库 + 已加载 mapper 的工厂（单连接池保证表持久）
func setupFactory(t *testing.T) *SqlSessionFactory {
	t.Helper()
	baseDir := t.TempDir()
	mappersDir := filepath.Join(baseDir, "mappers")
	if err := os.MkdirAll(mappersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mappersDir, "UserMapper.xml"), []byte(userMapperXML), 0o644); err != nil {
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

	// 建表
	if _, err := factory.Environment().DB().Exec(
		"CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, score REAL, active INTEGER)"); err != nil {
		t.Fatal(err)
	}
	return factory
}

func seedUsers(t *testing.T, s *SqlSession) {
	t.Helper()
	for _, u := range []struct {
		name   string
		score  float64
		active int64
	}{
		{"张三", 95.5, 1},
		{"李四", 80.0, 0},
		{"王五", 60.0, 1},
	} {
		if _, err := s.Insert(context.Background(), "com.test.UserMapper", "insertUser", map[string]any{
			"name": u.name, "score": u.score, "active": u.active,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFactoryBuildAndSession(t *testing.T) {
	factory := setupFactory(t)
	if factory.Environment().Driver() != "sqlite" {
		t.Errorf("driver: %s", factory.Environment().Driver())
	}
	if factory.MapperCount() != 1 {
		t.Fatalf("mapper count: %d", factory.MapperCount())
	}
	if ns := factory.Namespaces(); len(ns) != 1 || ns[0] != "com.test.UserMapper" {
		t.Errorf("namespaces: %v", ns)
	}

	session := factory.OpenSession()
	if session.IsClosed() {
		t.Error("新 session 不应已关闭")
	}
	mapper, err := session.GetMapper("com.test.UserMapper")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mapper.Statements["findById"]; !ok {
		t.Error("缺少 findById")
	}
	if _, err := session.GetMapper("not.exist"); err == nil {
		t.Error("不存在的 namespace 应报错")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if !session.IsClosed() {
		t.Error("Close 后应为已关闭")
	}
}

func TestSessionCRUD(t *testing.T) {
	factory := setupFactory(t)
	session := factory.OpenSession()
	defer session.Close()

	// Insert（sqlite 支持 LastInsertId）
	id, err := session.Insert(context.Background(), "com.test.UserMapper", "insertUser", map[string]any{
		"name": "张三", "score": 95.5, "active": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == nil || *id != 1 {
		t.Fatalf("last insert id: %v", id)
	}

	// SelectList
	users, err := SelectList[testUser](context.Background(), session, "com.test.UserMapper", "findAll", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Name != "张三" || users[0].Score != 95.5 || users[0].Active != 1 {
		t.Fatalf("users: %+v", users)
	}

	// SelectOne
	user, err := SelectOne[testUser](context.Background(), session, "com.test.UserMapper", "findById", map[string]any{"id": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if user == nil || user.ID != 1 || user.Name != "张三" {
		t.Fatalf("user: %+v", user)
	}

	// SelectOne 无结果 → nil
	none, err := SelectOne[testUser](context.Background(), session, "com.test.UserMapper", "findById", map[string]any{"id": int64(999)})
	if err != nil || none != nil {
		t.Fatalf("none: %v, %v", none, err)
	}

	// 动态 SQL：仅 name 条件
	byName, err := SelectList[testUser](context.Background(), session, "com.test.UserMapper", "findByCond", map[string]any{"name": "张三"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byName) != 1 {
		t.Fatalf("byName: %+v", byName)
	}
	// 动态 SQL：minScore 条件（score >= 90）
	byScore, err := SelectList[testUser](context.Background(), session, "com.test.UserMapper", "findByCond", map[string]any{"minScore": int64(90)})
	if err != nil {
		t.Fatal(err)
	}
	if len(byScore) != 1 || byScore[0].Name != "张三" {
		t.Fatalf("byScore: %+v", byScore)
	}

	// Update
	affected, err := session.Update(context.Background(), "com.test.UserMapper", "updateScore", map[string]any{"id": int64(1), "score": 99.0})
	if err != nil {
		t.Fatal(err)
	}
	if affected != 1 {
		t.Fatalf("affected: %d", affected)
	}
	user, _ = SelectOne[testUser](context.Background(), session, "com.test.UserMapper", "findById", map[string]any{"id": int64(1)})
	if user.Score != 99.0 {
		t.Fatalf("update 后 score: %v", user.Score)
	}

	// Delete
	affected, err = session.Delete(context.Background(), "com.test.UserMapper", "deleteById", map[string]any{"id": int64(1)})
	if err != nil || affected != 1 {
		t.Fatalf("delete: %d, %v", affected, err)
	}
	users, _ = SelectList[testUser](context.Background(), session, "com.test.UserMapper", "findAll", nil)
	if len(users) != 0 {
		t.Fatalf("删除后应有 0 行: %+v", users)
	}
}

func TestSessionInsertStructParams(t *testing.T) {
	factory := setupFactory(t)
	session := factory.OpenSession()
	defer session.Close()

	// struct 参数：json tag 字段名作为参数键
	type newUser struct {
		Name   string  `json:"name"`
		Score  float64 `json:"score"`
		Active int64   `json:"active"`
	}
	id, err := session.Insert(context.Background(), "com.test.UserMapper", "insertUser", newUser{Name: "struct", Score: 50, Active: 1})
	if err != nil {
		t.Fatal(err)
	}
	if id == nil || *id != 1 {
		t.Fatalf("id: %v", id)
	}
	users, _ := SelectList[testUser](context.Background(), session, "com.test.UserMapper", "findAll", nil)
	if len(users) != 1 || users[0].Name != "struct" {
		t.Fatalf("users: %+v", users)
	}
}

func TestSessionTooManyRows(t *testing.T) {
	factory := setupFactory(t)
	session := factory.OpenSession()
	defer session.Close()
	seedUsers(t, session)

	_, err := SelectOne[testUser](context.Background(), session, "com.test.UserMapper", "findAll", nil)
	if err == nil {
		t.Fatal("多行应报错")
	}
	var runtimeErr *Error
	if !errors.As(err, &runtimeErr) || runtimeErr.Kind != ErrTooManyRows {
		t.Fatalf("应为 TooManyRows: %v", err)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("错误信息应含行数: %v", err)
	}
}

func TestSessionClosedOperations(t *testing.T) {
	factory := setupFactory(t)
	session := factory.OpenSession()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := SelectList[testUser](context.Background(), session, "com.test.UserMapper", "findAll", nil); err == nil {
		t.Error("关闭后查询应报错")
	}
	if _, err := session.Insert(context.Background(), "com.test.UserMapper", "insertUser", nil); err == nil {
		t.Error("关闭后插入应报错")
	}
	var e *Error
	_, err := SelectList[testUser](context.Background(), session, "com.test.UserMapper", "findAll", nil)
	if !errors.As(err, &e) || e.Kind != ErrSessionClosed {
		t.Errorf("应为 SessionClosed: %v", err)
	}
}

func TestSessionTransactions(t *testing.T) {
	factory := setupFactory(t)

	// 回滚路径
	session := factory.OpenSession()
	if err := session.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !session.InTransaction() {
		t.Fatal("应处于事务中")
	}
	if _, err := session.Insert(context.Background(), "com.test.UserMapper", "insertUser", map[string]any{"name": "tx", "score": 1, "active": 1}); err != nil {
		t.Fatal(err)
	}
	if err := session.Rollback(); err != nil {
		t.Fatal(err)
	}
	check := factory.OpenSession()
	defer check.Close()
	users, _ := SelectList[testUser](context.Background(), check, "com.test.UserMapper", "findAll", nil)
	if len(users) != 0 {
		t.Fatalf("回滚后应有 0 行: %+v", users)
	}

	// 提交路径
	session2 := factory.OpenSession()
	if err := session2.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := session2.Insert(context.Background(), "com.test.UserMapper", "insertUser", map[string]any{"name": "tx2", "score": 2, "active": 1}); err != nil {
		t.Fatal(err)
	}
	if err := session2.Commit(); err != nil {
		t.Fatal(err)
	}
	if session2.InTransaction() {
		t.Error("提交后不应处于事务中")
	}
	users, _ = SelectList[testUser](context.Background(), check, "com.test.UserMapper", "findAll", nil)
	if len(users) != 1 || users[0].Name != "tx2" {
		t.Fatalf("提交后应有 1 行: %+v", users)
	}

	// 重复 Begin 报错
	session3 := factory.OpenSession()
	defer session3.Close()
	if err := session3.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := session3.Begin(context.Background())
	if err == nil || !strings.Contains(err.Error(), "事务已开启") {
		t.Errorf("重复开启应报错: %v", err)
	}
	_ = session3.Rollback()

	// Close 回滚未提交事务
	session4 := factory.OpenSession()
	_ = session4.Begin(context.Background())
	_, _ = session4.Insert(context.Background(), "com.test.UserMapper", "insertUser", map[string]any{"name": "gone", "score": 3, "active": 1})
	_ = session4.Close()
	users, _ = SelectList[testUser](context.Background(), check, "com.test.UserMapper", "findAll", nil)
	if len(users) != 1 {
		t.Fatalf("Close 回滚后应仍为 1 行: %+v", users)
	}
}

func TestSessionSelectForEach(t *testing.T) {
	factory := setupFactory(t)
	session := factory.OpenSession()
	defer session.Close()
	seedUsers(t, session)

	var names []string
	err := SelectForEach(context.Background(), session, "com.test.UserMapper", "findAll", nil, func(u *testUser) error {
		names = append(names, u.Name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 || names[0] != "张三" || names[2] != "王五" {
		t.Fatalf("names: %v", names)
	}
}

func TestSessionSelectForEachEarlyStop(t *testing.T) {
	factory := setupFactory(t)
	session := factory.OpenSession()
	defer session.Close()
	seedUsers(t, session)

	stop := errors.New("stop")
	var count int
	err := SelectForEach(context.Background(), session, "com.test.UserMapper", "findAll", nil, func(u *testUser) error {
		count++
		if count == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("应透传回调错误: %v", err)
	}
	if count != 2 {
		t.Errorf("count: %d", count)
	}
}

func TestSessionSqlLifecycleEvents(t *testing.T) {
	factory := setupFactory(t)

	var before, after atomic.Int64
	var lastAfter atomic.Value // *AfterSqlEvent
	On(factory.EventBus(), func(e *BeforeSqlEvent) { before.Add(1) })
	On(factory.EventBus(), func(e *AfterSqlEvent) {
		after.Add(1)
		lastAfter.Store(e)
	})

	session := factory.OpenSession()
	defer session.Close()
	seedUsers(t, session) // 3 次 Insert

	users, err := SelectList[testUser](context.Background(), session, "com.test.UserMapper", "findAll", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Fatalf("users: %d", len(users))
	}

	// 3 insert + 1 select = 4 对事件
	if before.Load() != 4 || after.Load() != 4 {
		t.Fatalf("before=%d after=%d", before.Load(), after.Load())
	}
	ev := lastAfter.Load().(*AfterSqlEvent)
	if ev.Kind != SqlSelect {
		t.Errorf("最后事件应为 SELECT: %v", ev.Kind)
	}
	if ev.Outcome.Kind != OutcomeFetched || ev.Outcome.Rows != 3 {
		t.Errorf("outcome: %+v", ev.Outcome)
	}
	if ev.Elapsed < 0 {
		t.Errorf("elapsed: %v", ev.Elapsed)
	}
	if !strings.Contains(ev.RawSQL, "FROM users") {
		t.Errorf("raw sql: %s", ev.RawSQL)
	}
}

func TestSessionMapperProxy(t *testing.T) {
	factory := setupFactory(t)
	session := factory.OpenSession()
	defer session.Close()

	dao, err := MapperOf[testUser](session, "com.test.UserMapper")
	if err != nil {
		t.Fatal(err)
	}
	if dao.Namespace() != "com.test.UserMapper" {
		t.Errorf("ns: %s", dao.Namespace())
	}
	if _, err := session.Insert(context.Background(), "com.test.UserMapper", "insertUser", map[string]any{
		"name": "proxy", "score": 10, "active": 1,
	}); err != nil {
		t.Fatal(err)
	}
	users, err := dao.SelectList(context.Background(), "findAll", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Name != "proxy" {
		t.Fatalf("users: %+v", users)
	}
	user, err := dao.SelectOne(context.Background(), "findById", map[string]any{"id": int64(1)})
	if err != nil || user == nil || user.Name != "proxy" {
		t.Fatalf("user: %+v, %v", user, err)
	}
	n, err := dao.Update(context.Background(), "updateScore", map[string]any{"id": int64(1), "score": 20.0})
	if err != nil || n != 1 {
		t.Fatalf("update: %d %v", n, err)
	}
	n, err = dao.Delete(context.Background(), "deleteById", map[string]any{"id": int64(1)})
	if err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	// 不存在的 namespace
	if _, err := MapperOf[testUser](session, "nope"); err == nil {
		t.Error("应报 MapperNotFound")
	}
}

func TestSessionBuildBoundSql(t *testing.T) {
	factory := setupFactory(t)
	session := factory.OpenSession()
	defer session.Close()

	bound, err := session.BuildBoundSql("com.test.UserMapper", "findById", map[string]any{"id": int64(5)})
	if err != nil {
		t.Fatal(err)
	}
	if bound.SQL != "SELECT id, name, score, active FROM users WHERE id = ?" {
		t.Errorf("sql: %s", bound.SQL)
	}
	if bound.ParamCount() != 1 || bound.Parameters[0] != int64(5) {
		t.Errorf("params: %v", bound.Parameters)
	}
	// 语句不存在
	if _, err := session.BuildBoundSql("com.test.UserMapper", "nope", nil); err == nil {
		t.Error("应报错")
	}
}
