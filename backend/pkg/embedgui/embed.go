//go:build !tui_only

package embedgui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"runtime"
	"strconv"
	"sync"

	"dsh-go/pkg/gateway"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/tools"
)

//go:embed assets/dsh.pck
var EmbeddedAssets embed.FS

const (
	pckAsset    = "assets/dsh.pck"
	runnerAsset = "assets/godot_runner.exe"
)

// assetHashResult memoizes embedded-asset hash resolution per asset name:
// embed.FS content is immutable for the process lifetime, so each launch
// hashes the ~350MB of embedded GUI assets at most once no matter how many
// disk copies need checking against it.
type assetHashResult struct {
	hex string
	err error
}

var assetHashCache sync.Map // name -> assetHashResult

// assetDataSource reads the raw bytes of a named embedded asset. Package-level
// indirection so tests can serve a small fake asset set instead of the real
// ~181MB runner (hermetic + fast) without embedding anything extra.
var assetDataSource = embeddedAssetData

// hashEmbeddedBytes computes the hex content hash of embedded asset bytes.
// Package-level indirection so tests can count exactly how often the heavy
// full-hash path runs (a warm launch must run it zero times).
var hashEmbeddedBytes = sha256Hex

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// exeStamp is the cheap identity of the running binary that keys the
// persisted embedded-asset hash records: size + mtime. Replacing or
// rebuilding the exe changes at least one of them, while the check itself
// costs one os.Stat. A whole-exe CONTENT hash would re-read the same ~181MB
// the cache exists to avoid and is deliberately NOT used.
type exeStamp struct {
	size    int64
	mtimeNS int64
}

var (
	exeStampOnce sync.Once
	exeStampVal  exeStamp
)

// currentExeStamp stats the running binary once per process. A zero stamp
// (stat failed) never matches a persisted record, so the full-hash path —
// the historical behavior — is what runs when no fingerprint can be
// established.
func currentExeStamp() exeStamp {
	exeStampOnce.Do(func() {
		path, err := os.Executable()
		if err != nil {
			return
		}
		fi, err := os.Stat(path)
		if err != nil || !fi.Mode().IsRegular() {
			return
		}
		exeStampVal = exeStamp{size: fi.Size(), mtimeNS: fi.ModTime().UnixNano()}
	})
	return exeStampVal
}

// embeddedAssetSize reports the size of a named embedded asset without
// reading its bytes (fs.Stat walks the embed.FS header only). The runner is
// registered in the GOOS-specific runner embed FS, hence the dispatch.
func embeddedAssetSize(name string) (int64, bool) {
	if name == runnerAsset {
		return runnerEmbeddedSize()
	}
	fi, err := fs.Stat(EmbeddedAssets, name)
	if err != nil {
		return 0, false
	}
	return fi.Size(), true
}

// embeddedAssetHash returns the hex sha256 of a named embedded asset without
// retaining the raw bytes (a cached []byte copy would pin hundreds of MB of
// heap for the process lifetime); extraction re-reads the bytes only on the
// rare mismatch path. Process-level memoization only — callers that have a
// concrete runtime cache dir go through embeddedCachedHash to also persist
// the result across launches.
func embeddedAssetHash(name string) (string, error) {
	return embeddedCachedHash(name, "")
}

// EmbeddedPCKHash returns the SHA-256 of the PCK baked into this binary.
// It is exposed for host.describe so a running GUI can prove which frontend
// payload it launched.
func EmbeddedPCKHash() (string, error) {
	return embeddedAssetHash(pckAsset)
}

// embeddedCachedHash resolves the hex sha256 of a named embedded asset and,
// when stampTarget is non-empty, lets the result outlive this process: it is
// persisted as the second record of that target's sidecar stamp file, keyed
// by the exe fingerprint (size + mtime) and the embedded asset size. A later
// launch whose fingerprint still matches resolves the hash from a few stats
// instead of re-reading the ~181MB of embedded bytes; any exe change (rebuild
// or replacement) or asset-size drift invalidates the record and produces
// exactly one full re-hash — the cost every cold launch already paid.
func embeddedCachedHash(name, stampTarget string) (string, error) {
	if v, ok := assetHashCache.Load(name); ok {
		r := v.(assetHashResult)
		return r.hex, r.err
	}
	if es := currentExeStamp(); es.size > 0 {
		if sz, ok := embeddedAssetSize(name); ok {
			if rec, ok := readEmbeddedStamp(stampTarget); ok && rec.validFor(es, sz) {
				r := assetHashResult{hex: rec.hashHex}
				assetHashCache.Store(name, r)
				return r.hex, nil
			}
		}
	}
	data, err := assetDataSource(name)
	if err != nil {
		r := assetHashResult{err: err}
		assetHashCache.Store(name, r)
		return "", err
	}
	hex := hashEmbeddedBytes(data)
	assetHashCache.Store(name, assetHashResult{hex: hex})
	persistEmbeddedStamp(stampTarget, name, hex)
	return hex, nil
}

