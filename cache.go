package higomapper

// 二级查询缓存（namespace 粒度，L2）。
//
// 由 mapper XML 中的 `<cache flushInterval="60000" size="512"/>` 标签启用：
// 未声明 `<cache/>` 的 namespace 不参与缓存（行为零变化）。
//
// 语义（与 Rust hirust-mapper v0.3+ 对齐）：
//   - 键 = 最终 SQL + "|" + 规范化参数 JSON（对象键排序，encoding/json 原生有序）
//     —— 参数顺序不同的同值对象不会误命中
//   - 值 = ResultMap 分组之后、类型映射（T 化）之前的行数据（[]map[string]any），
//     同一份数据可服务不同目标类型
//   - 命中：不执行 SQL、不派发 SQL 事件、不打 SQL 日志
//   - 失效：该 namespace 任一写语句成功后整 namespace 失效（粗暴但正确）
//   - 淘汰：namespace 内 LRU，容量来自 size（默认 1024）；
//     TTL 来自 flushInterval 毫秒（0 = 不过期）
//   - 仅 select_one / select_list 走缓存；分页与流式查询不缓存

import (
	"container/list"
	"encoding/json"
	"sync"
	"time"

	"github.com/dunpju/higo-mapper/core"
)

// 默认容量（size 未配置时）
const defaultCacheSize = 1024

// cacheEntry LRU 节点：缓存行 + 过期时间
type cacheEntry struct {
	key       string
	rows      []map[string]any
	expiresAt time.Time // 零值 = 永不过期
}

// nsCache 单个 namespace 的缓存实例（互斥锁 + LRU 双向链表 + 索引）
type nsCache struct {
	mu      sync.Mutex
	maxSize int
	ttl     time.Duration
	entries *list.List               // front = 最近使用
	index   map[string]*list.Element // key → 节点
}

func newNsCache(maxSize int, ttl time.Duration) *nsCache {
	if maxSize <= 0 {
		maxSize = defaultCacheSize
	}
	return &nsCache{
		maxSize: maxSize,
		ttl:     ttl,
		entries: list.New(),
		index:   make(map[string]*list.Element, maxSize),
	}
}

// get 命中返回行数据并提升 LRU 位置；过期条目当场清除
func (n *nsCache) get(key string) ([]map[string]any, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	el, ok := n.index[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cacheEntry)
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		n.entries.Remove(el)
		delete(n.index, key)
		return nil, false
	}
	n.entries.MoveToFront(el)
	return e.rows, true
}

// put 写入并按容量淘汰最久未用条目
func (n *nsCache) put(key string, rows []map[string]any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if el, ok := n.index[key]; ok {
		el.Value = &cacheEntry{key: key, rows: rows, expiresAt: n.expiry()}
		n.entries.MoveToFront(el)
		return
	}
	n.index[key] = n.entries.PushFront(&cacheEntry{key: key, rows: rows, expiresAt: n.expiry()})
	for n.entries.Len() > n.maxSize {
		oldest := n.entries.Back()
		if oldest == nil {
			break
		}
		n.entries.Remove(oldest)
		delete(n.index, oldest.Value.(*cacheEntry).key)
	}
}

func (n *nsCache) expiry() time.Time {
	if n.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(n.ttl)
}

func (n *nsCache) invalidateAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.entries.Init()
	n.index = make(map[string]*list.Element, n.maxSize)
}

func (n *nsCache) len() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.entries.Len()
}

// QueryCache 工厂级共享查询缓存（按 namespace 隔离，各 namespace 独立 LRU/TTL）。
//
// 并发安全：namespace 表读多写少用 RWMutex；单个 namespace 内部互斥锁。
type QueryCache struct {
	mu         sync.RWMutex
	namespaces map[string]*nsCache
}

// NewQueryCache 创建空缓存
func NewQueryCache() *QueryCache {
	return &QueryCache{namespaces: make(map[string]*nsCache)}
}

// ConfigureNamespace 按 mapper 的 <cache/> 配置初始化 namespace 容量与 TTL。
//
// 已存在的 namespace 不重建（热重载后配置变更不生效直至进程重启，与 Rust 一致）。
func (c *QueryCache) ConfigureNamespace(ns string, cfg *core.CacheConfig) {
	size, ttl := 0, time.Duration(0)
	if cfg != nil {
		size = int(cfg.Size)
		if cfg.FlushIntervalMs > 0 {
			ttl = time.Duration(cfg.FlushIntervalMs) * time.Millisecond
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.namespaces[ns]; !ok {
		c.namespaces[ns] = newNsCache(size, ttl)
	}
}

func (c *QueryCache) namespace(ns string) *nsCache {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.namespaces[ns]
}

// Get 查询缓存；命中返回行数据（调用方不得修改返回切片）
func (c *QueryCache) Get(ns, sql string, params []any) ([]map[string]any, bool) {
	n := c.namespace(ns)
	if n == nil {
		return nil, false
	}
	return n.get(cacheKey(sql, params))
}

// Put 写入缓存
func (c *QueryCache) Put(ns, sql string, params []any, rows []map[string]any) {
	n := c.namespace(ns)
	if n == nil {
		return
	}
	n.put(cacheKey(sql, params), rows)
}

// InvalidateNamespace 清空整个 namespace（任一写语句成功后调用）
func (c *QueryCache) InvalidateNamespace(ns string) {
	if n := c.namespace(ns); n != nil {
		n.invalidateAll()
	}
}

// Len namespace 内缓存条目数（测试用）
func (c *QueryCache) Len(ns string) int {
	if n := c.namespace(ns); n != nil {
		return n.len()
	}
	return 0
}

// cacheKey 缓存键 = SQL + "|" + 规范化参数 JSON。
//
// encoding/json 对 map 键排序输出，因此等值但构造顺序不同的参数对象
// 生成相同键。
func cacheKey(sql string, params []any) string {
	if len(params) == 0 {
		return sql + "|"
	}
	b, err := json.Marshal(params)
	if err != nil {
		// 参数不可序列化：退化为 SQL-only 键（代价是参数区分度丢失，
		// 但规范化后的参数实际上总是可序列化的）
		return sql + "|<unserializable>"
	}
	return sql + "|" + string(b)
}
