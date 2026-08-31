package plugin

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const disabledFileName = ".disabled"

// PluginInfo 是 GUI / 二次开发列出插件时的公开视图。
type PluginInfo struct {
	Name         string   `json:"name"`
	ABIVersion   int      `json:"abiVersion"`
	Status       string   `json:"status"` // "mounted" | "installed" | "disabled" | "error"
	Command      string   `json:"command,omitempty"`
	Source       string   `json:"source,omitempty"` // builtin | external
	Capabilities []string `json:"capabilities,omitempty"`
	Error        string   `json:"error,omitempty"`
	Tools        []string `json:"tools,omitempty"`
}

// ListInfo 返回内置与外部插件的当前状态（按 Name 排序）。
func (r *Registry) ListInfo() []PluginInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]PluginInfo, 0, len(r.builtins)+len(r.external))
	seen := map[string]bool{}
	for name := range r.builtins {
		out = append(out, r.infoLocked(name))
		seen[name] = true
	}
	for name := range r.external {
		if seen[name] {
			continue
		}
		out = append(out, r.infoLocked(name))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) infoLocked(name string) PluginInfo {
	info := PluginInfo{Name: name, ABIVersion: ABIVersion}
	if _, ok := r.builtins[name]; ok {
		info.Source = "builtin"
	}
	if ext, ok := r.external[name]; ok {
		if info.Source == "" {
			info.Source = "external"
		}
		info.Command = ext.Command
		if ext.ABIVersion != 0 {
			info.ABIVersion = ext.ABIVersion
		}
		if len(ext.Capabilities) > 0 {
			info.Capabilities = append([]string{}, ext.Capabilities...)
		}
	}
	if r.tools != nil {
		info.Tools = r.OwnedTools(name)
	}
	switch {
	case r.disabled[name]:
		info.Status = "disabled"
	case r.lastErr[name] != "":
		info.Status = "error"
		info.Error = r.lastErr[name]
	case r.mounted[name] != nil:
		info.Status = "mounted"
	default:
		info.Status = "installed"
	}
	return info
}

// InstallFromPath 把插件复制进 destDir 并登记、Reconcile。
// src 可以是：含 manifest.json（或 <name>.json）+ 可执行文件的目录；
// 单份 .json manifest（相对 command 改写到 destDir）；或上述内容的 .zip。
func (r *Registry) InstallFromPath(ctx context.Context, src, destDir string) (PluginInfo, error) {
	if err := ctx.Err(); err != nil {
		return PluginInfo{}, err
	}
	src = strings.TrimSpace(src)
	destDir = strings.TrimSpace(destDir)
	if src == "" {
		return PluginInfo{}, fmt.Errorf("plugin: src 为空")
	}
	if destDir == "" {
		return PluginInfo{}, fmt.Errorf("plugin: destDir 为空")
	}
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return PluginInfo{}, err
	}
	destDirAbs, err := filepath.Abs(destDir)
	if err != nil {
		return PluginInfo{}, err
	}
	if err := os.MkdirAll(destDirAbs, 0755); err != nil {
		return PluginInfo{}, err
	}

	st, err := os.Stat(srcAbs)
	if err != nil {
		return PluginInfo{}, err
	}

	switch {
	case st.IsDir():
		return r.installTree(ctx, srcAbs, destDirAbs)
	case strings.EqualFold(filepath.Ext(srcAbs), ".zip"):
		return r.installZip(ctx, srcAbs, destDirAbs)
	case strings.EqualFold(filepath.Ext(srcAbs), ".json"):
		return r.installJSON(ctx, srcAbs, destDirAbs)
	default:
		return PluginInfo{}, fmt.Errorf("plugin: 不支持的安装源 %s（需要目录、.json 或 .zip）", src)
	}
}

func (r *Registry) installZip(ctx context.Context, zipPath, destDir string) (PluginInfo, error) {
	tmp, err := os.MkdirTemp("", "dsh-plugin-")
	if err != nil {
		return PluginInfo{}, err
	}
	defer os.RemoveAll(tmp)
	if err := extractZip(zipPath, tmp); err != nil {
		return PluginInfo{}, err
	}
	root, err := findPluginRoot(tmp)
	if err != nil {
		return PluginInfo{}, err
	}
	return r.installTree(ctx, root, destDir)
}

