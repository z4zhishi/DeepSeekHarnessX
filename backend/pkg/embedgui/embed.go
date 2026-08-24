package embedgui

import (
	"embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"dsh-go/pkg/gateway"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/tools"
)

//go:embed assets/*
var EmbeddedAssets embed.FS

// EnsureExtracted checks and extracts the embedded Godot runner and PCK asset to the user runtime cache.
func EnsureExtracted() (runnerPath string, pckPath string, err error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = os.TempDir()
	}
	runtimeDir := filepath.Join(cacheDir, "dsh", "runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return "", "", err
	}

	runnerName := "godot_runner"
	if runtime.GOOS == "windows" {
		runnerName += ".exe"
	}
	runnerPath = filepath.Join(runtimeDir, runnerName)
	pckPath = filepath.Join(runtimeDir, "dsh.pck")

	// Check if dsh.pck is located alongside the executable first
	if execPath, err := os.Executable(); err == nil {
		localPck := filepath.Join(filepath.Dir(execPath), "dsh.pck")
		if fileExists(localPck) {
			pckPath = localPck
		}
	}

	// 1. Extract dsh.pck if not using local
	if pckPath != filepath.Join(filepath.Dir(os.Args[0]), "dsh.pck") {
		pckData, err := EmbeddedAssets.ReadFile("assets/dsh.pck")
		if err == nil && len(pckData) > 0 {
			_ = os.WriteFile(pckPath, pckData, 0644)
		}
	}

	// 2. Extract embedded runner if present
	runnerFile, err := EmbeddedAssets.Open("assets/godot_runner.exe")
	if err == nil {
		defer runnerFile.Close()
		stat, _ := runnerFile.Stat()
		if !fileMatchesSize(runnerPath, stat.Size()) {
			out, err := os.OpenFile(runnerPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err == nil {
				_, _ = io.Copy(out, runnerFile)
				out.Close()
			}
		}
	}

	// If no embedded runner was extracted, fallback to local/system godot
	if !fileExists(runnerPath) {
		if localGodot, err := exec.LookPath("godot.exe"); err == nil {
			runnerPath = localGodot
		} else if fileExists(`I:\KH\Teplix\DSHX\.tools\godot\godot.exe`) {
			runnerPath = `I:\KH\Teplix\DSHX\.tools\godot\godot.exe`
		}
	}

	return runnerPath, pckPath, nil
}

// LaunchAllInOneGUI starts the embedded HTTP/WS gateway and launches the Godot desktop window.
func LaunchAllInOneGUI(port int, store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter) error {
	return LaunchAllInOneGUIWithServer(port, gateway.NewServer(store, toolReg, adapter))
}

// LaunchAllInOneGUIWithServer starts the given (already wired) gateway and
// launches the Godot desktop window. "Already wired" means subagent and
// approval hooks were attached before this call.
func LaunchAllInOneGUIWithServer(port int, srv *gateway.Server) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// 1. Start Go HTTP/WS Gateway in background
	go func() {
		_ = http.ListenAndServe(addr, srv.Routes())
	}()

	fmt.Println("=================================================================")
	fmt.Printf(" [DSHX] DeepSeekHarnessX All-in-One Desktop GUI Starting...\n")
	fmt.Printf(" [DSHX] Go 1.25 Backend API Gateway: http://%s\n", addr)
	fmt.Println("=================================================================")

	// Wait until the gateway is actually listening on the port (deterministic
	// readiness instead of a fixed sleep). Falls through if the port is already
	// bound or the probe errors, so a ready server is never delayed.
	waitForPort(addr)

	// 2. Extract self-contained Godot runner and PCK
	runnerPath, pckPath, err := EnsureExtracted()
	if err != nil {
		return fmt.Errorf("failed to extract embedded GUI assets: %w", err)
	}

	// 3. Launch Godot GUI Window
	var cmd *exec.Cmd
	if fileExists(pckPath) {
		cmd = exec.Command(runnerPath, "--main-pack", pckPath)
	} else {
		cmd = exec.Command(runnerPath)
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Printf("[DSHX] Failed to launch GUI window: %v\n", err)
		fmt.Printf("[DSHX] Backend server remains active on http://%s\n", addr)
		select {} // Keep server running
	}

	fmt.Printf("[DSHX] Godot 4 GUI window launched successfully (PID: %d).\n", cmd.Process.Pid)
	return cmd.Wait()
}

func fileMatchesSize(path string, expectedSize int64) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Size() == expectedSize
}

// waitForPort polls a TCP dial to addr until it succeeds, then returns. It is
// bounded (default ~1s) and treats a probe error as "not ready yet"; if the
// deadline is reached without the port opening, it returns anyway so the GUI
// launch is never blocked indefinitely by a listener that is slow or already
// gone. Polling is 10ms with a short first-delay so the common ready path adds
// almost no latency.
func waitForPort(addr string) {
	deadline := time.Now().Add(1 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
