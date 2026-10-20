package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// squashWS 折叠行内空白（gofmt 会对齐 struct 字段，断言须容忍列宽）
func squashWS(s string) string {
	return regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ")
}

func TestDialectOfURL(t *testing.T) {
	cases := map[string]struct {
		want  string
		valid bool
	}{
		"mysql://u:p@h/db":   {"mysql", true},
		"mariadb://u:p@h/db": {"mysql", true},
		"sqlite:C:/x/y.db":   {"sqlite", true},
		"sqlite::memory:":    {"sqlite", true},
		"postgres://x":       {"", false},
	}
	for url, c := range cases {
		got, ok := dialectOfURL(url)
		if ok != c.valid || got != c.want {
			t.Errorf("dialectOfURL(%q) = (%q,%v), want (%q,%v)", url, got, ok, c.want, c.valid)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	if quoteIdent("users") != `"users"` {
		t.Fatal(quoteIdent("users"))
	}
	if quoteIdent(`we"ird`) != `"we""ird"` {
		t.Fatal(quoteIdent(`we"ird`))
	}
}

func TestPascalCase(t *testing.T) {
	if pascalCase("user_orders") != "UserOrders" {
		t.Fatal(pascalCase("user_orders"))
	}
	if pascalCase("users") != "Users" {
		t.Fatal(pascalCase("users"))
	}
	if pascalCase("a_b_c") != "ABC" {
		t.Fatal(pascalCase("a_b_c"))
	}
}

func TestGoFieldName(t *testing.T) {
	cases := map[string]string{
		"id":         "Id",
		"user_name":  "UserName",
		"userId":     "UserId",
		"Name":       "Name",
		"created_at": "CreatedAt",
	}
	for col, want := range cases {
		if got := goFieldName(col); got != want {
			t.Errorf("goFieldName(%q) = %q, want %q", col, got, want)
		}
	}
}

func TestMysqlType(t *testing.T) {
	if got := mysqlType("tinyint", "tinyint(1)").Base; got != "bool" {
		t.Errorf("tinyint(1) = %s", got)
	}
	if got := mysqlType("tinyint", "tinyint(4)").Base; got != "int" {
		t.Errorf("tinyint(4) = %s", got)
	}
	if got := mysqlType("int", "int(11)").Base; got != "int" {
		t.Errorf("int = %s", got)
	}
	if got := mysqlType("bigint", "bigint(20) unsigned").Base; got != "int64" {
		t.Errorf("bigint = %s", got)
	}
	if got := mysqlType("year", "year").Base; got != "int" {
		t.Errorf("year = %s", got)
	}
	if got := mysqlType("double", "double").Base; got != "float64" {
		t.Errorf("double = %s", got)
	}
	if s := mysqlType("varchar", "varchar(255)"); s.Base != "string" || s.GoTypeAttr != "" {
		t.Errorf("varchar = %+v", s)
	}
	if j := mysqlType("json", "json"); j.Base != "any" || j.GoTypeAttr != "json" {
		t.Errorf("json = %+v", j)
	}
	if d := mysqlType("decimal", "decimal(18,2)"); d.Base != "string" || d.GoTypeAttr != "decimal" || d.Note == "" {
		t.Errorf("decimal = %+v", d)
	}
	if dt := mysqlType("datetime", "datetime"); dt.Base != "string" || dt.Note == "" {
		t.Errorf("datetime = %+v", dt)
	}
	if bl := mysqlType("blob", "blob"); bl.Base != "string" || bl.Note == "" {
		t.Errorf("blob = %+v", bl)
	}
}

func TestSqliteType(t *testing.T) {
	if sqliteType("INTEGER").Base != "int64" {
		t.Fatal("INTEGER")
	}
	if sqliteType("INT").Base != "int64" {
		t.Fatal("INT")
	}
	if sqliteType("TEXT").Base != "string" {
		t.Fatal("TEXT")
	}
	if sqliteType("VARCHAR(50)").Base != "string" {
		t.Fatal("VARCHAR(50)")
	}
	if sqliteType("REAL").Base != "float64" || sqliteType("DOUBLE").Base != "float64" {
		t.Fatal("REAL/DOUBLE")
	}
	if sqliteType("BLOB").Base != "string" || sqliteType("BLOB").Note == "" {
		t.Fatal("BLOB")
	}
	if sqliteType("").Base != "string" {
		t.Fatal("空类型")
	}
	if n := sqliteType("DECIMAL(10,2)"); n.Base != "string" || n.GoTypeAttr != "decimal" {
		t.Fatalf("DECIMAL = %+v", n)
	}
}

// ─── 生成内容 ─────────────────────────────────────────────────────

func testMeta() *TableMeta {
	return &TableMeta{
		Name: "user_orders",
		Columns: []ColumnMeta{
			{Name: "id", DataType: "bigint", FullType: "bigint(20)", Nullable: false, IsPrimaryKey: true},
			{Name: "user_name", DataType: "varchar", FullType: "varchar(255)", Nullable: false},
			{Name: "price", DataType: "decimal", FullType: "decimal(18,2)", Nullable: true},
			{Name: "meta", DataType: "json", FullType: "json", Nullable: true},
		},
	}
}

func TestGenerateEntityMySQL(t *testing.T) {
	code := squashWS(generateEntity(testMeta(), "mysql", "models"))
	for _, want := range []string{
		"package models",
		"type UserOrders struct {",
		"Id int64 `json:\"id\"`",
		"UserName string `json:\"user_name\"`",
		"Price *string `json:\"price\"`",
		"Meta *any `json:\"meta\"`",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("entity 缺少 %q:\n%s", want, code)
		}
	}
	if strings.Contains(code, "goType") {
		t.Error("struct 侧不应生成 goType")
	}
	if !strings.Contains(code, "金额列") {
		t.Error("金额列应有注释")
	}
}

func TestGenerateEntityCaseDiffers(t *testing.T) {
	m := &TableMeta{
		Name: "t",
		Columns: []ColumnMeta{
			{Name: "userId", DataType: "int", FullType: "int(11)", Nullable: false, IsPrimaryKey: true},
		},
	}
	code := squashWS(generateEntity(m, "mysql", "models"))
	// 字段名 UserId，json tag 保留原列名（列名直映路径靠 tag 命中）
	if !strings.Contains(code, "UserId int `json:\"userId\"`") {
		t.Errorf("命名不一致时应依赖 json tag:\n%s", code)
	}
}

func TestGenerateXMLStructure(t *testing.T) {
	xml := generateXML(testMeta(), "mysql", "app.dao")
	for _, want := range []string{
		`<mapper namespace="app.dao.UserOrdersDao">`,
		`<id property="id" column="id"/>`,
		`<result property="price" column="price" goType="decimal"/>`,
		`<result property="meta" column="meta" goType="json"/>`,
		"WHERE id = #{id}",
		"INSERT INTO user_orders (id, user_name, price, meta) VALUES (#{id}, #{user_name}, #{price}, #{meta})",
		`<foreach collection="list" item="u" separator=",">(#{u.id}, #{u.user_name}, #{u.price}, #{u.meta})</foreach>`,
		"UPDATE user_orders SET user_name = #{user_name}, price = #{price}, meta = #{meta} WHERE id = #{id}",
		"DELETE FROM user_orders WHERE id = #{id}",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("xml 缺少 %q:\n%s", want, xml)
		}
	}
}