func (r *Registry) installJSON(ctx context.Context, src, destDir string) (PluginInfo, error) {
	ext, man, err := loadPluginManifest(src)
	if err != nil {
		return PluginInfo{}, err
	}
	if err := man.Validate(); err != nil {
		return PluginInfo{}, err
	}
	if !validPluginName(ext.Name) {
		return PluginInfo{}, fmt.Errorf("plugin: 非法插件名 %q", ext.Name)
	}
	if strings.TrimSpace(ext.Command) == "" {
		return PluginInfo{}, fmt.Errorf("plugin manifest %q: 缺 command", ext.Name)
	}

	raw, err := os.ReadFile(src)
	if err != nil {
		return PluginInfo{}, err
	}
	rewritten, err := rewriteManifestBytes(raw, destDir)
	if err != nil {
		return PluginInfo{}, err
	}
	r.Unload(ext.Name)
	top := filepath.Join(destDir, ext.Name+".json")
	if err := os.WriteFile(top, rewritten, 0644); err != nil {
		return PluginInfo{}, err
	}
	loaded, _, err := loadPluginManifest(top)
	if err != nil {
		return PluginInfo{}, err
	}
	return r.finishInstall(ctx, loaded, destDir, []string{top})
}

func (r *Registry) installTree(ctx context.Context, root, destDir string) (PluginInfo, error) {
	manPath, err := locateManifest(root)
	if err != nil {
		return PluginInfo{}, err
	}
	ext, man, err := loadPluginManifest(manPath)
	if err != nil {
		return PluginInfo{}, err
	}
	if err := man.Validate(); err != nil {
		return PluginInfo{}, err
	}
	if !validPluginName(ext.Name) {
		return PluginInfo{}, fmt.Errorf("plugin: 非法插件名 %q", ext.Name)
	}

	dest := filepath.Join(destDir, ext.Name)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return PluginInfo{}, err
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return PluginInfo{}, err
	}
	if rootAbs != destAbs {
		if isUnder(rootAbs, destAbs) {
			return PluginInfo{}, fmt.Errorf("plugin: src 与 destDir 路径重叠")
		}
		r.Unload(ext.Name)
		if err := os.RemoveAll(destAbs); err != nil {
			return PluginInfo{}, err
		}
		if err := copyTree(rootAbs, destAbs); err != nil {
			_ = os.RemoveAll(destAbs)
			return PluginInfo{}, err
		}
	}

	copiedMan, err := locateManifest(destAbs)
	if err != nil {
		_ = os.RemoveAll(destAbs)
		return PluginInfo{}, err
	}
	if _, err := rewriteManifestFile(copiedMan, destAbs); err != nil {
		_ = os.RemoveAll(destAbs)
		return PluginInfo{}, err
	}
	top := filepath.Join(destDir, ext.Name+".json")
	if err := copyFile(copiedMan, top); err != nil {
		_ = os.RemoveAll(destAbs)
		return PluginInfo{}, err
	}

	loaded, _, err := loadPluginManifest(top)
	if err != nil {
		_ = os.RemoveAll(destAbs)
		_ = os.Remove(top)
		return PluginInfo{}, err
	}
	if strings.TrimSpace(loaded.Command) == "" {
		_ = os.RemoveAll(destAbs)
		_ = os.Remove(top)
		return PluginInfo{}, fmt.Errorf("plugin manifest %q: 缺 command", loaded.Name)
	}
	return r.finishInstall(ctx, loaded, destDir, []string{destAbs, top})
}

func (r *Registry) finishInstall(ctx context.Context, ext External, destDir string, paths []string) (PluginInfo, error) {
	if err := ctx.Err(); err != nil {
		return PluginInfo{}, err
	}
	r.mu.Lock()
	if r.pluginDir == "" {
		r.pluginDir = destDir
	}
	if r.installed == nil {
		r.installed = map[string][]string{}
	}
	r.installed[ext.Name] = append([]string{}, paths...)
	delete(r.disabled, ext.Name)
	delete(r.lastErr, ext.Name)
	disabled := cloneDisabled(r.disabled)
	pluginDir := r.pluginDir
	r.mu.Unlock()

	_ = persistDisabled(pluginDir, disabled)
	r.Unload(ext.Name)
	r.AddExternal(ext)
	r.Reconcile(ctx)

	r.mu.Lock()
	info := r.infoLocked(ext.Name)
	r.mu.Unlock()
	return info, nil
}