// persistEmbeddedStamp best-effort records a freshly computed embedded hash.
// Best-effort like the disk stamps: an unwritable sidecar or an unknown
// exe/asset size only costs a full re-hash on the next launch.
func persistEmbeddedStamp(stampTarget, name, hashHex string) {
	if stampTarget == "" {
		return
	}
	es := currentExeStamp()
	if es.size <= 0 {
		return
	}
	sz, ok := embeddedAssetSize(name)
	if !ok {
		return
	}
	writeEmbeddedStamp(stampTarget, embeddedStamp{
		exeSize:    es.size,
		exeMtimeNS: es.mtimeNS,
		assetSize:  sz,
		hashHex:    hashHex,
	})
}

// embeddedAssetData reads the raw bytes of a named embedded asset; used only
// on the extraction (mismatch) path. The PCK is embedded for every GOOS
// from EmbeddedAssets; the runner is embedded per-GOOS and read through
// readRunnerAsset (Windows PE on windows builds; non-Windows builds embed NO
// runner — W7-a — and readRunnerAsset reports fs.ErrNotExist there).
func embeddedAssetData(name string) ([]byte, error) {
	if name == runnerAsset {
		return readRunnerAsset(name)
	}
	return EmbeddedAssets.ReadFile(name)
}

// fileMatchesHash reports whether path exists with exactly the given content
// hash.
func fileMatchesHash(path, wantHex string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == wantHex
}

// diskStamp is the sidecar fingerprint written to <file>.sha256 after a disk
// copy has been fully verified against the embedded hash. Format:
// "<hex> <size> <mtimeUnixNano>". A later launch trusts it only while the
// file's size AND mtime are unchanged and the recorded hash equals the
// current embedded build's hash — any replacement (binary upgrade, manual
// swap, re-extract) changes at least one of them and forces the full-hash
// path again.
type diskStamp struct {
	hashHex string
	size    int64
	mtimeNS int64
}

// embeddedStamp is the launch-cost record for the embedded assets themselves,
// carried as a second line of the SAME sidecar stamp file:
//
//	dshx-embedded <exeSize> <exeMtimeUnixNano> <assetSize> <embeddedHex>
//
// It answers "what does THIS exe's embedded asset hash to" across launches
// without re-reading the asset. It lives next to the extracted copies
// (<UserCacheDir>/dsh/runtime/*.sha256) and is validated by three cheap
// stats (exe size, exe mtime, embedded asset size) — not by hashing anything.
// A missing, corrupt or foreign-keyed record degrades to the historical
// full-hash path, which then refreshes the record.
type embeddedStamp struct {
	exeSize    int64
	exeMtimeNS int64
	assetSize  int64
	hashHex    string
}

const embeddedStampTag = "dshx-embedded"

// validFor reports whether the record describes exactly this exe and this
// embedded asset size. Keyed by cheap stats, not by hashing the exe.
func (s embeddedStamp) validFor(es exeStamp, assetSize int64) bool {
	return s.hashHex != "" && es.size > 0 &&
		s.exeSize == es.size &&
		s.exeMtimeNS == es.mtimeNS &&
		s.assetSize == assetSize
}

func sidecarPath(path string) string { return path + ".sha256" }

