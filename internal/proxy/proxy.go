// Package proxy 基于 ltaoo/echo 实现本地 MITM 代理,并向匹配域名注入同步按钮。
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ltaoo/echo"
)

// Proxy 封装 ltaoo/echo MITM 代理,挂载到一个本地 HTTP 服务上。
type Proxy struct {
	rootCA  *RootCA
	echo    *echo.Echo
	handler http.Handler
	server  *http.Server
	port    int
}

// NewProxy 创建代理实例。adminBase 是管理服务地址,用于状态页跳转。
// upstream 是可选的上级代理(如 Clash),用于访问直连不通的站点。
func NewProxy(rootCA *RootCA, adminBase, upstream string) (*Proxy, error) {
	e, err := echo.NewEchoWithOptions(rootCA.Cert, rootCA.PrivateKey, &echo.Options{
		EnableBuiltinBypass:  false,
		InterceptOnlyMatched: true,
		UpstreamProxy:        upstream,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 MITM 代理失败: %w", err)
	}
	return &Proxy{
		rootCA:  rootCA,
		echo:    e,
		handler: &serverHandler{rootCA: rootCA, proxy: e, adminBase: adminBase},
	}, nil
}

// AddPlugin 添加 echo 拦截插件(如注入插件)。
func (p *Proxy) AddPlugin(plugin *echo.Plugin) {
	p.echo.AddPlugin(plugin)
}

// Start 在指定端口启动代理服务。
// 绑定 127.0.0.1 而不是 0.0.0.0:Windows 上通配绑定会与其它进程的
// 127.0.0.1:同端口监听共存,导致本机连接被更具体的监听抢走。
// 端口被占用时自动向后尝试,避免因端口冲突静默失效。
func (p *Proxy) Start(port int) error {
	const maxTries = 20
	var lastErr error
	for i := 0; i < maxTries; i++ {
		candidate := port + i
		if candidate > 65535 {
			break
		}
		l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", candidate))
		if err == nil {
			p.port = candidate
			p.server = &http.Server{Handler: p.handler}
			go func() {
				if err := p.server.Serve(l); err != nil && err != http.ErrServerClosed {
					fmt.Printf("代理服务错误: %v\n", err)
				}
			}()
			if candidate != port {
				fmt.Printf("⚠️ 代理端口 %d 已被占用,已自动改用 %d(系统代理也会指向该端口)\n", port, candidate)
			}
			return nil
		}
		lastErr = err
		if !isPortInUse(err) {
			return fmt.Errorf("代理端口 %d 监听失败: %w", candidate, err)
		}
	}
	return fmt.Errorf("代理端口 %d-%d 均被占用,请用 --proxy-port 指定其他端口: %w", port, port+maxTries-1, lastErr)
}

// isPortInUse 判断监听错误是否由端口占用引起。
func isPortInUse(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address")
}

// Port 返回代理实际监听端口(可能因端口占用与传入值不同)。
func (p *Proxy) Port() int {
	return p.port
}

// Addr 返回代理地址(host:port)。
func (p *Proxy) Addr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port))
}

// Stop 优雅关闭代理服务。
func (p *Proxy) Stop() error {
	if p.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.server.Shutdown(ctx)
	p.server = nil
	return err
}

// serverHandler 先处理本地路径(证书下载、状态页),其余转发给 echo 代理。
type serverHandler struct {
	rootCA    *RootCA
	proxy     http.Handler
	adminBase string
}

func (h *serverHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if hname, _, err := net.SplitHostPort(r.Host); err == nil {
		host = hname
	}
	isLocal := host == "localhost" || host == "127.0.0.1" || host == "::1"

	if isLocal && r.URL.Path == "/cert" {
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="wx-mp-sync.cer"`)
		der, err := h.rootCA.CertDER()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(der)
		return
	}
	if isLocal && (r.URL.Path == "/" || r.URL.Path == "") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		adminLink := ""
		if h.adminBase != "" {
			adminLink = `<p>管理页面: <a href="` + h.adminBase + `" target="_blank">` + h.adminBase + `</a></p>`
		}
		fmt.Fprintf(w, `<html><head><meta charset="utf-8"><title>wx-mp-sync</title></head><body><h1>代理服务运行中</h1><p><a href="/cert">点击下载证书</a>(已自动安装则无需)</p>%s</body></html>`, adminLink)
		return
	}

	h.proxy.ServeHTTP(w, r)
}
