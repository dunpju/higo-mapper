package higomapper

// 分页查询（PageHelper 风格）。
//
// 在既有 mapper 语句之上提供「物理分页 + 总数统计」：
//
//   - 数据页：原 SQL 追加 LIMIT ? OFFSET ?（MySQL / PostgreSQL / SQLite 三方言通用，
//     postgres 的 ? → $n 转换由 pgx 驱动在 prepare 阶段完成）
//   - 总数页：剥离顶层 ORDER BY 后包一层 SELECT COUNT(*) FROM (…) AS __higo_count_t
//     （子查询包装同时正确处理 GROUP BY（对分组数计数）与 UNION（对合并结果计数））
//
// # 示例
//
//	page, err := higomapper.SelectPage[User](ctx, session, "app.UserDao", "findAll", nil,
//		higomapper.PageRequest{PageNo: 2, PageSize: 10})
//	// page.Total / page.TotalPages() / page.Records
//
// # 限制
//
//   - 原语句不得包含顶层 LIMIT / OFFSET（子查询内的 LIMIT 不受影响），否则报错——
//     追加式分页与既有分页子句会冲突。需要「先限制再分页」的语义请写在子查询里。
//   - 以 FOR UPDATE 结尾的锁定读不支持追加式分页（数据库会报语法错误）。
//   - 流式语义与分页不组合；ResultMap 嵌套分组可与分页正常配合（分组发生在页内）。

import (
	"strings"

	"github.com/dunpju/higo-mapper/core"
)

// PageRequest 分页请求（PageNo 从 1 开始）
type PageRequest struct {
	// PageNo 页码（1-based；0 非法）
	PageNo uint64
	// PageSize 每页行数（≥ 1）
	PageSize uint64
}

// NewPageRequest 构造分页请求（合法性与 ValidatedOffset 一致，执行期统一校验）
func NewPageRequest(pageNo, pageSize uint64) PageRequest {
	return PageRequest{PageNo: pageNo, PageSize: pageSize}
}

// ValidatedOffset 计算并校验 OFFSET。
//
// 校验规则：PageNo ≥ 1、PageSize ≥ 1、偏移量乘法不溢出且落在 int64 可绑定的
// 正数范围内。违反返回 ErrPagination。
func (p PageRequest) ValidatedOffset() (uint64, error) {
	if p.PageNo < 1 {
		return 0, newPaginationError("page_no 必须 ≥ 1, 实际为 %d", p.PageNo)
	}
	if p.PageSize < 1 {
		return 0, newPaginationError("page_size 必须 ≥ 1, 实际为 %d", p.PageSize)
	}
	if p.PageSize > uint64(maxInt64) {
		return 0, newPaginationError("分页参数超出 int64 可绑定范围")
	}
	var offset uint64
	if p.PageNo > 1 {
		factor := p.PageNo - 1
		if factor > (^uint64(0))/p.PageSize {
			return 0, newPaginationError("分页偏移量溢出: (%d) * (%d)", factor, p.PageSize)
		}
		offset = factor * p.PageSize
		if offset > uint64(maxInt64) {
			return 0, newPaginationError("分页参数超出 int64 可绑定范围")
		}
	}
	return offset, nil
}

const maxInt64 = int64(^uint64(0) >> 1)

// Page 分页结果
type Page[T any] struct {
	// Records 当前页记录
	Records []T
	// Total 满足条件的总行数（count 查询结果）
	Total uint64
	// PageNo 当前页码（1-based）
	PageNo uint64
	// PageSize 每页行数
	PageSize uint64
}

// NewPage 手工构建分页结果（一般经 SelectPage 产生）
func NewPage[T any](records []T, total, pageNo, pageSize uint64) Page[T] {
	return Page[T]{Records: records, Total: total, PageNo: pageNo, PageSize: pageSize}
}

// TotalPages 总页数（total = 0 时为 0；向上取整）
func (p Page[T]) TotalPages() uint64 {
	if p.PageSize == 0 {
		return 0
	}
	return (p.Total + p.PageSize - 1) / p.PageSize
}

// HasNext 是否还有下一页
func (p Page[T]) HasNext() bool { return p.PageNo < p.TotalPages() }

// HasPrev 是否存在上一页（PageNo > 1）
func (p Page[T]) HasPrev() bool { return p.PageNo > 1 }

// IsEmpty 当前页是否为空
func (p Page[T]) IsEmpty() bool { return len(p.Records) == 0 }

// EmptyPage 空页（total 为 0 时可跳过数据查询）
func EmptyPage[T any](page PageRequest) Page[T] {
	return Page[T]{Records: []T{}, Total: 0, PageNo: page.PageNo, PageSize: page.PageSize}
}

// MapPage 记录类型转换（保留分页元信息；Go 方法不能引入新类型参数，故为包级函数）
func MapPage[T any, U any](p Page[T], f func(T) U) Page[U] {
	out := make([]U, 0, len(p.Records))
	for _, r := range p.Records {
		out = append(out, f(r))
	}
	return Page[U]{Records: out, Total: p.Total, PageNo: p.PageNo, PageSize: p.PageSize}
}

// appendLimitOffset 向 BoundSql 追加 LIMIT ? OFFSET ?（参数按出现顺序追加到尾部）。
//
// 追加前会去除尾部空白与多余 ;。参数以 int64 绑定（三方言均接受）。
func appendLimitOffset(bound *core.BoundSql, limit, offset uint64) {
	t := strings.TrimRight(bound.SQL, " \t\r\n")
	t = strings.TrimSuffix(t, ";")
	t = strings.TrimRight(t, " \t\r\n")
	bound.SQL = t + " LIMIT ? OFFSET ?"
	bound.Parameters = append(bound.Parameters, int64(limit), int64(offset))
}

