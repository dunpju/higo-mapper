package core

import (
	"strings"
	"testing"
)

// normalizeSQL 标准化 SQL 空白用于断言（折叠连续空白为单个空格）
func normalizeSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}

func buildSQL(t *testing.T, xml, stmtID string, params Params) string {
	t.Helper()
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	sql, err := mapper.BuildSQL(stmtID, params)
	if err != nil {
		t.Fatalf("生成 SQL 失败: %v", err)
	}
	return sql
}

func buildBound(t *testing.T, xml, stmtID string, params Params) *BoundSql {
	t.Helper()
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	bound, err := mapper.BuildBoundSQL(stmtID, params)
	if err != nil {
		t.Fatalf("生成 BoundSql 失败: %v", err)
	}
	return bound
}

// ─── 基础解析与内联生成 ───────────────────────────────────────────

func TestParseAndGenerateIf(t *testing.T) {
	xml := `<mapper namespace="com.example.UserMapper">
        <select id="findUserById" parameterType="Long" resultType="User">
            SELECT * FROM users WHERE 1=1
            <if test="id != null">AND id = #{id}</if>
            <if test="name != null and name != ''">AND name = #{name}</if>
        </select>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "findUserById", Params{
		"id":   int64(1),
		"name": "张三",
	}))
	if !strings.Contains(sql, "AND id = 1") {
		t.Errorf("SQL: %s", sql)
	}
	if !strings.Contains(sql, "AND name = '张三'") {
		t.Errorf("SQL: %s", sql)
	}
}

func TestForeach(t *testing.T) {
	xml := `<mapper namespace="com.example.UserMapper">
        <select id="test_foreach">
            SELECT * FROM tab1 where column1 in
            <foreach collection="list" index="index" item="item" open="(" separator="," close=")">
                #{item}
            </foreach>
        </select>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "test_foreach", Params{
		"list": []any{int64(1), int64(2), int64(3)},
	}))
	if sql != "SELECT * FROM tab1 where column1 in (1,2,3)" {
		t.Errorf("SQL: %s", sql)
	}
}

func TestInclude(t *testing.T) {
	xml := `<mapper namespace="com.example.UserMapper">
        <sql id="sql1">select a,b,c</sql>
        <select id="select0"><include refid="sql1"/> from tab1</select>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "select0", Params{}))
	if !strings.Contains(sql, "select a,b,c") {
		t.Errorf("SQL: %s", sql)
	}
	if !strings.Contains(sql, "from tab1") {
		t.Errorf("SQL: %s", sql)
	}
}

func TestIncludeOpenCloseForm(t *testing.T) {
	// <include refid="x"></include> 展开形式（非自闭合）
	xml := `<mapper namespace="com.example.UserMapper">
        <sql id="sql1">select a,b,c</sql>
        <select id="select1"><include refid="sql1"></include> from tab1</select>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "select1", Params{}))
	if !strings.Contains(sql, "select a,b,c") {
		t.Errorf("SQL: %s", sql)
	}
}

func TestInsert(t *testing.T) {
	xml := `<mapper namespace="com.example.UserMapper">
        <insert id="insert2">
            insert into tab2 (ID) values (#{id})
        </insert>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "insert2", Params{"id": int64(42)}))
	if !strings.Contains(sql, "insert into tab2 (ID) values (42)") {
		t.Errorf("SQL: %s", sql)
	}
}

func TestBatchInsert(t *testing.T) {
	xml := `<mapper namespace="com.example.UserMapper">
        <insert id="batchInsert">
            INSERT INTO book_attach_ocr_result(book_attach_ocr_task_id, book_attach_id) VALUES
            <foreach collection="list" separator="," item="entity">
                (#{entity.bookAttachOcrTaskId}, #{entity.bookAttachId})
            </foreach>
        </insert>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "batchInsert", Params{
		"list": []any{map[string]any{
			"bookAttachOcrTaskId": int64(1),
			"bookAttachId":        int64(2),
		}},
	}))
	if !strings.Contains(sql, "(1, 2)") {
		t.Errorf("SQL: %s", sql)
	}
}

