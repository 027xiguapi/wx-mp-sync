//go:build !windows

package proxy

import "errors"

// CheckCAInstalled 非 Windows 平台不支持。
func CheckCAInstalled(cn string) (bool, error) {
	return false, errors.New("证书安装仅支持 Windows")
}

// InstallCA 非 Windows 平台不支持。
func InstallCA(der []byte) error {
	return errors.New("证书安装仅支持 Windows")
}

// UninstallCA 非 Windows 平台不支持。
func UninstallCA(cn string) error {
	return errors.New("证书卸载仅支持 Windows")
}
