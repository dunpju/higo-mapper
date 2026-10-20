package higomapper

// 读写分离路由（v0.3+）。
//
// 可选的只读副本池：配置了 [[read_environments]] 时，非事务读
// （select_* / stream）轮询路由到副本；写语句与事务内的一切操作
// 始终走主库。未配置副本则全部走主库（行为零变化）。
//
// # 路由规则
//
//   - 读（非事务）→ Pick() 轮询选副本
//   - 写、事务内的读、SELECT ... FOR UPDATE 语义由调用方自行保证
//     （事务内 session 已统一走主库）
//   - 副本延迟导致的读到旧数据是读写分离的固有代价，强一致读请在
//     事务内执行或直连主库
//
// # 配置（higo-mapper.toml）
//
//	[[read_environments]]
//	driver = "mysql"
//	url = "mysql://ro_user:pass@replica1:3306/db"
//
//	[[read_environments]]
//	driver = "mysql"
//	url = "mysql://ro_user:pass@replica2:3306/db"

import (
	"context"
	"sync/atomic"
)

// ReadRouter 只读副本路由器（原子轮询）
type ReadRouter struct {
	replicas []*Environment
	next     atomic.Uint64
}

// NewReadRouter 为每个配置创建环境并建立连接池；任一副本连接失败则
// 关闭已创建的并整体报错（避免半初始化状态）。
func NewReadRouter(ctx context.Context, configs []EnvironmentConfig) (*ReadRouter, error) {
	if len(configs) == 0 {
		return &ReadRouter{}, nil
	}
	replicas := make([]*Environment, 0, len(configs))
	for _, cfg := range configs {
		// 编程式配置可能未设置连接池参数：套用 TOML 同款默认
		if cfg.PoolMaxConnections == 0 && cfg.PoolMinConnections == 0 {
			d := defaultEnvironmentConfig()
			cfg.PoolMaxConnections, cfg.PoolMinConnections = d.PoolMaxConnections, d.PoolMinConnections
		}
		env, err := NewEnvironment(ctx, cfg)
		if err != nil {
			for _, e := range replicas {
				e.Close()
			}
			return nil, newConfigErrorWrap(err, "只读副本连接失败: %s", cfg.URL)
		}
		replicas = append(replicas, env)
	}
	return &ReadRouter{replicas: replicas}, nil
}

// Len 副本数量
func (r *ReadRouter) Len() int { return len(r.replicas) }

// IsEmpty 是否没有副本（此时读也应走主库）
func (r *ReadRouter) IsEmpty() bool { return len(r.replicas) == 0 }

// Pick 轮询选取下一个副本（原子递增取模；空池返回 nil）
func (r *ReadRouter) Pick() *Environment {
	if len(r.replicas) == 0 {
		return nil
	}
	i := r.next.Add(1)
	return r.replicas[int(i-1)%len(r.replicas)]
}

// Replica 按下标取副本（越界返回 nil）
func (r *ReadRouter) Replica(i int) *Environment {
	if i < 0 || i >= len(r.replicas) {
		return nil
	}
	return r.replicas[i]
}

// URLs 各副本连接 URL（诊断用）
func (r *ReadRouter) URLs() []string {
	out := make([]string, 0, len(r.replicas))
	for _, e := range r.replicas {
		out = append(out, e.URL())
	}
	return out
}

// Close 关闭全部副本连接池（返回首个错误）
func (r *ReadRouter) Close() error {
	var firstErr error
	for _, e := range r.replicas {
		if err := e.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
