//go:build windows

package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"wx-mp-sync/internal/winutil"
)

// CheckCAInstalled 检查 CN 对应的根证书是否已装入 Windows LocalMachine\Root。
func CheckCAInstalled(cn string) (bool, error) {
	cmd := "Get-ChildItem Cert:\\LocalMachine\\Root | ForEach-Object { $_.Subject }"
	ps := exec.Command("powershell.exe", "-NoProfile", "-Command", cmd)
	out, err := ps.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("读取证书存储失败: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "CN="+cn) {
			return true, nil
		}
	}
	return false, nil
}

// InstallCA 将 DER 格式的根证书安装到 LocalMachine\Root。
// 直接调用 certutil,失败(通常因权限不足)时通过 UAC 提权重试。
func InstallCA(der []byte) error {
	tmp, err := os.CreateTemp("", "wx-mp-sync-*.cer")
	if err != nil {
		return fmt.Errorf("创建临时证书文件失败: %v", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(der); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时证书文件失败: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时证书文件失败: %v", err)
	}

	certutil, err := exec.LookPath("certutil")
	if err != nil {
		return fmt.Errorf("找不到 certutil: %v", err)
	}

	cmd := exec.Command(certutil, "-addstore", "Root", tmpPath)
	if out, err := cmd.CombinedOutput(); err == nil {
		return nil
	} else {
		_ = out
	}

	// 权限不足则提权重试(会弹出 UAC 授权窗口)
	if _, err := winutil.RunElevated(certutil, "-addstore", "Root", tmpPath); err != nil {
		return fmt.Errorf("安装证书失败: %v(若弹出授权窗口请点击“是”,或右键“以管理员身份运行”本程序)", err)
	}
	return nil
}

// UninstallCA 按 CN 卸载本地根证书(需要管理员权限)。
func UninstallCA(cn string) error {
	script := fmt.Sprintf(
		"Get-ChildItem Cert:\\LocalMachine\\Root | Where-Object { $_.Subject -like '*CN=%s*' } | ForEach-Object { Remove-Item -Path $_.PSPath -Force }; exit 0\n",
		cn,
	)
	tmp, err := os.CreateTemp("", "wx-mp-sync-uninstall-*.ps1")
	if err != nil {
		return fmt.Errorf("创建临时脚本失败: %v", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(script); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时脚本失败: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时脚本失败: %v", err)
	}

	if _, err := winutil.RunElevatedScript(tmpPath); err != nil {
		return fmt.Errorf("卸载证书失败(需要管理员权限): %v", err)
	}
	return nil
}
