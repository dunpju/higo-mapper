package higomapper

import (
	"sync"
	"testing"
	"time"

	"github.com/dunpju/higo-mapper/core"
)

func TestQueryCacheBasics(t *testing.T) {
	c := NewQueryCache()
	c.ConfigureNamespace("app.UserDao", &core.CacheConfig{FlushIntervalMs: 60000, Size: 8})

	// 未命中
	if _, hit := c.Get("app.UserDao", "SELECT 1", []any{int64(1)}); hit {
		t.Fatal("空缓存不应命中")
	}

	// 写入后命中
	rows := []map[string]any{{"id": int64(1)}}
	c.Put("app.UserDao", "SELECT 1", []any{int64(1)}, rows)
	got, hit := c.Get("app.UserDao", "SELECT 1", []any{int64(1)})
	if !hit {
		t.Fatal("应命中")
	}
	if len(got) != 1 || got[0]["id"] != int64(1) {
		t.Fatalf("命中数据 = %#v", got)
	}

	// 参数不同 → 不命中（key 区分参数）
	if _, hit = c.Get("app.UserDao", "SELECT 1", []any{int64(2)}); hit {
		t.Fatal("不同参数不应命中")
	}
	// SQL 不同 → 不命中
	if _, hit = c.Get("app.UserDao", "SELECT 2", []any{int64(1)}); hit {
		t.Fatal("不同 SQL 不应命中")
	}
	// namespace 不同 → 不命中
	if _, hit = c.Get("other", "SELECT 1", []any{int64(1)}); hit {
		t.Fatal("不同 namespace 不应命中")
	}

	// namespace 整体失效
	c.InvalidateNamespace("app.UserDao")
	if _, hit = c.Get("app.UserDao", "SELECT 1", []any{int64(1)}); hit {
		t.Fatal("失效后不应命中")
	}
	if c.Len("app.UserDao") != 0 {
		t.Fatalf("失效后条目数 = %d", c.Len("app.UserDao"))
	}
}

func TestQueryCacheCanonicalParams(t *testing.T) {
	c := NewQueryCache()
	c.ConfigureNamespace("ns", nil)

	// 对象键顺序不同但等值的 map 参数 → 相同键
	p1 := map[string]any{"a": int64(1), "b": "x"}
	p2 := map[string]any{"b": "x", "a": int64(1)}
	c.Put("ns", "Q", []any{p1}, []map[string]any{{"ok": true}})
	if _, hit := c.Get("ns", "Q", []any{p2}); !hit {
		t.Fatal("等值不同序的 map 参数应命中（键排序规范化）")
	}

	// nil / 布尔 / 浮点参数均可区分
	c.Put("ns", "Q2", []any{nil, true, 1.5}, nil)
	if _, hit := c.Get("ns", "Q2", []any{nil, true, 1.5}); !hit {
		t.Fatal("混合参数应命中")
	}
	if _, hit := c.Get("ns", "Q2", []any{nil, false, 1.5}); hit {
		t.Fatal("布尔不同不应命中")
	}

	// 空参数 vs 无参数：同一键（均无 WHERE 值）
	c.Put("ns", "Q3", nil, nil)
	if _, hit := c.Get("ns", "Q3", []any{}); !hit {
		t.Fatal("nil 与空切片参数应等价")
	}
}

func TestQueryCacheTTLExpiry(t *testing.T) {
	c := NewQueryCache()
	c.ConfigureNamespace("ns", &core.CacheConfig{FlushIntervalMs: 30}) // 30ms TTL
	c.Put("ns", "Q", nil, []map[string]any{{"v": 1}})
	if _, hit := c.Get("ns", "Q", nil); !hit {
		t.Fatal("TTL 内应命中")
	}
	time.Sleep(50 * time.Millisecond)
	if _, hit := c.Get("ns", "Q", nil); hit {
		t.Fatal("TTL 过期后不应命中")
	}
	if c.Len("ns") != 0 {
		t.Fatal("过期条目应被清除")
	}

	// flushInterval = 0 → 永不过期
	c2 := NewQueryCache()
	c2.ConfigureNamespace("ns", &core.CacheConfig{})
	c2.Put("ns", "Q", nil, nil)
	time.Sleep(20 * time.Millisecond)
	if _, hit := c2.Get("ns", "Q", nil); !hit {
		t.Fatal("未配置 TTL 不应过期")
	}
}

func TestQueryCacheLRUEviction(t *testing.T) {
	c := NewQueryCache()
	c.ConfigureNamespace("ns", &core.CacheConfig{Size: 3})
	for i := 0; i < 3; i++ {
		c.Put("ns", "Q"+string(rune('a'+i)), nil, nil)
	}
	// 触碰最旧的 Qa，使其变为最近使用；再放入 Qd 淘汰 Qb
	c.Get("ns", "Qa", nil)
	c.Put("ns", "Qd", nil, nil)

	if _, hit := c.Get("ns", "Qb", nil); hit {
		t.Fatal("LRU 应淘汰 Qb")
	}
	for _, k := range []string{"Qa", "Qc", "Qd"} {
		if _, hit := c.Get("ns", k, nil); !hit {
			t.Fatalf("%s 不应被淘汰", k)
		}
	}
	if c.Len("ns") != 3 {
		t.Fatalf("条目数 = %d, want 3", c.Len("ns"))
	}
}

func TestQueryCacheUnconfiguredNamespaceIsNoop(t *testing.T) {
	c := NewQueryCache()
	c.Put("ghost", "Q", nil, nil) // 未配置 namespace：静默丢弃
	if _, hit := c.Get("ghost", "Q", nil); hit {
		t.Fatal("未配置 namespace 不应缓存")
	}
	if c.Len("ghost") != 0 {
		t.Fatal("未配置 namespace 条目数应为 0")
	}
}

func TestQueryCacheConcurrentAccess(t *testing.T) {
	c := NewQueryCache()
	c.ConfigureNamespace("ns", &core.CacheConfig{Size: 128})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				sql := "Q" + string(rune('a'+(i%26)))
				c.Put("ns", sql, []any{int64(i)}, nil)
				c.Get("ns", sql, []any{int64(i)})
				if i%50 == 0 {
					c.InvalidateNamespace("ns")
				}
			}
		}(g)
	}
	wg.Wait()
}