func TestGenerateSQLiteDialect(t *testing.T) {
	m := &TableMeta{
		Name: "t",
		Columns: []ColumnMeta{
			{Name: "id", DataType: "INTEGER", FullType: "INTEGER", Nullable: false, IsPrimaryKey: true},
		},
	}
	code := generateEntity(m, "sqlite", "models")
	if !strings.Contains(code, "Id int64") {
		t.Errorf("sqlite INTEGER → int64:\n%s", code)
	}
}

// ─── 内省（sqlite 内存库）────────────────────────────────────────

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:higomapper_cli_test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS user_orders (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        user_name TEXT NOT NULL,
        price DECIMAL(10,2),
        age INTEGER DEFAULT NULL
    )`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLiteIntrospectRoundTrip(t *testing.T) {
	db := openTestDB(t)

	tables, err := listTables(db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 1 || tables[0] != "user_orders" {
		t.Fatalf("tables = %v", tables)
	}

	meta, err := introspectTable(db, "sqlite", "user_orders")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "user_orders" || len(meta.Columns) != 4 {
		t.Fatalf("meta = %+v", meta)
	}
	if pk := meta.PrimaryKey(); pk == nil || pk.Name != "id" {
		t.Fatalf("主键 = %+v", meta.PrimaryKey())
	}
	if meta.Columns[1].Nullable {
		t.Error("user_name NOT NULL")
	}
	if !meta.Columns[2].Nullable {
		t.Error("price 应可空")
	}
	if meta.Columns[1].DataType != "TEXT" || meta.Columns[2].DataType != "DECIMAL(10,2)" {
		t.Fatalf("类型 = %s / %s", meta.Columns[1].DataType, meta.Columns[2].DataType)
	}

	// 不存在的表报错
	if _, err := introspectTable(db, "sqlite", "nope"); err == nil {
		t.Error("不存在的表应报错")
	}
}

func TestGenerateEndToEndSQLite(t *testing.T) {
	db := openTestDB(t)
	meta, err := introspectTable(db, "sqlite", "user_orders")
	if err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(t.TempDir(), "models")
	xmlDir := filepath.Join(t.TempDir(), "mappers")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(xmlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entity := generateEntity(meta, "sqlite", "models")
	xml := generateXML(meta, "sqlite", "app")
	if err := os.WriteFile(filepath.Join(outDir, "user_orders.go"), []byte(entity), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xmlDir, "UserOrdersDao.xml"), []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, err := os.ReadFile(filepath.Join(outDir, "user_orders.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type UserOrders struct {", "Price *string", "Age *int64", "UserName string"} {
		if !strings.Contains(squashWS(string(rs)), want) {
			t.Errorf("entity 缺少 %q:\n%s", want, rs)
		}
	}

	x, err := os.ReadFile(filepath.Join(xmlDir, "UserOrdersDao.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(x), `<mapper namespace="app.UserOrdersDao">`) {
		t.Errorf("namespace 异常:\n%s", x)
	}
	if !strings.Contains(string(x), `goType="decimal"`) {
		t.Errorf("decimal 列应带 goType:\n%s", x)
	}
}

func TestRunGenerateFileSQLite(t *testing.T) {
	// 文件库 → runGenerate 全流程（connectDB + 内省 + 落盘）
	base := t.TempDir()
	dbPath := filepath.Join(base, "app.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE goods (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT NOT NULL, price DECIMAL(10,2), stock INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(base, "models")
	xmlDir := filepath.Join(base, "mappers")
	if err := runGenerate([]string{
		"--url", "sqlite:" + filepath.ToSlash(dbPath),
		"--out-dir", outDir,
		"--xml-dir", xmlDir,
		"--namespace", "shop",
	}); err != nil {
		t.Fatalf("runGenerate: %v", err)
	}

	goFile := filepath.Join(outDir, "goods.go")
	xmlFile := filepath.Join(xmlDir, "GoodsDao.xml")
	for _, p := range []string{goFile, xmlFile} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("产物缺失 %s: %v", p, err)
		}
	}
	entity, err := os.ReadFile(goFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(squashWS(string(entity)), "Title string `json:\"title\"`") {
		t.Errorf("entity 异常:\n%s", entity)
	}
	xml, err := os.ReadFile(xmlFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xml), `<mapper namespace="shop.GoodsDao">`) {
		t.Errorf("xml namespace 异常:\n%s", xml)
	}

	// dry-run：不落盘
	if err := runGenerate([]string{
		"--url", "sqlite:" + filepath.ToSlash(dbPath),
		"--out-dir", filepath.Join(base, "dry"),
		"--xml-dir", filepath.Join(base, "drym"),
		"--dry-run",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "dry")); !os.IsNotExist(err) {
		t.Error("dry-run 不应写盘")
	}

	// 不支持的方法报错
	if err := runGenerate([]string{"--url", "postgres://x"}); err == nil {
		t.Error("postgres 应报不支持")
	}
}

func TestParseGenerateArgs(t *testing.T) {
	opt, err := parseGenerateArgs([]string{"--url", "sqlite:x.db", "--tables", "a,b", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if opt.url != "sqlite:x.db" || len(opt.tables) != 2 || !opt.dryRun {
		t.Fatalf("%+v", opt)
	}
	// 默认值
	opt, err = parseGenerateArgs([]string{"--url", "sqlite:x.db"})
	if err != nil {
		t.Fatal(err)
	}
	if opt.outDir != "./generated" || opt.xmlDir != "./generated/mappers" || opt.pkg != "models" || opt.namespace != "app" {
		t.Fatalf("默认值异常: %+v", opt)
	}
	// 缺 url
	if _, err = parseGenerateArgs(nil); err == nil {
		t.Fatal("缺 --url 应报错")
	}
	// 未知参数
	if _, err = parseGenerateArgs([]string{"--url", "u", "--bogus"}); err == nil {
		t.Fatal("未知参数应报错")
	}
}
