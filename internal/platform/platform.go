// Package platform detects the host OS, CPU architecture, RAM and GPU
// acceleration options, and maps them onto runtime folders on the drive.
package platform

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Info describes the host computer.
type Info struct {
	OS       string   `json:"os"`   // windows, macos, linux
	Arch     string   `json:"arch"` // x64, arm64
	CPUs     int      `json:"cpus"`
	RAMBytes uint64   `json:"ram_bytes"`
	Accel    []string `json:"accel"` // detected accelerators, best first: cuda, metal, vulkan
}

// Detect inspects the host. Every probe is best-effort: a failed probe means
// "not detected", and CPU inference is always the fallback.
func Detect() Info {
	info := Info{OS: osName(), Arch: archName(), CPUs: runtime.NumCPU()}
	info.RAMBytes = totalRAM()
	info.Accel = detectAccel(info)
	return info
}

func osName() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func archName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	default:
		return runtime.GOARCH
	}
}

// RAMGB returns total RAM in whole gigabytes (0 if unknown).
func (i Info) RAMGB() int { return int(i.RAMBytes / (1 << 30)) }

// Slug is the platform part of folder names, e.g. "windows-x64".
func (i Info) Slug() string { return i.OS + "-" + i.Arch }

// RuntimeCandidates lists runtime folder names under <drive>/runtime to try,
// best first. CPU is always last so it acts as the universal fallback.
func (i Info) RuntimeCandidates() []string {
	if i.OS == "macos" {
		// macOS llama.cpp builds include Metal on Apple Silicon.
		return []string{i.Slug()}
	}
	var out []string
	for _, a := range i.Accel {
		out = append(out, i.Slug()+"-"+a)
	}
	return append(out, i.Slug()+"-cpu")
}

// ServerBinary is the llama-server executable name on this OS.
func (i Info) ServerBinary() string {
	if i.OS == "windows" {
		return "llama-server.exe"
	}
	return "llama-server"
}

func detectAccel(i Info) []string {
	var out []string
	switch i.OS {
	case "macos":
		if i.Arch == "arm64" {
			out = append(out, "metal")
		}
	case "windows", "linux":
		if hasNvidia(i.OS) {
			out = append(out, "cuda")
		}
		if hasVulkan(i.OS) {
			out = append(out, "vulkan")
		}
	}
	return out
}

func hasNvidia(osName string) bool {
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		return true
	}
	if osName == "windows" {
		return fileExists(filepath.Join(os.Getenv("SystemRoot"), "System32", "nvcuda.dll"))
	}
	return fileExists("/proc/driver/nvidia/version")
}

func hasVulkan(osName string) bool {
	if osName == "windows" {
		return fileExists(filepath.Join(os.Getenv("SystemRoot"), "System32", "vulkan-1.dll"))
	}
	for _, dir := range []string{"/usr/lib/x86_64-linux-gnu", "/usr/lib/aarch64-linux-gnu", "/usr/lib64", "/usr/lib", "/lib/x86_64-linux-gnu"} {
		if fileExists(filepath.Join(dir, "libvulkan.so.1")) {
			return true
		}
	}
	return false
}

func totalRAM() uint64 {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return 0
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && fields[0] == "MemTotal:" {
				kb, _ := strconv.ParseUint(fields[1], 10, 64)
				return kb * 1024
			}
		}
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			n, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
			return n
		}
	case "windows":
		out, err := hiddenCommand("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory").Output()
		if err == nil {
			n, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
			return n
		}
	}
	return 0
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