func TestBatchUpdateCaseWhen(t *testing.T) {
	xml := `<mapper namespace="com.example.UserMapper">
        <update id="batchUpdateCaseWhen">
            UPDATE company
            <set>
                <trim prefix="` + "`company_name`= CASE company_id" + `" suffix="END,">
                    <foreach collection="companies" item="company">
                        WHEN #{company.companyId} THEN #{company.companyName}
                    </foreach>
                </trim>
            </set>
            <where>
                company_id in
                <foreach collection="companies" item="company" separator="," open="(" close=")">
                    #{company.companyId}
                </foreach>
            </where>
        </update>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "batchUpdateCaseWhen", Params{
		"companies": []any{map[string]any{
			"companyId":   int64(1),
			"companyName": "Test",
		}},
	}))
	if !strings.Contains(sql, "UPDATE company") {
		t.Errorf("SQL: %s", sql)
	}
	if !strings.Contains(sql, "SET") {
		t.Errorf("SQL: %s", sql)
	}
	if !strings.Contains(sql, "WHERE") {
		t.Errorf("SQL: %s", sql)
	}
}

func TestChooseWithNestedForeach(t *testing.T) {
	xml := `<mapper namespace="com.example.UserMapper">
        <select id="getCourseExamList">
            <foreach collection="newExamCourseList" item="newExamCourse" separator="UNION">
                (SELECT #{newExamCourse.courseIds} AS courseIds
                FROM exam A WHERE A.examId IN
                <foreach collection="examIds" item="id" open="(" separator="," close=")">
                    #{id}
                </foreach>
                <choose>
                    <when test="newExamCourse.selectContainCourse != null and newExamCourse.selectContainCourse != ''">
                        AND A.sysCourseId IN(${newExamCourse.selectContainCourse})
                    </when>
                    <otherwise>
                        AND A.sysCourseId IN(0)
                    </otherwise>
                </choose>
                LIMIT 10)
            </foreach>
        </select>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "getCourseExamList", Params{
		"newExamCourseList": []any{
			map[string]any{"courseIds": "1001", "selectContainCourse": "1,2,3"},
			map[string]any{"courseIds": "1002", "selectContainCourse": "4,5,6"},
		},
		"examIds": []any{int64(1), int64(2)},
	}))
	if !strings.Contains(sql, "UNION") {
		t.Errorf("应含 UNION（2 项）: %s", sql)
	}
	if !strings.Contains(sql, "IN (1,2)") {
		t.Errorf("应含 IN(1,2): %s", sql)
	}
	if !strings.Contains(sql, "IN(1,2,3)") {
		t.Errorf("应含 IN(1,2,3): %s", sql)
	}
}

func TestInsertDuplicateKeyUpdate(t *testing.T) {
	xml := `<mapper namespace="com.example.UserMapper">
        <insert id="insertDuplicateKeyUpdate">
            INSERT INTO book_school (book_id, school_id, is_delete) VALUES
            <foreach collection="entityList" item="entity" separator=",">
                (#{entity.bookId}, #{entity.schoolId}, 1)
            </foreach>
            ON DUPLICATE KEY UPDATE
            is_delete = values(is_delete)
        </insert>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "insertDuplicateKeyUpdate", Params{
		"entityList": []any{
			map[string]any{"bookId": int64(100), "schoolId": int64(200)},
			map[string]any{"bookId": int64(101), "schoolId": int64(201)},
		},
	}))
	if !strings.Contains(sql, "(100, 200, 1)") {
		t.Errorf("应含 (100,200,1,): %s", sql)
	}
	if !strings.Contains(sql, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("SQL: %s", sql)
	}
}

func TestMissingParamProducesMarker(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="t">SELECT #{missing_param}</select>
        </mapper>`

	sql := buildSQL(t, xml, "t", Params{})
	if !strings.Contains(sql, "/* MISSING:#missing_param */") {
		t.Errorf("SQL: %s", sql)
	}
}

func TestMissingFragmentReturnsError(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="t"><include refid="nonexistent"/></select>
        </mapper>`

	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mapper.BuildSQL("t", Params{})
	if err == nil {
		t.Fatal("缺片段应报错")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("错误信息应含片段 id: %v", err)
	}
}

func TestStatementNotFound(t *testing.T) {
	xml := `<mapper namespace="t"></mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mapper.BuildSQL("nonexistent", Params{})
	if err == nil {
		t.Fatal("语句不存在应报错")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("错误信息应含语句 id: %v", err)
	}
}

