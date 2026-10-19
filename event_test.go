package higomapper

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─── 事件总线测试 ─────────────────────────────────────────────────

type loginEvent struct{ User string }
type logoutEvent struct{}

func TestDispatchCallsMatchingListener(t *testing.T) {
	bus := NewEventBus()
	var counter atomic.Int64
	On(bus, func(e *loginEvent) {
		if e.User != "张三" {
			t.Errorf("user: %s", e.User)
		}
		counter.Add(1)
	})
	if ListenerCount[*loginEvent](bus) != 1 {
		t.Fatalf("listener count: %d", ListenerCount[*loginEvent](bus))
	}
	Dispatch(bus, &loginEvent{User: "张三"})
	if counter.Load() != 1 {
		t.Errorf("counter: %d", counter.Load())
	}
}

func TestEventTypeIsolation(t *testing.T) {
	// Login 的监听器不应被 Logout 事件触发
	bus := NewEventBus()
	var seen atomic.Int64
	On(bus, func(*loginEvent) { seen.Add(1) })
	Dispatch(bus, &logoutEvent{})
	Dispatch(bus, &loginEvent{User: "x"})
	if seen.Load() != 1 {
		t.Errorf("只应被 loginEvent 触发一次: %d", seen.Load())
	}
}

func TestMultipleListenersInRegistrationOrder(t *testing.T) {
	bus := NewEventBus()
	var mu sync.Mutex
	var order []int
	On(bus, func(*loginEvent) { mu.Lock(); order = append(order, 1); mu.Unlock() })
	On(bus, func(*loginEvent) { mu.Lock(); order = append(order, 2); mu.Unlock() })
	On(bus, func(*loginEvent) { mu.Lock(); order = append(order, 3); mu.Unlock() })

	Dispatch(bus, &loginEvent{User: "a"})
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Errorf("order: %v", order)
	}
}

func TestDispatchIfLazyAndSkipsWhenEmpty(t *testing.T) {
	bus := NewEventBus()
	var built atomic.Int64
	// 无监听器：build 不应被调用
	DispatchIf(bus, func() *loginEvent {
		built.Add(1)
		return &loginEvent{User: "x"}
	})
	if built.Load() != 0 {
		t.Fatal("无监听器时不应构造事件")
	}

	On(bus, func(*loginEvent) { built.Add(10) })
	DispatchIf(bus, func() *loginEvent {
		built.Add(1)
		return &loginEvent{User: "y"}
	})
	if built.Load() != 11 { // 构造一次 + 触发一次
		t.Errorf("built: %d", built.Load())
	}
}

// auditSubscriber 批量订阅两种事件
type auditSubscriber struct {
	logins  *atomic.Int64
	logouts *atomic.Int64
}

func (a *auditSubscriber) Subscribe(bus *EventBus) {
	On(bus, func(*loginEvent) { a.logins.Add(1) })
	On(bus, func(*logoutEvent) { a.logouts.Add(1) })
}

func TestSubscriberRegistersMultiple(t *testing.T) {
	bus := NewEventBus()
	logins, logouts := &atomic.Int64{}, &atomic.Int64{}
	bus.AddSubscriber(&auditSubscriber{logins: logins, logouts: logouts})

	if bus.TotalListeners() != 2 {
		t.Fatalf("total: %d", bus.TotalListeners())
	}
	Dispatch(bus, &loginEvent{User: "a"})
	Dispatch(bus, &loginEvent{User: "b"})
	Dispatch(bus, &logoutEvent{})
	if logins.Load() != 2 || logouts.Load() != 1 {
		t.Errorf("logins=%d logouts=%d", logins.Load(), logouts.Load())
	}
}