// parseSidecarFiles tolerantly parses the (up to) two-record sidecar format:
// line 1 is the v1 disk-copy stamp, line 2 the embedded record. Unknown or
// malformed lines are skipped individually rather than invalidating the whole
// file; a sidecar that lost both records simply forces full re-verification.
func parseSidecarFiles(raw string) (disk diskStamp, diskOK bool, emb embeddedStamp, embOK bool) {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 3 && len(fields[0]) == 64:
			size, errSize := strconv.ParseInt(fields[1], 10, 64)
			mtimeNS, errMtime := strconv.ParseInt(fields[2], 10, 64)
			if errSize == nil && errMtime == nil {
				disk, diskOK = diskStamp{hashHex: fields[0], size: size, mtimeNS: mtimeNS}, true
			}
		case len(fields) == 5 && fields[0] == embeddedStampTag:
			exeSize, errExe := strconv.ParseInt(fields[1], 10, 64)
			exeMtimeNS, errExeMtime := strconv.ParseInt(fields[2], 10, 64)
			assetSize, errAsset := strconv.ParseInt(fields[3], 10, 64)
			if errExe == nil && errExeMtime == nil && errAsset == nil && len(fields[4]) == 64 {
				emb, embOK = embeddedStamp{exeSize: exeSize, exeMtimeNS: exeMtimeNS, assetSize: assetSize, hashHex: fields[4]}, true
			}
		}
	}
	return
}

func readSidecarRecords(target string) (disk diskStamp, diskOK bool, emb embeddedStamp, embOK bool) {
	raw, err := os.ReadFile(sidecarPath(target))
	if err != nil {
		return diskStamp{}, false, embeddedStamp{}, false
	}
	return parseSidecarFiles(string(raw))
}

func readDiskStamp(path string) (diskStamp, bool) {
	disk, ok, _, _ := readSidecarRecords(path)
	return disk, ok
}

// readEmbeddedStamp reads the embedded-record line from the sidecar of the
// given target file.
func readEmbeddedStamp(target string) (embeddedStamp, bool) {
	_, _, emb, ok := readSidecarRecords(target)
	return emb, ok
}

// writeSidecar rewrites the sidecar file of target, updating only the records
// handed in as non-nil and preserving the others (each writer owns its own
// line, so a stamp refresh never wipes the embedded record and vice versa).
func writeSidecar(target string, setDisk *diskStamp, setEmb *embeddedStamp) {
	disk, diskOK, emb, embOK := readSidecarRecords(target)
	if setDisk != nil {
		disk, diskOK = *setDisk, true
	}
	if setEmb != nil {
		emb, embOK = *setEmb, true
	}
	var b strings.Builder
	if diskOK {
		fmt.Fprintf(&b, "%s %d %d\n", disk.hashHex, disk.size, disk.mtimeNS)
	}
	if embOK {
		fmt.Fprintf(&b, "%s %d %d %d %s\n", embeddedStampTag, emb.exeSize, emb.exeMtimeNS, emb.assetSize, emb.hashHex)
	}
	_ = os.WriteFile(sidecarPath(target), []byte(b.String()), 0644)
}

// writeDiskStamp records the file's current size+mtime alongside its verified
// content hash, preserving the embedded record line. Best-effort: failure
// (e.g. read-only install dir) only costs a full hash on the next launch.
func writeDiskStamp(target, hashHex string) {
	fi, err := os.Stat(target)
	if err != nil {
		return
	}
	writeSidecar(target, &diskStamp{hashHex: hashHex, size: fi.Size(), mtimeNS: fi.ModTime().UnixNano()}, nil)
}

// writeEmbeddedStamp persists the embedded-asset hash record under the
// current exe fingerprint, preserving the disk-copy stamp line. Best-effort,
// like writeDiskStamp.
func writeEmbeddedStamp(target string, stamp embeddedStamp) {
	writeSidecar(target, nil, &stamp)
}

func stampMatchesDisk(stamp diskStamp, path string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return fi.Size() == stamp.size && fi.ModTime().UnixNano() == stamp.mtimeNS
}

// diskCopyMatches reports whether the file at path holds exactly the content
// the given embedded hash describes. Fast path: a sidecar fingerprint written
// after a previous full verification, trusted only while size+mtime are
// unchanged AND its recorded hash equals the current embedded build's hash
// (so a rebuilt binary invalidates stale copies). Slow path: full sha256; on
// success the sidecar is refreshed for subsequent launches.
func diskCopyMatches(hashHex, path string) bool {
	if stamp, ok := readDiskStamp(path); ok &&
		stamp.hashHex == hashHex && stampMatchesDisk(stamp, path) {
		return true
	}
	if fileMatchesHash(path, hashHex) {
		writeDiskStamp(path, hashHex)
		return true
	}
	return false
}

