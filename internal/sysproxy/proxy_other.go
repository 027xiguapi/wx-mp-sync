//go:build !windows

package sysproxy

import "errors"

// Settings 描述一组系统代理设置。
type Settings struct {
	Hostname string
	Port     string
}

// Enabled 判断设置是否启用了代理。
func (s *Settings) Enabled() bool {
	return s != nil && s.Hostname != "" && s.Port != ""
}

// Enable 非 Windows 平台不支持。
func Enable(hostname, port string) error {
	return errors.New("系统代理设置仅支持 Windows")
}

// Disable 非 Windows 平台不支持。
func Disable() error {
	return errors.New("系统代理设置仅支持 Windows")
}

// Fetch 非 Windows 平台不支持。
func Fetch() (*Settings, error) {
	return nil, errors.New("系统代理设置仅支持 Windows")
}

// Restore 非 Windows 平台不支持。
func Restore(saved *Settings) error {
	return errors.New("系统代理设置仅支持 Windows")
}
