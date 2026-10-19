package higomapper

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dunpju/higo-mapper/core"
)

// ─── ResultMap 嵌套映射（association / collection）e2e ──────────────

type nestedDept struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type nestedUserWithDept struct {
	ID     int64       `json:"id"`
	Name   string      `json:"name"`
	DeptID int64       `json:"deptId"`
	Dept   *nestedDept `json:"department"`
}

type nestedOrder struct {
	ID     int64 `json:"id"`
	UserID int64 `json:"userId"`
	Amount int64 `json:"amount"`
}

type nestedUserWithOrders struct {
	ID     int64         `json:"id"`
	Name   string        `json:"name"`
	Orders []nestedOrder `json:"orders"`
}

const nestedMapperXML = `<mapper namespace="com.test.NestedMapper">
    <resultMap id="userWithDeptMap" type="UserWithDept">
        <id property="id" column="id"/>
        <result property="name" column="name"/>
        <result property="deptId" column="dept_id"/>
        <association property="department" javaType="Dept">
            <id property="id" column="dept_id"/>
            <result property="name" column="dept_name"/>
        </association>
    </resultMap>
    <resultMap id="userWithOrdersMap" type="UserWithOrders">
        <id property="id" column="id"/>
        <result property="name" column="name"/>
        <collection property="orders" ofType="Order">
            <id property="id" column="order_id"/>
            <result property="userId" column="user_id"/>
            <result property="amount" column="amount"/>
        </collection>
    </resultMap>
    <select id="selectUserWithDept" resultMap="userWithDeptMap">
        SELECT u.id, u.name, u.dept_id, d.name AS dept_name
        FROM users u LEFT JOIN departments d ON d.id = u.dept_id
        ORDER BY u.id
    </select>
    <select id="selectUserWithDeptOne" resultMap="userWithDeptMap">
        SELECT u.id, u.name, u.dept_id, d.name AS dept_name
        FROM users u LEFT JOIN departments d ON d.id = u.dept_id
        WHERE u.id = #{id}
    </select>
    <select id="selectUserWithOrders" resultMap="userWithOrdersMap">
        SELECT u.id, u.name, o.id AS order_id, o.user_id, o.amount
        FROM users u LEFT JOIN orders o ON o.user_id = u.id
        ORDER BY u.id, o.id
    </select>
</mapper>`

func setupNestedFactory(t *testing.T) *SqlSessionFactory {
	t.Helper()
	baseDir := t.TempDir()
	mappersDir := filepath.Join(baseDir, "mappers")
	if err := os.MkdirAll(mappersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mappersDir, "NestedMapper.xml"), []byte(nestedMapperXML), 0o644); err != nil {
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

	db := factory.Environment().DB()
	stmts := []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, dept_id INTEGER)",
		"CREATE TABLE departments (id INTEGER PRIMARY KEY, name TEXT)",
		"CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER, amount INTEGER)",
		"INSERT INTO departments VALUES (10, '工程部'), (20, '市场部')",
		"INSERT INTO users VALUES (1, '张三', 10), (2, '李四', 20), (3, '王五', NULL)",
		"INSERT INTO orders VALUES (101, 1, 500), (102, 1, 300), (103, 2, 800)",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return factory
}