// EnsureExtracted checks and extracts the embedded Godot runner and PCK asset to the user runtime cache.
func EnsureExtracted() (runnerPath string, pckPath string, err error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = os.TempDir()
	}
	// The extraction target and the sidecar stamp home are the same
	// <UserCacheDir>/dsh/runtime directory.
	return ensureExtractedInto(filepath.Join(cacheDir, "dsh", "runtime"))
}

// ensureExtractedInto is EnsureExtracted against an explicit runtime dir;
// split out so tests can drive the extract + hash-cache logic hermetically.
func ensureExtractedInto(runtimeDir string) (runnerPath string, pckPath string, err error) {
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return "", "", err
	}

	// pck 与 runner 统一走同一条内容指纹缓存路径（dsh/runtime）。
	runnerName := "godot_runner"
	if runtime.GOOS == "windows" {
		runnerName += ".exe"
	}
	runnerPath = filepath.Join(runtimeDir, runnerName)
	cachePckPath := filepath.Join(runtimeDir, "dsh.pck")

	// Embedded fingerprints resolve through the persisted sidecar records
	// (keyed by the exe fingerprint): a warm launch answers them from a few
	// stats instead of re-reading and re-hashing the ~181MB of embedded
	// bytes; cold launches compute and persist the record once.
	pckSum, pckErr := embeddedCachedHash(pckAsset, cachePckPath)

	// Check if dsh.pck is located alongside the executable first; the local
	// copy wins over the cache so a shipped dsh.pck is preferred — but only
	// when it is byte-identical to this binary's embedded frontend. A stale
	// copy left behind by an upgrade must never silently shadow the shipped
	// UI (版本漂移防护).
	localPck := ""
	if execPath, err := os.Executable(); err == nil {
		localPck = filepath.Join(filepath.Dir(execPath), "dsh.pck")
	}
	pckPath = resolvePckPath(cachePckPath, localPck, pckSum, pckErr)

	// 1. Extract dsh.pck into the cache only when its content differs from the
	//    embedded build (fast path: sidecar fingerprint; slow path: full sha256).
	if pckPath == cachePckPath && pckErr == nil && !diskCopyMatches(pckSum, cachePckPath) {
		if data, derr := assetDataSource(pckAsset); derr != nil {
			log.Printf("[DSHX] warning: cannot read embedded %s: %v", pckAsset, derr)
		} else if werr := writeAtomic(cachePckPath, data); werr != nil {
			log.Printf("[DSHX] warning: failed to extract dsh.pck to %s: %v", cachePckPath, werr)
		} else {
			// The extracted bytes are by construction the hashed content, so
			// this is a verified-after-write moment for the sidecar stamp.
			writeDiskStamp(cachePckPath, pckSum)
		}
	}

	// 2. Extract embedded runner if present for this GOOS. The runner is
	// embedded per-GOOS: the Windows build carries a Windows PE
	// (assets/godot_runner.exe); non-Windows builds embed NO runner at all
	// (see runner_other.go), so nothing is extracted here on those platforms
	// — extracting a PE under a Linux name would guarantee exec format errors
	// at launch time. Non-Windows relies on the system godot probe below.
	if runtime.GOOS == "windows" {
		if sum, aerr := embeddedCachedHash(runnerAsset, runnerPath); aerr == nil {
			if !diskCopyMatches(sum, runnerPath) || !fileHasPEMagic(runnerPath) {
				if data, derr := assetDataSource(runnerAsset); derr != nil {
					log.Printf("[DSHX] warning: cannot read embedded %s: %v", runnerAsset, derr)
				} else if werr := writeAtomic(runnerPath, data); werr != nil {
					log.Printf("[DSHX] warning: failed to extract godot runner to %s: %v", runnerPath, werr)
				} else {
					writeDiskStamp(runnerPath, sum)
				}
			}
		}
	}

	// 3. Resolve the runner: extracted embedded runner > DSH_GODOT env >
	// system godot on PATH (platform-appropriate name). No developer-machine
	// absolute paths are compiled into release binaries.
	if !fileExists(runnerPath) || !isExecutableImage(runnerPath) {
		runnerPath = findSystemGodot()
	}
	return runnerPath, pckPath, nil
}

