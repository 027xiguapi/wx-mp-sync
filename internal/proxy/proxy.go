// Package proxy 基于 ltaoo/echo 实现本地 MITM 代理,并向匹配域名注入同步按钮。
package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
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
func NewProxy(rootCA *RootCA, adminBase string) (*Proxy, error) {
	e, err := echo.NewEchoWithOptions(rootCA.Cert, rootCA.PrivateKey, &echo.Options{
		EnableBuiltinBypass:  false,
		InterceptOnlyMatched: true,
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
func (p *Proxy) Start(port int) error {
	p.port = port
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("代理端口 %d 被占用: %w", port, err)
	}
	p.server = &http.Server{Handler: p.handler}
	go func() {
		if err := p.server.Serve(l); err != nil && err != http.ErrServerClosed {
			fmt.Printf("代理服务错误: %v\n", err)
		}
	}()
	return nil
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
