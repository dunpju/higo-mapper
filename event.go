package higomapper

// 事件系统（事件监听与订阅）。
//
// 灵感来自 ThinkPHP 的「模型事件 / 事件订阅」，按 Go 习惯实现：
// 类型化事件 + 泛型 On[E] + reflect.Type 路由的 EventBus + Subscriber 批量订阅。
//
// # 设计要点
//
//   - 类型化事件：每种事件是一个结构体；监听器按事件类型（reflect.Type）路由。
//   - 观察者语义：监听器收到事件值（只读语义），用于日志/审计/指标/缓存失效等副作用，
//     不支持在事件中修改数据或取消操作（保持分发简单、无返回值串联）。
//   - 同步回调：监听器在派发点内联调用；耗时工作请在监听器内部启 goroutine。
//   - 线程安全：监听器表用 RWMutex 保护；派发时先取监听器切片快照、
//     释放锁后再回调，从而监听器内部可安全地再次订阅/派发（避免重入死锁）。
//   - 零开销快路径：无任何监听器时 Dispatch / DispatchIf 经一次原子读即返回，
//     不取锁、不构造事件。
//
// # 示例
//
//	bus := NewEventBus()
//	On(bus, func(e *LoginEvent) { fmt.Println(e.User, "登录") })
//	bus.AddSubscriber(&AuditSubscriber{})
//	Dispatch(bus, &LoginEvent{User: "张三"})