func TestOrCondition(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="t">
            SELECT * FROM t WHERE 1=1
            <if test="a == 1 or b == 'hello'">AND extra = 1</if>
        </select>
        </mapper>`

	if !strings.Contains(buildSQL(t, xml, "t", Params{"a": int64(1)}), "AND extra = 1") {
		t.Error("a==1 命中")
	}
	if !strings.Contains(buildSQL(t, xml, "t", Params{"a": int64(99), "b": "hello"}), "AND extra = 1") {
		t.Error("b=='hello' 命中")
	}
	if strings.Contains(buildSQL(t, xml, "t", Params{"a": int64(99), "b": "nope"}), "AND extra = 1") {
		t.Error("均不命中")
	}
}

func TestBindTag(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="t">
            <bind name="pattern" value="'%' + name + '%'"/>
            SELECT * FROM t WHERE name LIKE #{pattern}
        </select>
        </mapper>`

	sql := buildSQL(t, xml, "t", Params{"name": "test"})
	if !strings.Contains(sql, "LIKE") {
		t.Errorf("SQL: %s", sql)
	}
}

func TestErrorDisplay(t *testing.T) {
	err := newMissingParamError("foo", "findUser")
	msg := err.Error()
	if !strings.Contains(msg, "foo") || !strings.Contains(msg, "findUser") {
		t.Errorf("错误信息: %s", msg)
	}
}

// ─── BoundSql（参数化模式）测试 ────────────────────────────────────

func TestBoundSQLHashBecomesPlaceholder(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="findById">SELECT * FROM users WHERE id = #{id}</select>
        </mapper>`

	bound := buildBound(t, xml, "findById", Params{"id": int64(42)})
	if bound.SQL != "SELECT * FROM users WHERE id = ?" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 1 {
		t.Errorf("参数量: %d", bound.ParamCount())
	}
	if bound.Parameters[0] != int64(42) {
		t.Errorf("参数[0]: %v", bound.Parameters[0])
	}
}

func TestBoundSQLStringParamNotInlined(t *testing.T) {
	// 关键差异：字符串值不再内联加引号，而是作为参数
	xml := `<mapper namespace="t">
        <select id="findByName">SELECT * FROM users WHERE name = #{name}</select>
        </mapper>`

	bound := buildBound(t, xml, "findByName", Params{"name": "O'Brien"})
	if bound.SQL != "SELECT * FROM users WHERE name = ?" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.Parameters[0] != "O'Brien" {
		t.Errorf("参数[0]: %v", bound.Parameters[0])
	}
	if strings.Contains(bound.SQL, "O'Brien") {
		t.Errorf("SQL 不应内联值: %s", bound.SQL)
	}
}

func TestBoundSQLDollarStillInlined(t *testing.T) {
	// ${} 必须保持内联行为（如动态表名 / 排序字段）
	xml := `<mapper namespace="t">
        <select id="dynamic">SELECT * FROM ${table} ORDER BY ${col}</select>
        </mapper>`

	bound := buildBound(t, xml, "dynamic", Params{"table": "users", "col": "name"})
	if bound.SQL != "SELECT * FROM users ORDER BY name" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 0 {
		t.Errorf("${} 不产生参数，实际 %d", bound.ParamCount())
	}
}

func TestBoundSQLMixedMode(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="mixed">SELECT * FROM ${table} WHERE id = #{id} AND status = #{status}</select>
        </mapper>`

	bound := buildBound(t, xml, "mixed", Params{
		"table":  "orders",
		"id":     int64(7),
		"status": "active",
	})
	if bound.SQL != "SELECT * FROM orders WHERE id = ? AND status = ?" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 2 {
		t.Fatalf("参数量: %d", bound.ParamCount())
	}
	if bound.Parameters[0] != int64(7) || bound.Parameters[1] != "active" {
		t.Errorf("参数顺序错: %v", bound.Parameters)
	}
}