// Uninstall 关闭 Host、从注册表移除；若该插件由 InstallFromPath 复制而来则删除目标文件。
// 内置能力不可卸载。
func (r *Registry) Uninstall(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("plugin: 缺 name")
	}
	r.mu.Lock()
	if _, ok := r.builtins[name]; ok {
		r.mu.Unlock()
		return fmt.Errorf("plugin: 不能卸载内置能力 %q", name)
	}
	_, inExt := r.external[name]
	copied := append([]string{}, r.installed[name]...)
	pluginDir := r.pluginDir
	if !inExt && len(copied) == 0 && !r.disabled[name] {
		r.mu.Unlock()
		return fmt.Errorf("plugin: 未找到 %q", name)
	}
	r.mu.Unlock()

	r.Unload(name)

	r.mu.Lock()
	delete(r.external, name)
	delete(r.installed, name)
	delete(r.lastErr, name)
	delete(r.hosts, name)
	delete(r.disabled, name)
	disabled := cloneDisabled(r.disabled)
	r.mu.Unlock()

	for _, p := range copied {
		if p == "" {
			continue
		}
		if pluginDir != "" && !isUnder(pluginDir, p) {
			continue
		}
		_ = os.RemoveAll(p)
	}
	if pluginDir != "" {
		sub := filepath.Join(pluginDir, name)
		top := filepath.Join(pluginDir, name+".json")
		if isUnder(pluginDir, sub) {
			_ = os.RemoveAll(sub)
		}
		if isUnder(pluginDir, top) {
			_ = os.Remove(top)
		}
		_ = persistDisabled(pluginDir, disabled)
	}
	return nil
}

// SetEnabled 停用时 Close+卸载但保留文件；启用时 Reconcile。
func (r *Registry) SetEnabled(name string, enabled bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("plugin: 缺 name")
	}
	r.mu.Lock()
	_, isB := r.builtins[name]
	_, isE := r.external[name]
	if !isB && !isE {
		r.mu.Unlock()
		return fmt.Errorf("plugin: 未找到 %q", name)
	}
	if r.disabled == nil {
		r.disabled = map[string]bool{}
	}
	if enabled {
		delete(r.disabled, name)
	} else {
		r.disabled[name] = true
	}
	disabled := cloneDisabled(r.disabled)
	pluginDir := r.pluginDir
	r.mu.Unlock()

	if err := persistDisabled(pluginDir, disabled); err != nil {
		return err
	}
	if !enabled {
		r.reconcileMu.Lock()
		r.Unload(name)
		r.reconcileMu.Unlock()
		return nil
	}
	r.Reconcile(context.Background())
	return nil
}

func loadDisabled(dir string) map[string]bool {
	out := map[string]bool{}
	if dir == "" {
		return out
	}
	data, err := os.ReadFile(filepath.Join(dir, disabledFileName))
	if err != nil {
		return out
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, line := range strings.Split(text, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		out[name] = true
	}
	return out
}

func persistDisabled(dir string, names map[string]bool) error {
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, disabledFileName)
	list := make([]string, 0, len(names))
	for n, on := range names {
		if on && n != "" {
			list = append(list, n)
		}
	}
	sort.Strings(list)
	if len(list) == 0 {
		_ = os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(list, "\n")+"\n"), 0644)
}

func cloneDisabled(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		if v {
			out[k] = true
		}
	}
	return out
}