import (
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EventBus 事件分发器（事件总线）：线程安全，按事件类型路由到监听器。
//
// 一个 *EventBus 可被多处共享；监听器在其生命周期内常驻。
type EventBus struct {
	mu sync.RWMutex
	// listeners 事件类型 → 该类型的监听器列表。
	// 切片一经存入即不再原地修改（订阅时整体重建），派发取快照后可安全回调。
	listeners map[reflect.Type][]func(any)
	// total 所有事件类型的监听器总数；用于无监听器时的一次原子读快路径
	total atomic.Int64
}

// NewEventBus 创建空的事件总线
func NewEventBus() *EventBus {
	return &EventBus{listeners: make(map[reflect.Type][]func(any))}
}

// eventTypeOf 取泛型参数 E 的 reflect.Type
func eventTypeOf[E any]() reflect.Type {
	return reflect.TypeOf((*E)(nil)).Elem()
}

// On 订阅：注册一个 E 类型的监听器函数。
//
// 多次订阅按注册顺序回调；监听器应快速返回，耗时工作请内部启 goroutine。
func On[E any](bus *EventBus, handler func(E)) {
	key := eventTypeOf[E]()
	adapter := func(ev any) {
		if e, ok := ev.(E); ok {
			handler(e)
		}
	}
	bus.mu.Lock()
	existing := bus.listeners[key]
	// 重建切片（订阅是罕见路径，重建成本可接受；换取派发的快照安全）
	next := make([]func(any), len(existing)+1)
	copy(next, existing)
	next[len(existing)] = adapter
	bus.listeners[key] = next
	bus.mu.Unlock()
	bus.total.Add(1)
}

// AddSubscriber 批量订阅：通过 Subscriber 注册其全部监听器。
func (b *EventBus) AddSubscriber(s Subscriber) {
	s.Subscribe(b)
}

// snapshotFor 取 E 监听器列表快照（读锁内取切片头，锁随读锁释放；
// 切片内容不可变，回调期间的新订阅不影响当前派发）。无监听器返回 nil。
func (b *EventBus) snapshotFor(t reflect.Type) []func(any) {
	if b.total.Load() == 0 {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.listeners[t]
}

// Dispatch 派发事件：按注册顺序同步调用 E 的所有监听器。
// 无监听器时零开销（一次原子读）。
func Dispatch[E any](bus *EventBus, event E) {
	listeners := bus.snapshotFor(eventTypeOf[E]())
	for _, l := range listeners {
		l(event)
	}
}

// DispatchIf 惰性派发：仅当存在 E 的监听器时才调用 build 构造事件并派发。
// 适用于构造事件代价较高（如克隆大参数）的场景。
func DispatchIf[E any](bus *EventBus, build func() E) {
	listeners := bus.snapshotFor(eventTypeOf[E]())
	if len(listeners) == 0 {
		return
	}
	event := build()
	for _, l := range listeners {
		l(event)
	}
}

// HasListeners 是否存在 E 的监听器
func HasListeners[E any](bus *EventBus) bool {
	if bus.total.Load() == 0 {
		return false
	}
	bus.mu.RLock()
	defer bus.mu.RUnlock()
	return len(bus.listeners[eventTypeOf[E]()]) > 0
}

// ListenerCount E 的监听器数量
func ListenerCount[E any](bus *EventBus) int {
	bus.mu.RLock()
	defer bus.mu.RUnlock()
	return len(bus.listeners[eventTypeOf[E]()])
}

// TotalListeners 所有事件类型的监听器总数
func (b *EventBus) TotalListeners() int {
	return int(b.total.Load())
}

// Subscriber 事件订阅器：批量注册多个事件监听器（对应 ThinkPHP 的「事件订阅」）。
//
// 实现者在 Subscribe 中将（自身的）多个处理逻辑绑定到不同事件。
type Subscriber interface {
	// Subscribe 将本订阅器关心的所有事件监听器注册到 bus
	Subscribe(bus *EventBus)
}

// ─── SQL 生命周期事件 ─────────────────────────────────────────────
//
// 在 SQL 执行前后由执行器自动派发，配合 EventBus 实现 SQL 层的观察者。
// 对应 ThinkPHP 模型事件的 on_before_* / on_after_* 思路（观察语义）。

// SqlKind SQL 操作种类
type SqlKind int

const (
	// SqlSelect 查询
	SqlSelect SqlKind = iota
	// SqlInsert 插入
	SqlInsert
	// SqlUpdate 更新
	SqlUpdate
	// SqlDelete 删除
	SqlDelete
)

// String 操作种类名称
func (k SqlKind) String() string {
	switch k {
	case SqlSelect:
		return "SELECT"
	case SqlInsert:
		return "INSERT"
	case SqlUpdate:
		return "UPDATE"
	case SqlDelete:
		return "DELETE"
	}
	return "UNKNOWN"
}

// ClassifySQL 按首关键字（尽力而为）判定 SQL 种类，用于事件分类
func ClassifySQL(sql string) SqlKind {
	// 跳过前导空白与括号（兼容 "(SELECT 1) UNION ..." 形式）
	s := strings.TrimLeft(sql, " \t\n\r(")
	upper := strings.ToUpper(s)
	switch {
	case strings.HasPrefix(upper, "SELECT"):
		return SqlSelect
	case strings.HasPrefix(upper, "INSERT"), strings.HasPrefix(upper, "REPLACE"):
		return SqlInsert
	case strings.HasPrefix(upper, "UPDATE"):
		return SqlUpdate
	case strings.HasPrefix(upper, "DELETE"):
		return SqlDelete
	}
	return SqlSelect
}

// BeforeSqlEvent SQL 执行前事件（观察用；不可修改 SQL 或参数）
type BeforeSqlEvent struct {
	// RawSQL 含 ? 占位符的原始 SQL
	RawSQL string
	// Params 绑定参数（按出现顺序）
	Params []any
	// Kind 操作种类
	Kind SqlKind
}

// AfterSqlEvent SQL 执行后事件（含耗时与结果摘要）
type AfterSqlEvent struct {
	// RawSQL 含 ? 占位符的原始 SQL
	RawSQL string
	// Params 绑定参数（按出现顺序）
	Params []any
	// Kind 操作种类
	Kind SqlKind
	// Elapsed 执行耗时
	Elapsed time.Duration
	// Outcome 结果摘要
	Outcome SqlOutcome
}

// OutcomeKind SQL 执行结果摘要类别
type OutcomeKind int

const (
	// OutcomeFetched SELECT 返回了行
	OutcomeFetched OutcomeKind = iota
	// OutcomeAffected INSERT/UPDATE/DELETE 受影响行数
	OutcomeAffected
	// OutcomeFailed 执行失败
	OutcomeFailed
)

// SqlOutcome SQL 执行结果摘要
type SqlOutcome struct {
	// Kind 摘要类别
	Kind OutcomeKind
	// Rows Fetched 行数或 Affected 受影响行数
	Rows int64
	// Err Failed 时的错误信息
	Err string
}

// IsOK 是否执行成功
func (o SqlOutcome) IsOK() bool { return o.Kind != OutcomeFailed }