func TestBoundSQLParamOrderPreserved(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="insert">INSERT INTO t (a, b, c, d) VALUES (#{a}, #{b}, #{c}, #{d})</select>
        </mapper>`

	bound := buildBound(t, xml, "insert", Params{
		"a": int64(1), "b": "two", "c": true, "d": int64(4),
	})
	if bound.SQL != "INSERT INTO t (a, b, c, d) VALUES (?, ?, ?, ?)" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 4 {
		t.Fatalf("参数量: %d", bound.ParamCount())
	}
	if bound.Parameters[0] != int64(1) || bound.Parameters[1] != "two" ||
		bound.Parameters[2] != true || bound.Parameters[3] != int64(4) {
		t.Errorf("参数: %v", bound.Parameters)
	}
}

func TestBoundSQLForeachPlaceholderCount(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="inClause">
            SELECT * FROM tab1 WHERE column1 IN
            <foreach collection="list" item="item" open="(" separator="," close=")">
                #{item}
            </foreach>
        </select>
        </mapper>`

	bound := buildBound(t, xml, "inClause", Params{
		"list": []any{int64(1), int64(2), int64(3)},
	})
	if bound.SQL != "SELECT * FROM tab1 WHERE column1 IN (?,?,?)" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 3 {
		t.Errorf("参数量: %d", bound.ParamCount())
	}
	if bound.Parameters[0] != int64(1) || bound.Parameters[2] != int64(3) {
		t.Errorf("参数: %v", bound.Parameters)
	}
}

func TestBoundSQLIfConditional(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="find">
            SELECT * FROM users WHERE 1=1
            <if test="id != null">AND id = #{id}</if>
            <if test="name != null and name != ''">AND name = #{name}</if>
        </select>
        </mapper>`

	bound := buildBound(t, xml, "find", Params{"id": int64(1)})
	if bound.SQL != "SELECT * FROM users WHERE 1=1 AND id = ?" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 1 {
		t.Errorf("参数量: %d", bound.ParamCount())
	}

	bound = buildBound(t, xml, "find", Params{"id": int64(1), "name": "张三"})
	if bound.SQL != "SELECT * FROM users WHERE 1=1 AND id = ? AND name = ?" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 2 {
		t.Errorf("参数量: %d", bound.ParamCount())
	}
}

func TestBoundSQLWhereAndSetTags(t *testing.T) {
	xml := `<mapper namespace="t">
        <update id="update">
            UPDATE users
            <set>
                <if test="name != null">name = #{name},</if>
                <if test="age != null">age = #{age},</if>
            </set>
            <where>
                <if test="id != null">id = #{id}</if>
            </where>
        </update>
        </mapper>`

	bound := buildBound(t, xml, "update", Params{
		"name": "李四", "age": int64(30), "id": int64(5),
	})
	// SET 后逗号被剥离，WHERE 前缀正确
	if !strings.Contains(bound.SQL, "SET name = ?, age = ?") {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if !strings.Contains(bound.SQL, "WHERE id = ?") {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if strings.Contains(bound.SQL, ",,") {
		t.Errorf("不应有双逗号: %s", bound.SQL)
	}
	if bound.ParamCount() != 3 {
		t.Errorf("参数量: %d", bound.ParamCount())
	}
	if bound.Parameters[0] != "李四" || bound.Parameters[1] != int64(30) || bound.Parameters[2] != int64(5) {
		t.Errorf("参数顺序应为 name,age,id: %v", bound.Parameters)
	}
}

func TestBoundSQLChooseWithDollar(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="q">
            SELECT * FROM t WHERE 1=1
            <choose>
                <when test="filter != null">AND id IN (${filter})</when>
                <otherwise>AND id = #{defaultId}</otherwise>
            </choose>
        </select>
        </mapper>`

	// when 分支命中 → ${} 内联，无参数
	bound := buildBound(t, xml, "q", Params{"filter": "1,2,3"})
	if bound.SQL != "SELECT * FROM t WHERE 1=1 AND id IN (1,2,3)" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 0 {
		t.Errorf("参数量: %d", bound.ParamCount())
	}

	// otherwise 分支 → defaultId 参数化
	bound = buildBound(t, xml, "q", Params{"defaultId": int64(99)})
	if bound.SQL != "SELECT * FROM t WHERE 1=1 AND id = ?" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 1 || bound.Parameters[0] != int64(99) {
		t.Errorf("参数: %v", bound.Parameters)
	}

	// 缺失参数 → 标记，不产生 ?
	bound = buildBound(t, xml, "q", Params{})
	if !strings.Contains(bound.SQL, "MISSING") {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 0 {
		t.Errorf("参数量: %d", bound.ParamCount())
	}
}

func TestBoundSQLBindTag(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="like">
            <bind name="pattern" value="%${name}%"/>
            SELECT * FROM t WHERE name LIKE #{pattern}
        </select>
        </mapper>`

	bound := buildBound(t, xml, "like", Params{"name": "test"})
	if bound.SQL != "SELECT * FROM t WHERE name LIKE ?" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 1 {
		t.Fatalf("参数量: %d", bound.ParamCount())
	}
	// bind 解析后 pattern = "%test%"（${name} 内联为 test），作为参数
	s, ok := bound.Parameters[0].(string)
	if !ok || !strings.Contains(s, "test") {
		t.Errorf("pattern: %v", bound.Parameters[0])
	}
}

func TestBoundSQLIncludeFragment(t *testing.T) {
	xml := `<mapper namespace="t">
        <sql id="cols">a, b, c</sql>
        <select id="sel">SELECT <include refid="cols"/> FROM tab WHERE id = #{id}</select>
        </mapper>`

	bound := buildBound(t, xml, "sel", Params{"id": int64(9)})
	if bound.SQL != "SELECT a, b, c FROM tab WHERE id = ?" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 1 {
		t.Errorf("参数量: %d", bound.ParamCount())
	}
}

func TestBoundSQLMissingParamMarker(t *testing.T) {
	// 缺失参数：与内联模式一致的 MISSING 标记，且不产生多余的 ?
	xml := `<mapper namespace="t">
        <select id="m">SELECT #{missing}</select>
        </mapper>`

	bound := buildBound(t, xml, "m", Params{})
	if !strings.Contains(bound.SQL, "/* MISSING:#missing */") {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 0 {
		t.Errorf("缺失参数不进列表，实际 %d", bound.ParamCount())
	}
}

func TestBoundSQLStaticStatement(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="static">SELECT * FROM t WHERE a = #{a} AND b = #{b}</select>
        </mapper>`

	bound := buildBound(t, xml, "static", Params{"a": int64(1), "b": "x"})
	if bound.SQL != "SELECT * FROM t WHERE a = ? AND b = ?" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.ParamCount() != 2 {
		t.Errorf("参数量: %d", bound.ParamCount())
	}
}

func TestBoundSQLNoParams(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="all">SELECT * FROM t</select>
        </mapper>`

	bound := buildBound(t, xml, "all", Params{})
	if bound.SQL != "SELECT * FROM t" {
		t.Errorf("SQL: %s", bound.SQL)
	}
	if bound.HasParams() {
		t.Error("不应有参数")
	}
}

func TestBoundSQLConsistentWithInlineForStructure(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="q">
            SELECT * FROM users
            <where>
                <if test="id != null">AND id = #{id}</if>
                <if test="name != null">AND name = #{name}</if>
            </where>
        </select>
        </mapper>`

	params := Params{"id": int64(1), "name": "a"}
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	inline, err := mapper.BuildSQL("q", params)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := mapper.BuildBoundSQL("q", params)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inline, "WHERE id =") {
		t.Errorf("inline: %s", inline)
	}
	if !strings.Contains(bound.SQL, "WHERE id = ?") {
		t.Errorf("bound: %s", bound.SQL)
	}
	if !strings.Contains(bound.SQL, "AND name = ?") {
		t.Errorf("bound: %s", bound.SQL)
	}
}

// ─── ResultMap / selectKey 解析测试 ───────────────────────────────

func TestParseResultMapWithIDResult(t *testing.T) {
	xml := `<mapper namespace="t">
        <resultMap id="userMap" type="User">
            <id property="id" column="user_id"/>
            <result property="name" column="user_name" rustType="String"/>
        </resultMap>
        </mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	rm, ok := mapper.ResultMaps["userMap"]
	if !ok {
		t.Fatal("resultMap 未解析")
	}
	if rm.TypeName != "User" {
		t.Errorf("type: %s", rm.TypeName)
	}
	if len(rm.ResultColumns) != 2 {
		t.Fatalf("列数: %d", len(rm.ResultColumns))
	}
	if !rm.ResultColumns[0].IsID {
		t.Error("<id> 应标记为 id")
	}
	if rm.ResultColumns[0].Property != "id" || rm.ResultColumns[0].Column != "user_id" {
		t.Errorf("id 列: %+v", rm.ResultColumns[0])
	}
	if rm.ResultColumns[1].IsID {
		t.Error("<result> 不是 id")
	}
	if rm.ResultColumns[1].GoType != "String" {
		t.Errorf("rustType/goType: %s", rm.ResultColumns[1].GoType)
	}
	if len(rm.Associations) != 0 || len(rm.Collections) != 0 {
		t.Error("无嵌套")
	}
}

func TestParseResultMapSelfClosing(t *testing.T) {
	xml := `<mapper namespace="t">
        <resultMap id="m" type="User">
            <id property="id" column="id"/>
            <result property="name" column="name"/>
        </resultMap>
        </mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapper.ResultMaps["m"].ResultColumns) != 2 {
		t.Errorf("列数: %d", len(mapper.ResultMaps["m"].ResultColumns))
	}
}