// resolvePckPath picks between the exe-side dsh.pck and the runtime-cache
// copy. The local copy wins only when verified byte-identical to the embedded
// build; anything else (missing file, stale after upgrade, unreadable
// embedded asset) logs why and falls back to the cache copy that step 1 of
// EnsureExtracted maintains.
func resolvePckPath(cachePckPath, localPck, embeddedHex string, embeddedErr error) string {
	if localPck == "" || !fileExists(localPck) {
		return cachePckPath
	}
	if embeddedErr != nil {
		log.Printf("[DSHX] warning: cannot verify dsh.pck beside executable (%s): %v; using embedded/cache version", localPck, embeddedErr)
		return cachePckPath
	}
	if diskCopyMatches(embeddedHex, localPck) {
		return localPck
	}
	log.Printf("[DSHX] warning: dsh.pck beside executable (%s) does not match this binary's embedded frontend; ignoring the stale copy and using the embedded version", localPck)
	return cachePckPath
}

// hasPEMagic reports whether b starts with the Windows PE "MZ" marker.
func hasPEMagic(b []byte) bool {
	return len(b) >= 2 && b[0] == 'M' && b[1] == 'Z'
}

// fileHasPEMagic reads the first two bytes of path and checks the MZ marker.
func fileHasPEMagic(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return hasPEMagic(magic[:])
}

// isExecutableImage validates that a candidate runner on disk looks like an
// executable image this platform can run (PE "MZ" on Windows, ELF "\x7fELF"
// elsewhere). Guards against executing a stale or foreign-architecture blob.
func isExecutableImage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return hasPEMagic(magic)
	}
	return magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F'
}

// findSystemGodot probes DSH_GODOT then PATH for a Godot binary usable as the
// GUI runner. Returns "" when nothing usable exists.
func findSystemGodot() string {
	if custom := os.Getenv("DSH_GODOT"); custom != "" {
		if fileExists(custom) && isExecutableImage(custom) {
			return custom
		}
	}
	names := []string{"godot4", "godot"}
	if runtime.GOOS == "windows" {
		names = []string{"godot4.exe", "godot.exe", "godot4", "godot"}
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil && isExecutableImage(p) {
			return p
		}
	}
	return ""
}

// writeAtomic writes data to path via temp-file + rename so a crash mid-write
// never leaves a truncated cache entry that later hash checks would treat as
// fresh.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0755); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LaunchAllInOneGUI starts the embedded HTTP/WS gateway and launches the Godot desktop window.
func LaunchAllInOneGUI(host string, port int, store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter) error {
	return LaunchAllInOneGUIWithServer(host, port, gateway.NewServer(store, toolReg, adapter))
}

// LaunchAllInOneGUIWithServer starts the given (already wired) gateway and
// launches the Godot desktop window. "Already wired" means subagent and
// approval hooks were attached before this call.
func LaunchAllInOneGUIWithServer(host string, port int, srv *gateway.Server) error {
	addr := fmt.Sprintf("%s:%d", host, port)

	// Bind first, serve later: a failed bind (port already taken by another
	// process) must abort the launch instead of silently handing the GUI to
	// whatever foreign server occupies the port.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gateway bind failed on %s: %w", addr, err)
	}
	go func() {
		_ = http.Serve(listener, srv.Routes())
	}()

	// The URL the front end must dial, derived from the listener's concrete
	// address so --host/--port are honored end to end.
	gatewayURL := dialableGatewayURL(listener, port)

	fmt.Println("=================================================================")
	fmt.Printf(" [DSHX] DeepSeekHarnessX All-in-One Desktop GUI Starting...\n")
	fmt.Printf(" [DSHX] Go 1.25 Backend API Gateway: %s\n", gatewayURL)
	fmt.Println("=================================================================")

	// The listener we hold is by definition ready: no dial probe needed (a
	// dial probe cannot tell "our server" from "another process on the port").

	// 2. Extract self-contained Godot runner and PCK
	runnerPath, pckPath, err := EnsureExtracted()
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("failed to extract embedded GUI assets: %w", err)
	}
	if runnerPath == "" {
		// 无显示环境/无可用 runner：明确报错而非 select{} 挂死进程。
		_ = listener.Close()
		return errors.New("no usable Godot runner available; set DSH_GODOT or install godot on PATH")
	}

	// 3. Launch Godot GUI Window
	// 渲染 API 选择：DSHX_RENDER_API=vulkan|opengl|auto（缺省 auto）。
	// Vulkan 是现代路径且多 GPU 机器上通常选中 NVIDIA 独显（本机实测默认
	// 落在 Tesla M40）；老驱动/无 Vulkan 环境由 auto 在启动早期失败时回退
	// OpenGL。CUDA 不是图形渲染 API，不适用。
	renderAPI := normalizeRenderAPI(os.Getenv("DSHX_RENDER_API"))

	var cmd *exec.Cmd
	if fileExists(pckPath) {
		cmd = exec.Command(runnerPath, append(renderArgs(renderAPI), "--main-pack", pckPath)...)
	} else {
		log.Printf("[DSHX] warning: dsh.pck missing at %s; launching bare runner (project manager will open)", pckPath)
		cmd = exec.Command(runnerPath, renderArgs(renderAPI)...)
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// The Godot front end defaults to http://127.0.0.1:3080 and would
	// otherwise ignore the actual --host/--port (showing Backend offline or,
	// worse, attaching to a foreign instance). Inject the real endpoint;
	// os.Environ() keeps the inherited environment — including DSHX_WORKSPACE,
	// consumed by app.gd — intact for the child.
	cmd.Env = append(os.Environ(), "DSHX_GATEWAY_URL="+gatewayURL)

	if err := cmd.Start(); err != nil {
		// 启动失败返回错误让调用方决定回退（TUI）或以非零码退出，
		// 绝不 select{} 永久挂死。
		return fmt.Errorf("failed to launch GUI window (%s): %w", runnerPath, err)
	}

	fmt.Printf("[DSHX] Godot 4 GUI window launched successfully (PID: %d, render API: %s).\n", cmd.Process.Pid, renderAPI)

	if renderAPI == "auto" {
		// auto：Vulkan 先行。若进程在早期（渲染器初始化窗口内）异常退出，
		// 判定为 Vulkan 不可用，自动回退 OpenGL 再拉一次。
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err == nil {
				return nil // 正常退出（用户关窗）
			}
			log.Printf("[DSHX] Vulkan renderer exited early (%v); retrying with OpenGL (gl_compatibility)...", err)
			return launchGUIOnce(runnerPath, pckPath, gatewayURL, "opengl")
		case <-time.After(6 * time.Second):
			// 存活超过初始化窗口：Vulkan 会话正常，等待其退出。
			return <-done
		}
	}
	return cmd.Wait()
}

