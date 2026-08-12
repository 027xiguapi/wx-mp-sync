//go:build windows

package winutil

import (
	"os/exec"
	"strings"
)

// PsEscape 将字符串用单引号包裹,内嵌单引号翻倍,用于拼接到 PowerShell 命令行。
func PsEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// RunElevated 以提权方式(UAC)运行外部程序,等待其结束,返回输出。
// 通过 PowerShell Start-Process -Verb RunAs 实现;所有参数合并为一个
// -ArgumentList 数组,避免重复参数名导致传参异常。
func RunElevated(filePath string, args ...string) (string, error) {
	psCmd := "Start-Process -Verb RunAs -Wait -FilePath " + PsEscape(filePath)
	if len(args) > 0 {
		psCmd += " -ArgumentList " + psJoinArgs(args)
	}
	psExec := exec.Command("powershell", "-NoProfile", "-Command", psCmd)
	out, err := psExec.CombinedOutput()
	return string(out), err
}

// psJoinArgs 把参数列表拼接为 PowerShell 数组字面量:'a','b','c'。
func psJoinArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, PsEscape(a))
	}
	return strings.Join(quoted, ",")
}

// RunElevatedScript 以提权方式运行一个 PowerShell 脚本文件。
func RunElevatedScript(scriptPath string) (string, error) {
	psCmd := "Start-Process -Verb RunAs -Wait -FilePath 'powershell' -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File'," + PsEscape(scriptPath)
	psExec := exec.Command("powershell", "-NoProfile", "-Command", psCmd)
	out, err := psExec.CombinedOutput()
	return string(out), err
}