func validPluginName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, c := range name {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func locateManifest(dir string) (string, error) {
	candidates := []string{filepath.Join(dir, "manifest.json")}
	base := filepath.Base(dir)
	if base != "" && base != "." {
		candidates = append(candidates, filepath.Join(dir, base+".json"))
	}
	for _, p := range candidates {
		if fileExists(p) {
			return p, nil
		}
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return "", err
	}
	var files []string
	for _, m := range matches {
		if fileExists(m) {
			files = append(files, m)
		}
	}
	if len(files) == 1 {
		return files[0], nil
	}
	if len(files) == 0 {
		return "", fmt.Errorf("plugin: 未找到 manifest.json")
	}
	return "", fmt.Errorf("plugin: 目录内有多个 json，请使用 manifest.json")
}

func findPluginRoot(dir string) (string, error) {
	if _, err := locateManifest(dir); err == nil {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "." && e.Name() != ".." {
			dirs = append(dirs, filepath.Join(dir, e.Name()))
		}
	}
	if len(dirs) == 1 {
		return findPluginRoot(dirs[0])
	}
	for _, d := range dirs {
		if _, err := locateManifest(d); err == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("plugin: 未找到 manifest.json（或 <name>.json）")
}

func rewriteManifestFile(path, destDir string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	out, err := rewriteManifestBytes(data, destDir)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, out, 0644); err != nil {
		return "", err
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", err
	}
	cmd, _ := raw["command"].(string)
	return cmd, nil
}

func rewriteManifestBytes(data []byte, destDir string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	cmd, _ := raw["command"].(string)
	raw["command"] = resolveCommand(cmd, destDir, true)
	if abi, ok := raw["abiVersion"]; !ok || abi == nil || abi == float64(0) || abi == 0 {
		raw["abiVersion"] = ABIVersion
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// resolveCommand 把相对 command 解析到 baseDir。force 时凡相对路径都拼到 baseDir
// （安装改写）；否则仅当目标文件存在才改写（ScanDir 加载）。
func resolveCommand(command, baseDir string, force bool) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return command
	}
	if filepath.IsAbs(command) {
		local := filepath.Join(baseDir, filepath.Base(command))
		if fileExists(local) {
			if abs, err := filepath.Abs(local); err == nil {
				return abs
			}
			return local
		}
		return command
	}
	local := filepath.Join(baseDir, command)
	exists := fileExists(local)
	rel := strings.HasPrefix(command, ".") || strings.ContainsAny(command, `/\`)
	if force || exists || rel {
		if abs, err := filepath.Abs(local); err == nil {
			return abs
		}
		return local
	}
	return command
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func isUnder(parent, child string) bool {
	parent, err1 := filepath.Abs(parent)
	child, err2 := filepath.Abs(child)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func copyTree(src, dest string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("plugin: 拒绝路径穿越 %q", rel)
		}
		if rel == "." {
			return os.MkdirAll(dest, 0755)
		}
		target := filepath.Join(dest, rel)
		if !isUnder(dest, target) {
			return fmt.Errorf("plugin: 拒绝路径穿越 %q", rel)
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dest string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	perm := st.Mode().Perm()
	if perm == 0 {
		perm = 0644
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

func extractZip(zipPath, dest string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destAbs, 0755); err != nil {
		return err
	}
	for _, f := range zr.File {
		target, err := safeJoin(destAbs, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() || strings.HasSuffix(filepath.ToSlash(f.Name), "/") {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := extractZipFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

func extractZipFile(f *zip.File, dest string) (err error) {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, rc)
	return err
}

// safeJoin 把 zip 条目拼到 dest 下；含 ".."、绝对路径或落到 dest 外则拒绝。
func safeJoin(dest, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("plugin: zip 条目名为空")
	}
	norm := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(norm, "/") || strings.HasPrefix(norm, "//") {
		return "", fmt.Errorf("plugin: 拒绝路径穿越 %q", name)
	}
	if len(norm) >= 2 && norm[1] == ':' {
		return "", fmt.Errorf("plugin: 拒绝路径穿越 %q", name)
	}
	parts := strings.Split(norm, "/")
	relParts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			return "", fmt.Errorf("plugin: 拒绝路径穿越 %q", name)
		}
		relParts = append(relParts, p)
	}
	if len(relParts) == 0 {
		return dest, nil
	}
	target := filepath.Join(append([]string{dest}, relParts...)...)
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if !isUnder(dest, targetAbs) {
		return "", fmt.Errorf("plugin: 拒绝路径穿越 %q", name)
	}
	return targetAbs, nil
}