func TestParseAssociationAndCollection(t *testing.T) {
	xml := `<mapper namespace="t">
        <resultMap id="userMap" type="User">
            <id property="id" column="id"/>
            <result property="name" column="name"/>
            <association property="department" javaType="Department">
                <id property="id" column="dept_id"/>
                <result property="name" column="dept_name"/>
            </association>
            <collection property="roles" ofType="Role">
                <id property="id" column="role_id"/>
                <result property="name" column="role_name"/>
            </collection>
        </resultMap>
        </mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	rm := mapper.ResultMaps["userMap"]

	if len(rm.Associations) != 1 {
		t.Fatalf("association 数: %d", len(rm.Associations))
	}
	assoc := rm.Associations[0]
	if assoc.Property != "department" || assoc.NestedType != "Department" {
		t.Errorf("assoc: %+v", assoc)
	}
	if len(assoc.ResultColumns) != 2 || !assoc.ResultColumns[0].IsID {
		t.Errorf("assoc 列: %+v", assoc.ResultColumns)
	}

	if len(rm.Collections) != 1 {
		t.Fatalf("collection 数: %d", len(rm.Collections))
	}
	coll := rm.Collections[0]
	if coll.Property != "roles" || coll.NestedType != "Role" {
		t.Errorf("coll: %+v", coll)
	}
	if len(coll.ResultColumns) != 2 {
		t.Errorf("coll 列: %+v", coll.ResultColumns)
	}
}

func TestParseNestedAssociationInsideCollection(t *testing.T) {
	xml := `<mapper namespace="t">
        <resultMap id="m" type="User">
            <id property="id" column="id"/>
            <collection property="orders" ofType="Order">
                <id property="id" column="order_id"/>
                <association property="addr" javaType="Addr">
                    <result property="city" column="city"/>
                </association>
            </collection>
        </resultMap>
        </mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	rm := mapper.ResultMaps["m"]
	if len(rm.Collections) != 1 {
		t.Fatal("collection 数错误")
	}
	coll := rm.Collections[0]
	if len(coll.Associations) != 1 {
		t.Fatalf("collection 内应含 association: %d", len(coll.Associations))
	}
	if coll.Associations[0].Property != "addr" {
		t.Errorf("addr: %s", coll.Associations[0].Property)
	}
	if coll.Associations[0].ResultColumns[0].Column != "city" {
		t.Errorf("city 列: %+v", coll.Associations[0].ResultColumns)
	}
}

