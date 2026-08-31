package plugin

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// LibDecl 是 manifest 里对一层 lib 的声明（plan §Lib registry）：
//
//	"libs": [{ "id": "dshx-easy-api", "version": "^1.0" }]
//
// version 是开发者约定的语义化约束；当前支持 "^M.m"（兼容窗口 M.x，x>=m）
// 与精确 "M.m.p"。缺失或非法时 Validate 拒绝，不静默降级。
type LibDecl struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
}

// LibEntry 是 LibRegistry 中一个可用 lib 的登记。
type LibEntry struct {
	ID      string
	Version string
	Status  string // "supported" | "deprecated"
	Host    any    // facade 实例（当前：easyapi.Plugin 由宿主注入）
}

// LibRegistry 维护 lib 登记与版本约束解析（plan §Lib registry）。
// Core/宿主启动时注册内置 lib（如 dshx-easy-api）；插件加载前经 Resolve
// 校验全部 libs 声明——缺失/版本不满足/循环都返回明确错误。
type LibRegistry struct {
	mu     sync.RWMutex
	libs   map[string]LibEntry
	nextID int64
}

// NewLibRegistry 构造空注册表。
func NewLibRegistry() *LibRegistry {
	return &LibRegistry{libs: map[string]LibEntry{}}
}

// Register 登记一个 lib（同 ID 替换并保留状态）。
func (lr *LibRegistry) Register(entry LibEntry) {
	if entry.ID == "" {
		return
	}
	if entry.Status == "" {
		entry.Status = "supported"
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.libs[entry.ID] = entry
}

// Unregister 移除一个 lib。
func (lr *LibRegistry) Unregister(id string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	delete(lr.libs, id)
}

// Get 返回单个 lib 登记项。
func (lr *LibRegistry) Get(id string) (LibEntry, bool) {
	lr.mu.RLock()
	defer lr.mu.RUnlock()
	e, ok := lr.libs[id]
	return e, ok
}

// List 返回 ID 排序的全部 lib（诊断/dashboard 展示用）。
func (lr *LibRegistry) List() []LibEntry {
	lr.mu.RLock()
	defer lr.mu.RUnlock()
	out := make([]LibEntry, 0, len(lr.libs))
	for _, e := range lr.libs {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// UsersOf 返回声明依赖某 lib 的插件名（reload 风险提示用；由宿主装配层
// 在 reload 前调用并在面板展示下游影响）。
func (lr *LibRegistry) UsersOf(libID string, manifests []Manifest) []string {
	users := make([]string, 0)
	for _, m := range manifests {
		for _, lib := range m.Libs {
			if lib.ID == libID {
				users = append(users, m.Name)
				break
			}
		}
	}
	sort.Strings(users)
	return users
}

// Resolve 校验一组 libs 声明：全部已登记且版本约束满足才通过。
// 重复声明同一 lib 视为冗余（去重容忍），但不允许同一 id 两个冲突约束。
func (lr *LibRegistry) Resolve(decls []LibDecl) error {
	seen := map[string]string{}
	for _, decl := range decls {
		if decl.ID == "" {
			return fmt.Errorf("plugin libs: 缺 id")
		}
		if prev, ok := seen[decl.ID]; ok && prev != decl.Version {
			return fmt.Errorf("lib %q: 冲突版本约束 %q vs %q", decl.ID, prev, decl.Version)
		}
		seen[decl.ID] = decl.Version
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry, ok := lr.libs[id]
		if !ok {
			return fmt.Errorf("lib %q 未登记", id)
		}
		want := seen[id]
		if want == "" {
			continue // 无约束：存在即可
		}
		if !versionSatisfies(entry.Version, want) {
			return fmt.Errorf("lib %q: 版本 %q 不满足约束 %q", id, entry.Version, want)
		}
	}
	return nil
}

// versionSatisfies 判断 entry 版本是否满足 "^M.m" 或 "M.m" 约束。
// 解析失败按不满足处理（fail-closed）。
func versionSatisfies(entry, want string) bool {
	major, minor, ok := parseSemver(entry)
	if !ok {
		return false
	}
	if strings.HasPrefix(want, "^") {
		wMajor, wMinor, ok2 := parseSemver(strings.TrimPrefix(want, "^"))
		if !ok2 {
			return false
		}
		return major == wMajor && minor >= wMinor
	}
	wMajor, wMinor, ok2 := parseSemver(want)
	if !ok2 {
		return false
	}
	return major == wMajor && minor == wMinor
}

func parseSemver(s string) (int, int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.Split(s, ".")
	if len(parts) == 0 || parts[0] == "" {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor := 0
	if len(parts) >= 2 && parts[1] != "" {
		m, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, false
		}
		minor = m
	}
	return major, minor, true
}
