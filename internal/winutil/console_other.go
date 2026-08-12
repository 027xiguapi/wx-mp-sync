//go:build !windows

package winutil

// RegisterConsoleCloseHandler 非 Windows 平台为空操作。
func RegisterConsoleCloseHandler(fn func()) error {
	return nil
}