func TestParseSelectKey(t *testing.T) {
	xml := `<mapper namespace="t">
        <insert id="insertWithKey">
            <selectKey keyProperty="id" resultType="i64" order="AFTER">
                SELECT LAST_INSERT_ID()
            </selectKey>
            INSERT INTO users (name) VALUES (#{name})
        </insert>
        </mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	stmt := mapper.Statements["insertWithKey"]
	if stmt.SelectKey == nil {
		t.Fatal("应解析 selectKey")
	}
	sk := stmt.SelectKey
	if sk.KeyProperty != "id" || sk.ResultType != "i64" || sk.Order != SelectKeyAfter {
		t.Errorf("selectKey: %+v", sk)
	}
	if !strings.Contains(sk.SQL, "LAST_INSERT_ID") {
		t.Errorf("selectKey SQL: %s", sk.SQL)
	}
	// selectKey 不应混入主 SQL
	if strings.Contains(stmt.SQL, "LAST_INSERT_ID") {
		t.Errorf("主 SQL 不应含 selectKey: %s", stmt.SQL)
	}
	if !strings.Contains(stmt.SQL, "INSERT INTO users") {
		t.Errorf("主 SQL: %s", stmt.SQL)
	}
}

func TestParseSelectKeyBeforeOrder(t *testing.T) {
	xml := `<mapper namespace="t">
        <insert id="i">
            <selectKey keyProperty="id" resultType="i64" order="BEFORE">
                SELECT seq.nextval
            </selectKey>
            INSERT INTO t VALUES (#{id})
        </insert>
        </mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	if got := mapper.Statements["i"].SelectKey.Order; got != SelectKeyBefore {
		t.Errorf("order: %v", got)
	}
}

// ─── .size() / .isEmpty() / 布尔字面量条件测试 ────────────────────

func TestConditionSizeGreaterThanZero(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="q">
            SELECT * FROM t
            <if test="list != null and list.size() > 0">
                WHERE id IN (<foreach collection="list" item="x" separator=",">#{x}</foreach>)
            </if>
        </select>
        </mapper>`

	// 非空列表 → 条件成立
	sql := buildSQL(t, xml, "q", Params{"list": []any{int64(1)}})
	if !strings.Contains(sql, "WHERE id IN") {
		t.Errorf("非空列表应进入条件: %s", sql)
	}
	// 空列表 → 条件不成立（size() == 0）
	sql = buildSQL(t, xml, "q", Params{"list": []any{}})
	if strings.Contains(sql, "WHERE") {
		t.Errorf("空列表应跳过条件: %s", sql)
	}
}

func TestConditionIsEmpty(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="q">
            SELECT * FROM t
            <if test="name.isEmpty() == false">WHERE active = 1</if>
        </select>
        </mapper>`

	// 非空字符串 → isEmpty() == false → true
	sql := buildSQL(t, xml, "q", Params{"name": "hi"})
	if !strings.Contains(sql, "WHERE active = 1") {
		t.Errorf("非空应进入: %s", sql)
	}
	// 空字符串 → 条件不成立
	sql = buildSQL(t, xml, "q", Params{"name": ""})
	if strings.Contains(sql, "WHERE") {
		t.Errorf("空字符串应跳过: %s", sql)
	}
}

func TestConditionBoolLiteralComparison(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="q">SELECT 1<if test="flag == true"> WHERE a=1</if></select>
        </mapper>`

	if !strings.Contains(buildSQL(t, xml, "q", Params{"flag": true}), "WHERE a=1") {
		t.Error("true 命中")
	}
	if strings.Contains(buildSQL(t, xml, "q", Params{"flag": false}), "WHERE") {
		t.Error("false 不命中")
	}
}

func TestConditionSizeWithString(t *testing.T) {
	// 字符串的 .size() → 字符数
	xml := `<mapper namespace="t">
        <select id="q">SELECT 1<if test="s.size() >= 3"> WHERE ok=1</if></select>
        </mapper>`

	if !strings.Contains(buildSQL(t, xml, "q", Params{"s": "abc"}), "WHERE ok=1") {
		t.Error("size 3 应命中")
	}
	if strings.Contains(buildSQL(t, xml, "q", Params{"s": "a"}), "WHERE") {
		t.Error("size 1 不命中")
	}
}

func TestConditionSizeMissingParamIsZero(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="q">SELECT 1<if test="missing.size() > 0"> WHERE x=1</if></select>
        </mapper>`

	// 缺失参数 .size() → 0，条件不成立
	if strings.Contains(buildSQL(t, xml, "q", Params{}), "WHERE") {
		t.Error("缺失参数 size()=0 应跳过")
	}
}

// ─── 实体解码（&gt; 等在 SQL 文本中）──────────────────────────────

func TestEntityDecodingInSQL(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="q">SELECT * FROM users WHERE age &gt;= #{min} AND age &lt; #{max}</select>
        </mapper>`

	bound := buildBound(t, xml, "q", Params{"min": int64(18), "max": int64(60)})
	want := "SELECT * FROM users WHERE age >= ? AND age < ?"
	if normalizeSQL(bound.SQL) != want {
		t.Errorf("SQL: %q", bound.SQL)
	}
	if bound.ParamCount() != 2 {
		t.Errorf("参数量: %d", bound.ParamCount())
	}
}

func TestEntityDecodingInCondition(t *testing.T) {
	// test 属性中的 &gt; 应被解码后再做条件匹配
	xml := `<mapper namespace="t">
        <select id="q">SELECT 1<if test="n &gt; 5"> WHERE big=1</if></select>
        </mapper>`

	if !strings.Contains(buildSQL(t, xml, "q", Params{"n": int64(10)}), "WHERE big=1") {
		t.Error("n>5 应命中")
	}
	if strings.Contains(buildSQL(t, xml, "q", Params{"n": int64(3)}), "WHERE") {
		t.Error("n=3 不命中")
	}
}

// ─── 杂项：foreach 空集合、嵌套 include ──────────────────────────

func TestForeachEmptyCollection(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="q">SELECT * FROM t WHERE id IN <foreach collection="list" item="x" open="(" separator="," close=")">#{x}</foreach></select>
        </mapper>`

	// 空集合 → foreach 产出空字符串（open/close 也不输出）
	sql := normalizeSQL(buildSQL(t, xml, "q", Params{"list": []any{}}))
	if strings.Contains(sql, "(") {
		t.Errorf("空集合不应输出 open/close: %s", sql)
	}
}

func TestNestedInclude(t *testing.T) {
	xml := `<mapper namespace="t">
        <sql id="base">select a,b,c</sql>
        <sql id="mid"><include refid="base"/>,d</sql>
        <select id="q"><include refid="mid"/> from tab1</select>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "q", Params{}))
	if !strings.Contains(sql, "select a,b,c ,d from tab1") && !strings.Contains(sql, "select a,b,c, d from tab1") && !strings.Contains(sql, "select a,b,c , d from tab1") {
		// 主要验证嵌套 include 被展开（内容齐全且顺序正确）
		if !strings.Contains(sql, "select a,b,c") || !strings.Contains(sql, "d") || !strings.Contains(sql, "from tab1") {
			t.Errorf("SQL: %s", sql)
		}
	}
}

func TestNamespaceIncludeReference(t *testing.T) {
	// namespace.id 形式的 refid（同 mapper 内查找）
	xml := `<mapper namespace="app">
        <sql id="cols">a, b</sql>
        <select id="q"><include refid="app.cols"/> FROM t</select>
        </mapper>`

	sql := normalizeSQL(buildSQL(t, xml, "q", Params{}))
	if !strings.Contains(sql, "a, b FROM t") {
		t.Errorf("SQL: %s", sql)
	}
}

func TestExtractParameters(t *testing.T) {
	xml := `<mapper namespace="t">
        <select id="q">SELECT #{a}, #{b,jdbcType=VARCHAR}, ${c}, #{d:numeric} FROM t</select>
        </mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	stmt := mapper.Statements["q"]
	got := map[string]bool{}
	for _, p := range stmt.Parameters {
		got[p] = true
	}
	for _, want := range []string{"a", "b", "c", "d"} {
		if !got[want] {
			t.Errorf("缺少参数 %s，实际 %v", want, stmt.Parameters)
		}
	}
}

// ─── <cache/> 二级缓存标签 ───────────────────────────────────────

func TestParseCacheSelfClosingWithAttrs(t *testing.T) {
	xml := `<mapper namespace="app.UserDao">
        <cache flushInterval="60000" size="512"/>
        <select id="findAll">SELECT id FROM users</select>
        </mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	if mapper.Cache == nil {
		t.Fatal("应解析出 Cache 配置")
	}
	if mapper.Cache.FlushIntervalMs != 60000 {
		t.Errorf("flushInterval: %d", mapper.Cache.FlushIntervalMs)
	}
	if mapper.Cache.Size != 512 {
		t.Errorf("size: %d", mapper.Cache.Size)
	}
}

func TestParseCacheBareTagDefaults(t *testing.T) {
	xml := `<mapper namespace="app.UserDao">
        <cache/>
        <select id="findAll">SELECT id FROM users</select>
        </mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	if mapper.Cache == nil {
		t.Fatal("裸 <cache/> 也应启用缓存（零值配置）")
	}
	if mapper.Cache.FlushIntervalMs != 0 || mapper.Cache.Size != 0 {
		t.Errorf("零值配置: %+v", mapper.Cache)
	}
}

func TestParseCachePairedTagAndMissing(t *testing.T) {
	// 成对标签形式
	xml := `<mapper namespace="a"><cache size="8"></cache></mapper>`
	mapper, err := ParseMapper(xml)
	if err != nil {
		t.Fatal(err)
	}
	if mapper.Cache == nil || mapper.Cache.Size != 8 {
		t.Errorf("成对标签: %+v", mapper.Cache)
	}

	// 未声明 <cache> → nil
	xml2 := `<mapper namespace="b"><select id="q">SELECT 1</select></mapper>`
	mapper2, err := ParseMapper(xml2)
	if err != nil {
		t.Fatal(err)
	}
	if mapper2.Cache != nil {
		t.Errorf("未声明缓存应为 nil: %+v", mapper2.Cache)
	}
}

func TestParseCacheInvalidAttrs(t *testing.T) {
	for _, attr := range []string{`flushInterval="abc"`, `size="-3"`, `size="1.5"`} {
		xml := `<mapper namespace="a"><cache ` + attr + `/></mapper>`
		if _, err := ParseMapper(xml); err == nil {
			t.Errorf("非法属性 %s 应报错", attr)
		}
	}
}
