package main

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// proxyFor decides how to reach the gate: the "proxy" setting wins ("direct" = no proxy,
// otherwise a URL like http://127.0.0.1:7890); empty = auto: HTTPS_PROXY/HTTP_PROXY env,
// then the Windows system proxy (what browsers use, e.g. Clash / v2rayN "system proxy").
func proxyFor(req *http.Request) (*url.URL, error) {
	if isLocalHost(req.URL.Hostname()) { // local models (Ollama / LM Studio) and LAN hosts never go through a proxy
		return nil, nil
	}
	p := strings.TrimSpace(getConfig().Proxy)
	switch strings.ToLower(p) {
	case "direct", "none", "off", "直连":
		return nil, nil
	case "", "auto", "system":
		if u, _ := http.ProxyFromEnvironment(req); u != nil {
			return u, nil
		}
		if s := systemProxy(); s != "" {
			return parseProxy(s), nil
		}
		return nil, nil
	}
	return parseProxy(p), nil
}

func parseProxy(s string) *url.URL {
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil
	}
	return u
}

// proxyLabel describes the route for error messages.
func proxyLabel() string {
	req, _ := http.NewRequest("GET", baseURL()+"/", nil)
	if req == nil {
		return ""
	}
	u, _ := proxyFor(req)
	if u == nil {
		return "直连"
	}
	return "经代理 " + u.Host
}

var transport = &http.Transport{
	Proxy:                 proxyFor,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          20,
	IdleConnTimeout:       60 * time.Second,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 60 * time.Second,
}

func init() {
	httpc.Transport = transport
	_ = os.Getenv
}

func isLocalHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".local") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}