// renderArgs 把渲染 API 名映射为 Godot 引擎参数。空串（auto）默认 Vulkan。
func renderArgs(api string) []string {
	switch api {
	case "opengl":
		return []string{"--rendering-method", "gl_compatibility", "--rendering-driver", "opengl3"}
	default:
		return []string{"--rendering-method", "forward_plus", "--rendering-driver", "vulkan"}
	}
}

// normalizeRenderAPI 归一 DSHX_RENDER_API 取值；未知值按 auto 处理。
func normalizeRenderAPI(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "vulkan":
		return "vulkan"
	case "opengl", "gl", "opengl3":
		return "opengl"
	default:
		return "auto"
	}
}

// launchGUIOnce 以指定渲染 API 拉起一次 GUI 并等待退出（auto 回退路径用）。
func launchGUIOnce(runnerPath, pckPath, gatewayURL, api string) error {
	var cmd *exec.Cmd
	args := append(renderArgs(api), "--main-pack", pckPath)
	if fileExists(pckPath) {
		cmd = exec.Command(runnerPath, args...)
	} else {
		cmd = exec.Command(runnerPath)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "DSHX_GATEWAY_URL="+gatewayURL)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch GUI window with %s: %w", api, err)
	}
	fmt.Printf("[DSHX] Godot 4 GUI window relaunched (PID: %d, render API: %s).\n", cmd.Process.Pid, api)
	return cmd.Wait()
}

// dialableGatewayURL builds the HTTP URL the Godot front end should dial to
// reach this process's gateway. It is derived from the listener's concrete
// address, so wildcard binds (--host 0.0.0.0 / [::]) normalize to loopback —
// wildcard IPs are listen-side selectors, not endpoints every client can dial
// — and net.JoinHostPort bracket-wraps IPv6 literals correctly. fallbackPort
// only covers the impossible case of a non-TCP listener.
func dialableGatewayURL(l net.Listener, fallbackPort int) string {
	ta, ok := l.Addr().(*net.TCPAddr)
	if !ok || ta.Port <= 0 {
		return fmt.Sprintf("http://127.0.0.1:%d", fallbackPort)
	}
	host := "127.0.0.1"
	if ta.IP != nil && !ta.IP.IsUnspecified() {
		host = ta.IP.String()
	} else if ta.IP != nil && ta.IP.To4() == nil {
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(ta.Port))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
