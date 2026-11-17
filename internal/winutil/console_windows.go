//go:build windows

package winutil

import (
	"golang.org/x/sys/windows"
)

// 控制台控制事件类型(wincon.h)。
const (
	ctrlCEvent        = 0
	ctrlBreakEvent    = 1
	ctrlCloseEvent    = 2 // 用户点击控制台窗口的关闭按钮(X)
	ctrlLogoffEvent   = 5
	ctrlShutdownEvent = 6
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	consoleCloseHandlers      []func()
	consoleCtrlCallback       uintptr // 持有回调指针,防止被 GC
)

// RegisterConsoleCloseHandler 注册一个在控制台窗口被关闭时调用的回调,
// 覆盖点击窗口 ×(CTRL_CLOSE)、注销、关机事件。
// Ctrl+C 仍交给 Go 标准库信号处理,这里不拦截,避免冲突。
func RegisterConsoleCloseHandler(fn func()) error {
	if fn == nil {
		return nil
	}
	consoleCloseHandlers = append(consoleCloseHandlers, fn)
	if consoleCtrlCallback != 0 {
		return nil
	}

	handler := func(ctrlType uint32) uintptr {
		switch ctrlType {
		case ctrlCloseEvent, ctrlLogoffEvent, ctrlShutdownEvent:
			for _, h := range consoleCloseHandlers {
				go h()
			}
			return 1 // 已处理,允许进程继续执行清理
		}
		return 0 // 不处理,交给系统/Go 信号
	}

	consoleCtrlCallback = windows.NewCallback(handler)
	r1, _, err := procSetConsoleCtrlHandler.Call(consoleCtrlCallback, 1)
	if r1 == 0 {
		return err
	}
	return nil
}