func TestNestedAssociation(t *testing.T) {
	factory := setupNestedFactory(t)
	session := factory.OpenSession()
	defer session.Close()

	users, err := SelectList[nestedUserWithDept](context.Background(), session, "com.test.NestedMapper", "selectUserWithDept", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Fatalf("users: %d", len(users))
	}
	// 张三 → 工程部
	if users[0].Dept == nil || users[0].Dept.ID != 10 || users[0].Dept.Name != "工程部" {
		t.Errorf("张三 dept: %+v", users[0].Dept)
	}
	// 李四 → 市场部
	if users[1].Dept == nil || users[1].Dept.Name != "市场部" {
		t.Errorf("李四 dept: %+v", users[1].Dept)
	}
	// 王五 dept_id NULL → association 全空 → nil
	if users[2].Dept != nil {
		t.Errorf("王五 dept 应为 nil: %+v", users[2].Dept)
	}

	// SelectOne 路径（单行 resultMap）
	one, err := SelectOne[nestedUserWithDept](context.Background(), session, "com.test.NestedMapper", "selectUserWithDeptOne", map[string]any{"id": int64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if one == nil || one.Name != "李四" || one.Dept == nil || one.Dept.Name != "市场部" {
		t.Fatalf("one: %+v", one)
	}
}

func TestNestedCollectionGrouping(t *testing.T) {
	factory := setupNestedFactory(t)
	session := factory.OpenSession()
	defer session.Close()

	users, err := SelectList[nestedUserWithOrders](context.Background(), session, "com.test.NestedMapper", "selectUserWithOrders", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Fatalf("应按用户 id 分组为 3 个父对象: %d", len(users))
	}
	// 张三 2 单
	if len(users[0].Orders) != 2 || users[0].Orders[0].ID != 101 || users[0].Orders[1].Amount != 300 {
		t.Errorf("张三 orders: %+v", users[0].Orders)
	}
	// 李四 1 单
	if len(users[1].Orders) != 1 || users[1].Orders[0].ID != 103 {
		t.Errorf("李四 orders: %+v", users[1].Orders)
	}
	// 王五无单 → 空数组（非 nil 也可能是 nil，由 JSON 决定；至少可安全迭代）
	for _, o := range users[2].Orders {
		_ = o
	}
	if len(users[2].Orders) != 0 {
		t.Errorf("王五 orders 应为空: %+v", users[2].Orders)
	}
}

func TestNestedCollectionOrderIDs(t *testing.T) {
	factory := setupNestedFactory(t)
	session := factory.OpenSession()
	defer session.Close()

	users, err := SelectList[nestedUserWithOrders](context.Background(), session, "com.test.NestedMapper", "selectUserWithOrders", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 子项 id 列（order_id）正确映射
	if users[0].Orders[0].UserID != 1 {
		t.Errorf("userId 映射: %+v", users[0].Orders[0])
	}
}

// ─── 纯函数级：分组键与嵌套构建 ───────────────────────────────────

func TestBuildNestedObjectAllNull(t *testing.T) {
	mapping := resultMapFixture("orders")
	row := map[string]any{"order_id": nil, "amount": nil}
	if got := buildNestedObject(row, mapping); got != nil {
		t.Errorf("全空应返回 nil: %v", got)
	}
}

func TestBuildNestedObjectSomeValue(t *testing.T) {
	mapping := resultMapFixture("orders")
	row := map[string]any{"order_id": int64(9), "amount": nil}
	got := buildNestedObject(row, mapping)
	if got == nil {
		t.Fatal("有值应返回对象")
	}
	if got["id"] != int64(9) || got["amount"] != nil {
		t.Errorf("obj: %v", got)
	}
}

func TestIdKeyForms(t *testing.T) {
	if idKey(nil) != "null" {
		t.Error("nil → null")
	}
	if idKey("1") != `"1"` { // 字符串加引号，区别于数字
		t.Errorf("string: %s", idKey("1"))
	}
	if idKey(int64(1)) != "1" {
		t.Errorf("int: %s", idKey(int64(1)))
	}
	if idKey(true) != "true" {
		t.Errorf("bool: %s", idKey(true))
	}
}

// resultMapFixture 构造嵌套映射（属性 id/amount，id 列 order_id）
func resultMapFixture(property string) *core.NestedMapping {
	return &core.NestedMapping{
		Property: property,
		ResultColumns: []core.ResultColumn{
			{Property: "id", Column: "order_id", IsID: true},
			{Property: "amount", Column: "amount"},
		},
	}
}
