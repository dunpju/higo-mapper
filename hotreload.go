package higomapper

// Mapper 热重载监视器。
//
// 基于 fsnotify 监控文件系统事件，专用 goroutine 收集变更并去抖，
// 安静期（可配，最小 50ms）后批量重新解析变更的 XML 文件，
// 通过 MapperRegistry 原子替换（写锁内整体换入）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// minDebounceMs 最小去抖间隔（毫秒），防止过快刷新
const minDebounceMs = 50

// MapperWatcher Mapper 热重载监视器。
//
// 持有底层 fsnotify watcher 与去抖 worker goroutine；Close 时优雅关闭两者。
type MapperWatcher struct {
	watcher    *fsnotify.Watcher
	events     chan string
	stopped    chan struct{}
	stopOnce   sync.Once
	workerDone chan struct{}
}

// StartMapperWatcher 启动热重载监视器。
//
//   - registry：共享的 Mapper 注册表（变更后调用 RegisterFromFile 重新解析）
//   - watchDirs：待监视的目录列表（递归监视）
//   - debounceMs：去抖间隔（安静期后触发重解析，最小 50ms）
func StartMapperWatcher(registry *MapperRegistry, watchDirs []string, debounceMs uint64) (*MapperWatcher, error) {
	if len(watchDirs) == 0 {
		return nil, newFileWatcherError(nil, "无可监视的目录，热重载未启动")
	}
	if debounceMs < minDebounceMs {
		debounceMs = minDebounceMs
	}
	debounce := time.Duration(debounceMs) * time.Millisecond

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, newFileWatcherError(err, "创建 watcher 失败")
	}

	// 递归监视各目录（fsnotify 无原生递归：预扫描子目录逐个加入）
	watched := 0
	for _, dir := range watchDirs {
		n, err := watchRecursive(watcher, dir)
		if err != nil {
			watcher.Close()
			return nil, newFileWatcherError(err, "监视目录 %s 失败", dir)
		}
		watched += n
	}
	if watched == 0 {
		watcher.Close()
		return nil, newFileWatcherError(nil, "目录均不存在，热重载未启动: %v", watchDirs)
	}

	// 事件通道：fsnotify（同步回调 goroutine）→ 去抖 worker goroutine
	events := make(chan string, 256)
	mw := &MapperWatcher{
		watcher:    watcher,
		events:     events,
		stopped:    make(chan struct{}),
		workerDone: make(chan struct{}),
	}

	// 事件泵：fsnotify 事件 → events 通道
	go func() {
		defer close(events)
		for {
			select {
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if isXML(ev.Name) {
					select {
					case events <- ev.Name:
					default: // 通道满时丢弃（下个安静期仍会处理后续事件）
					}
				}
			case <-watcher.Errors:
				// 读错误忽略，继续监听
			case <-mw.stopped:
				return
			}
		}
	}()

	// 去抖 worker：安静期结束 → 批量重解析
	go func() {
		defer close(mw.workerDone)
		pending := map[string]struct{}{}
		timer := time.NewTimer(debounce)
		defer timer.Stop()
		for {
			select {
			case path, ok := <-events:
				if !ok {
					mw.flush(registry, pending)
					return
				}
				pending[path] = struct{}{}
				timer.Reset(debounce)
			case <-timer.C:
				mw.flush(registry, pending)
				timer.Reset(debounce)
			case <-mw.stopped:
				return
			}
		}
	}()

	return mw, nil
}

// flush 批量重解析并清空待处理集合
func (w *MapperWatcher) flush(registry *MapperRegistry, pending map[string]struct{}) {
	for path := range pending {
		reloadMapper(registry, path)
		delete(pending, path)
	}
}

// watchRecursive 递归加入目录监视，返回成功加入的目录数
func watchRecursive(w *fsnotify.Watcher, dir string) (int, error) {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return 0, nil
	}
	count := 0
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个子目录失败不影响其余
		}
		if d.IsDir() {
			if err := w.Add(path); err != nil {
				return nil
			}
			count++
		}
		return nil
	})
	if err != nil {
		return count, err
	}
	return count, nil
}

// IsRunning 是否正在运行
func (w *MapperWatcher) IsRunning() bool {
	return w.watcher != nil
}

// Close 优雅关闭：停事件泵 → 发停止信号 → 等 worker 退出
func (w *MapperWatcher) Close() {
	w.stopOnce.Do(func() {
		close(w.stopped)
		if w.watcher != nil {
			w.watcher.Close()
		}
		<-w.workerDone
		w.watcher = nil
	})
}

// reloadMapper 重新解析单个 mapper 文件并原子替换注册表中的 Mapper
func reloadMapper(registry *MapperRegistry, path string) {
	ns, err := registry.RegisterFromFile(path)
	if err != nil {
		watcherLogWarn("热重载失败 %s: %v", path, err)
		return
	}
	watcherLogInfo("热重载成功: %s (%s)", ns, path)
}

// isXML 路径是否为 XML 文件（扩展名大小写不敏感）
func isXML(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".xml")
}

// 轻量日志：默认写 stderr（避免依赖日志后端）
func watcherLogInfo(format string, args ...any) {
	if Logger != nil {
		Logger.Printf("[higo-mapper] "+format, args...)
	} else {
		fmt.Fprintf(os.Stderr, "[higo-mapper] "+format+"\n", args...)
	}
}

func watcherLogWarn(format string, args ...any) {
	if Logger != nil {
		Logger.Printf("[higo-mapper][WARN] "+format, args...)
	} else {
		fmt.Fprintf(os.Stderr, "[higo-mapper][WARN] "+format+"\n", args...)
	}
}

// ExtractWatchDirs 从 mapper glob 模式列表推导出需监视的目录。
//
// 规则：取每个模式中第一个通配符（* ? [ {）之前的静态前缀作为路径，
// 相对 baseDir 解析；若该路径指向文件（有扩展名），取其父目录。结果去重。
func ExtractWatchDirs(mapperPaths []string, baseDir string) []string {
	dirs := map[string]struct{}{}
	for _, pattern := range mapperPaths {
		// 截取首个通配符之前的静态前缀
		staticPrefix := pattern
		if i := strings.IndexAny(pattern, "*?[{"); i >= 0 {
			staticPrefix = pattern[:i]
		}
		trimmed := strings.TrimRight(staticPrefix, "/\\")
		if trimmed == "" {
			// 模式以通配符开头 → 监视 baseDir
			dirs[filepath.Clean(baseDir)] = struct{}{}
			continue
		}
		resolved := trimmed
		if !filepath.IsAbs(trimmed) {
			resolved = filepath.Join(baseDir, trimmed)
		}
		// 若解析结果像文件（有扩展名），取父目录
		if filepath.Ext(resolved) != "" {
			resolved = filepath.Dir(resolved)
		}
		dirs[filepath.Clean(resolved)] = struct{}{}
	}
	out := make([]string, 0, len(dirs))
	for d := range dirs {
		out = append(out, d)
	}
	return out
}