func TestReentrantSubscribeDuringDispatch(t *testing.T) {
	// 派发期间监听器再次订阅 —— 不应死锁（回调用的是切片快照）
	bus := NewEventBus()
	var added atomic.Int64
	On(bus, func(*loginEvent) {
		added.Add(1)
		// 回调中新增一个监听器：不影响当前派发（用的是旧快照）
		On(bus, func(*loginEvent) {})
	})
	Dispatch(bus, &loginEvent{User: "x"})
	if added.Load() != 1 {
		t.Fatalf("added: %d", added.Load())
	}
	if ListenerCount[*loginEvent](bus) != 2 {
		t.Fatalf("回调中应已新增一个监听器: %d", ListenerCount[*loginEvent](bus))
	}
	// 再次派发应触发全部 3 个监听器中的计数器
	var second atomic.Int64
	On(bus, func(*loginEvent) { second.Add(1) })
	Dispatch(bus, &loginEvent{User: "y"})
	if second.Load() != 1 {
		t.Errorf("新监听器应被后续派发触发: %d", second.Load())
	}
}

func TestZeroCostWhenEmpty(t *testing.T) {
	bus := NewEventBus()
	if bus.TotalListeners() != 0 || HasListeners[*loginEvent](bus) {
		t.Error("空总线")
	}
	// 无监听器派发不应 panic
	Dispatch(bus, &loginEvent{User: "x"})
	DispatchIf(bus, func() *loginEvent { return &loginEvent{User: "y"} })
}

// ─── 生命周期事件辅助 ─────────────────────────────────────────────

func TestClassifySQL(t *testing.T) {
	cases := []struct {
		sql  string
		want SqlKind
	}{
		{"SELECT * FROM t", SqlSelect},
		{"  insert into t", SqlInsert},
		{"REPLACE INTO t", SqlInsert},
		{"(SELECT 1) UNION (SELECT 2)", SqlSelect},
		{"UPDATE t SET", SqlUpdate},
		{"delete from t", SqlDelete},
		{"unknown stmt", SqlSelect},
	}
	for _, c := range cases {
		if got := ClassifySQL(c.sql); got != c.want {
			t.Errorf("ClassifySQL(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
}

func TestSqlKindString(t *testing.T) {
	if SqlSelect.String() != "SELECT" || SqlInsert.String() != "INSERT" ||
		SqlUpdate.String() != "UPDATE" || SqlDelete.String() != "DELETE" {
		t.Error("SqlKind.String")
	}
}

func TestOutcomeIsOK(t *testing.T) {
	if !(SqlOutcome{Kind: OutcomeFetched}).IsOK() {
		t.Error("Fetched 应为成功")
	}
	if !(SqlOutcome{Kind: OutcomeAffected, Rows: 3}).IsOK() {
		t.Error("Affected 应为成功")
	}
	if (SqlOutcome{Kind: OutcomeFailed, Err: "boom"}).IsOK() {
		t.Error("Failed 应为失败")
	}
}

// SQL 执行事件经 executor 自动派发（e2e 在 session_test 覆盖，这里验证计时语义）
func TestNeedTimingOffByDefault(t *testing.T) {
	e := NewSimpleExecutor()
	if e.needTiming() {
		t.Error("默认（日志关 + 无监听器）不应计时")
	}
	e = NewSimpleExecutor().WithSqlLog(SqlLogConfig{Enabled: true})
	if !e.needTiming() {
		t.Error("日志开启应计时")
	}
	e = NewSimpleExecutor().WithEventBus(NewEventBus())
	On(e.Bus, func(*AfterSqlEvent) {})
	if !e.needTiming() {
		t.Error("有 AfterSql 监听器应计时")
	}
}

// ─── 慢查询阈值 ───────────────────────────────────────────────────

func TestShouldLogThreshold(t *testing.T) {
	cfg := SqlLogConfig{Enabled: true}
	if !cfg.ShouldLog(time.Millisecond) || !cfg.ShouldLog(time.Second) {
		t.Error("阈值 0 记录全部")
	}
	cfg.SlowThresholdMs = 100
	if cfg.ShouldLog(50 * time.Millisecond) {
		t.Error("50ms < 100ms 不应记录")
	}
	if !cfg.ShouldLog(100 * time.Millisecond) {
		t.Error("100ms ≥ 100ms 应记录")
	}
	if !cfg.ShouldLog(500 * time.Millisecond) {
		t.Error("500ms 应记录")
	}
	cfg.Enabled = false
	cfg.SlowThresholdMs = 0
	if cfg.ShouldLog(10 * time.Second) {
		t.Error("开关关闭时一律不记录")
	}
}
