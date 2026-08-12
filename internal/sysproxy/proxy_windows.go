//go:build windows

// Package sysproxy 负责 Windows 系统代理的设置、读取与恢复。
// 移植自参考项目 pkg/system/proxy_windows.go。
package sysproxy

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"

	"wx-mp-sync/internal/winutil"
)

var internetSetOption = windows.NewLazySystemDLL("wininet.dll").NewProc("InternetSetOptionW")

const (
	internetOptionRefresh          = 37
	internetOptionSettingsChanged  = 39
	internetSettingsRegPath        = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
)

// Settings 描述一组系统代理设置。
type Settings struct {
	Hostname string
	Port     string
}

// Enabled 判断设置是否启用了代理(hostname 非空)。
func (s *Settings) Enabled() bool {
	return s != nil && s.Hostname != "" && s.Port != ""
}

// Enable 设置系统代理为 hostname:port,并刷新系统使其生效。
func Enable(hostname, port string) error {
	proxyServer := fmt.Sprintf("%s:%s", hostname, port)
	if err := runRegCommand("add", internetSettingsRegPath, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "1", "/f"); err != nil {
		return fmt.Errorf("开启系统代理失败: %v", err)
	}
	if err := runRegCommand("add", internetSettingsRegPath, "/v", "ProxyServer", "/t", "REG_SZ", "/d", proxyServer, "/f"); err != nil {
		return fmt.Errorf("设置代理地址失败: %v", err)
	}
	return notifySettingsChanged()
}

// Disable 关闭系统代理。
func Disable() error {
	if err := runRegCommand("add", internetSettingsRegPath, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "0", "/f"); err != nil {
		return fmt.Errorf("关闭系统代理失败: %v", err)
	}
	return notifySettingsChanged()
}

// Fetch 读取当前系统代理设置;未启用则返回 nil。
func Fetch() (*Settings, error) {
	enableValue, err := readRegValue(internetSettingsRegPath, "ProxyEnable")
	if err != nil {
		return nil, err
	}
	if enableValue == "" {
		return nil, nil
	}
	enabled, err := strconv.ParseInt(strings.TrimSpace(enableValue), 0, 64)
	if err != nil || enabled == 0 {
		return nil, nil
	}
	serverValue, err := readRegValue(internetSettingsRegPath, "ProxyServer")
	if err != nil {
		return nil, err
	}
	if serverValue == "" {
		return nil, nil
	}
	host, port, err := parseProxyServerValue(serverValue)
	if err != nil {
		return nil, err
	}
	if host == "" || port == "" {
		return nil, nil
	}
	return &Settings{Hostname: host, Port: port}, nil
}

// Restore 将系统代理恢复为 saved 描述的状态:
// saved 非空且启用了代理 → 恢复原代理;否则关闭代理。
func Restore(saved *Settings) error {
	if saved.Enabled() {
		return Enable(saved.Hostname, saved.Port)
	}
	return Disable()
}

// notifySettingsChanged 通知 Windows 系统代理设置已变化。
func notifySettingsChanged() error {
	if err := internetSetOption.Find(); err != nil {
		return fmt.Errorf("加载 InternetSetOptionW 失败: %v", err)
	}
	for _, option := range []uintptr{internetOptionSettingsChanged, internetOptionRefresh} {
		result, _, callErr := internetSetOption.Call(0, option, 0, 0)
		if result == 0 {
			return fmt.Errorf("刷新系统代理设置失败: %v", callErr)
		}
	}
	return nil
}

// runRegCommand 执行 reg 命令;直接调用失败(如组策略锁定)时通过 UAC 提权重试。
func runRegCommand(args ...string) error {
	reg, err := exec.LookPath("reg")
	if err != nil {
		return err
	}
	cmd := exec.Command(reg, args...)
	if out, err := cmd.CombinedOutput(); err == nil {
		return nil
	} else {
		_ = out
	}
	if _, err := winutil.RunElevated(reg, args...); err != nil {
		return err
	}
	return nil
}

func readRegValue(path, name string) (string, error) {
	reg, err := exec.LookPath("reg")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(reg, "query", path, "/v", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		outputText := string(out)
		lower := strings.ToLower(outputText)
		if strings.Contains(lower, "unable to find") || strings.Contains(outputText, "找不到") || strings.Contains(outputText, "无法找到") {
			return "", nil
		}
		return "", fmt.Errorf("读取系统代理失败: %v", outputText)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, name) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		return fields[len(fields)-1], nil
	}
	return "", nil
}

func parseProxyServerValue(value string) (string, string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", "", nil
	}
	parts := strings.Split(raw, ";")
	candidate := pickProxyCandidate(parts, "http=")
	if candidate == "" {
		candidate = pickProxyCandidate(parts, "https=")
	}
	if candidate == "" {
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if idx := strings.Index(part, "="); idx >= 0 {
				candidate = strings.TrimSpace(part[idx+1:])
				break
			}
		}
	}
	if candidate == "" {
		candidate = raw
	}
	return splitHostPort(candidate)
}

func pickProxyCandidate(parts []string, prefix string) string {
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), prefix) {
			return strings.TrimSpace(part[len(prefix):])
		}
	}
	return ""
}

func splitHostPort(value string) (string, string, error) {
	candidate := strings.TrimSpace(value)
	if candidate == "" {
		return "", "", nil
	}
	if strings.HasPrefix(candidate, "[") {
		host, port, err := net.SplitHostPort(candidate)
		if err != nil {
			return "", "", fmt.Errorf("解析系统代理地址失败: %v", err)
		}
		return host, port, nil
	}
	idx := strings.LastIndex(candidate, ":")
	if idx <= 0 || idx == len(candidate)-1 {
		return "", "", errors.New("解析系统代理地址失败")
	}
	return candidate[:idx], candidate[idx+1:], nil
}
