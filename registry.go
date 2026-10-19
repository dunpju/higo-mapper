package higomapper

// Mapper 注册表与类型别名注册表。
//
// 线程安全地持有所有已解析的 *core.Mapper 实例，
// 支持热重载时的并发读写（RWMutex：查询路径并发读，热重载独占写）。

import (
	"os"
	"sort"
	"sync"

	"github.com/dunpju/higo-mapper/core"
)

// MapperRegistry 线程安全的 Mapper 注册表
type MapperRegistry struct {
	mu      sync.RWMutex
	mappers map[string]*core.Mapper
}

// NewMapperRegistry 创建空注册表
func NewMapperRegistry() *MapperRegistry {
	return &MapperRegistry{mappers: map[string]*core.Mapper{}}
}

// RegisterFromXML 从 XML 内容解析并注册一个 Mapper，返回其 namespace
func (r *MapperRegistry) RegisterFromXML(xmlContent string) (string, error) {
	mapper, err := core.ParseMapper(xmlContent)
	if err != nil {
		return "", err
	}
	ns := mapper.Namespace
	r.InsertMapper(mapper)
	return ns, nil
}

// RegisterFromFile 从文件路径加载并注册一个 Mapper，返回其 namespace
func (r *MapperRegistry) RegisterFromFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", newConfigErrorWrap(err, "无法读取 mapper 文件 %s", path)
	}
	ns, err := r.RegisterFromXML(string(content))
	if err != nil {
		return "", newConfigErrorWrap(err, "加载 mapper 文件 %s 失败", path)
	}
	return ns, nil
}

// LoadFromConfig 按配置批量加载所有 mapper 文件。
//
// 返回成功注册的 namespace 列表；任一文件解析失败即返回错误（含文件路径）。
func (r *MapperRegistry) LoadFromConfig(cfg *Config, baseDir string) ([]string, error) {
	files, err := cfg.DiscoverMapperFiles(baseDir)
	if err != nil {
		return nil, err
	}
	namespaces := make([]string, 0, len(files))
	for _, file := range files {
		ns, err := r.RegisterFromFile(file)
		if err != nil {
			return nil, err
		}
		namespaces = append(namespaces, ns)
	}
	return namespaces, nil
}

// InsertMapper 插入（或替换）一个已解析的 Mapper（按 namespace 键）
func (r *MapperRegistry) InsertMapper(mapper *core.Mapper) {
	r.mu.Lock()
	r.mappers[mapper.Namespace] = mapper
	r.mu.Unlock()
}

// GetMapper 按 namespace 查找 Mapper；不存在返回 nil。
//
// 返回的指针仅作只读使用（解析产物在重注册前不会被修改）。
func (r *MapperRegistry) GetMapper(namespace string) *core.Mapper {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mappers[namespace]
}

// Namespaces 当前已注册的所有 namespace（排序返回，便于稳定输出）
func (r *MapperRegistry) Namespaces() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ns := make([]string, 0, len(r.mappers))
	for k := range r.mappers {
		ns = append(ns, k)
	}
	sort.Strings(ns)
	return ns
}

// Len 已注册的 Mapper 数量
func (r *MapperRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.mappers)
}

// IsEmpty 是否为空
func (r *MapperRegistry) IsEmpty() bool { return r.Len() == 0 }

// TypeAliasRegistry 类型别名注册表：XML 短名 → Go 全限定类型名
type TypeAliasRegistry struct {
	aliases map[string]string
}

// NewTypeAliasRegistry 创建空注册表
func NewTypeAliasRegistry() *TypeAliasRegistry {
	return &TypeAliasRegistry{aliases: map[string]string{}}
}

// TypeAliasRegistryFromMap 从配置 map 构造
func TypeAliasRegistryFromMap(aliases map[string]string) *TypeAliasRegistry {
	m := make(map[string]string, len(aliases))
	for k, v := range aliases {
		m[k] = v
	}
	return &TypeAliasRegistry{aliases: m}
}

// Register 注册一个别名
func (r *TypeAliasRegistry) Register(alias, fullPath string) {
	r.aliases[alias] = fullPath
}

// Resolve 解析类型名：若是已注册别名则返回全限定名，否则原样返回
func (r *TypeAliasRegistry) Resolve(typeName string) string {
	if v, ok := r.aliases[typeName]; ok {
		return v
	}
	return typeName
}