// buildCountSQL 由原 BoundSql 生成 count 查询：
// SELECT COUNT(*) FROM (剥离顶层 ORDER BY 的原 SQL) AS __higo_count_t
//
// 参数列表原样保留（? 顺序与原 SQL 一致）。子查询包装使 GROUP BY / UNION
// 场景下 count 语义正确（数分组数 / 合并行数），与 PageHelper 行为一致。
func buildCountSQL(bound *core.BoundSql) *core.BoundSql {
	trimmed := strings.TrimRight(strings.TrimRight(bound.SQL, " \t\r\n"), ";")
	inner := stripTopLevelOrderBy(trimmed)
	return &core.BoundSql{
		SQL:        "SELECT COUNT(*) FROM (" + inner + ") AS __higo_count_t",
		Parameters: bound.Parameters,
	}
}

// ensureNoTopLevelLimit 校验原 SQL 不含顶层 LIMIT / OFFSET（与追加式分页冲突）。
//
// 子查询（括号深度 > 0）或字符串字面量内的 LIMIT 不受影响。
func ensureNoTopLevelLimit(sql string) error {
	for _, kw := range []string{"limit", "offset"} {
		if len(findTopLevelKeywords(sql, kw)) > 0 {
			return newPaginationError(
				"分页语句不应包含顶层 %s 子句（子查询内的不受限）: %s", kw, truncateForError(sql))
		}
	}
	return nil
}

// stripTopLevelOrderBy 剥离顶层（括号深度 0）的 ORDER BY 子句；未找到则原样返回。
//
// 词法扫描规则：
//   - 跳过单引号字符串字面量（含 ” 转义）、双引号与反引号标识符
//   - 仅匹配括号深度 0 处、前后均为词边界的 ORDER（且紧跟 BY）关键字
//   - 多个顶层 ORDER BY（如 UNION 尾部）时剥除最靠右的一个
func stripTopLevelOrderBy(sql string) string {
	positions := findTopLevelKeywords(sql, "order")
	for i := len(positions) - 1; i >= 0; i-- {
		if isFollowedByBy(sql, positions[i]+5) {
			return strings.TrimRight(sql[:positions[i]], " \t\r\n")
		}
	}
	return sql
}

// isFollowedByBy 检查 pos 起（跳过空白）是否为词边界限的 BY（大小写不敏感）
func isFollowedByBy(sql string, pos int) bool {
	i := pos
	for i < len(sql) && isASCIISpace(sql[i]) {
		i++
	}
	return i+2 <= len(sql) &&
		(sql[i]|0x20) == 'b' && (sql[i+1]|0x20) == 'y' &&
		isWordBoundaryAt(sql, i+2)
}

// findTopLevelKeywords 收集 SQL 顶层（深度 0、字面量之外）指定关键字
// （小写传入）的全部出现位置。
func findTopLevelKeywords(sql, keyword string) []int {
	kw := keyword
	var positions []int
	depth := 0
	idx := 0
	n := len(sql)

	for idx < n {
		b := sql[idx]
		switch b {
		case '\'':
			// 单引号字符串：跳到闭合（'' 为转义，成对跳过）
			idx++
			for idx < n {
				if sql[idx] == '\'' {
					if idx+1 < n && sql[idx+1] == '\'' {
						idx += 2
						continue
					}
					idx++
					break
				}
				idx++
			}
			continue
		case '"', '`':
			// 双引号 / 反引号标识符：跳到闭合（无转义语义）
			quote := b
			idx++
			for idx < n && sql[idx] != quote {
				idx++
			}
			idx++
			continue
		case '(':
			depth++
			idx++
			continue
		case ')':
			if depth > 0 {
				depth--
			}
			idx++
			continue
		}
		if depth == 0 &&
			matchKeywordAt(sql, idx, kw) &&
			isWordBoundaryAt(sql, idx-1) &&
			isWordBoundaryAt(sql, idx+len(kw)) {
			positions = append(positions, idx)
			idx += len(kw)
			continue
		}
		idx++
	}
	return positions
}

// matchKeywordAt sql[idx:] 是否以关键字（大小写不敏感）开头
func matchKeywordAt(sql string, idx int, kw string) bool {
	if idx+len(kw) > len(sql) {
		return false
	}
	for i := 0; i < len(kw); i++ {
		if (sql[idx+i] | 0x20) != kw[i] {
			return false
		}
	}
	return true
}

// isWordBoundaryAt pos 位置（-1 表示串首；≥ len 表示串尾）是否为词边界：
// 非字母数字/下划线/$ 且非多字节 UTF-8 续字节（≥ 0x80）
func isWordBoundaryAt(sql string, pos int) bool {
	if pos < 0 || pos >= len(sql) {
		return true
	}
	c := sql[pos]
	if c >= 0x80 {
		return true // 多字节字符视为词边界（不参与关键字匹配）
	}
	return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$')
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// truncateForError 错误信息用截断（超长 SQL 只保留前 120 个字符，按字符边界切分）
func truncateForError(sql string) string {
	runes := []rune(sql)
	if len(runes) <= 120 {
		return sql
	}
	return string(runes[:120]) + "..."
}
