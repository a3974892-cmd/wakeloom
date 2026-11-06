//go:build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

func systemProxy() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	if on, _, err := k.GetIntegerValue("ProxyEnable"); err != nil || on == 0 {
		return ""
	}
	s, _, err := k.GetStringValue("ProxyServer")
	if err != nil || s == "" {
		return ""
	}
	// "host:port" or "http=host:port;https=host:port;socks=..."
	if !strings.Contains(s, "=") {
		return s
	}
	var http, https string
	for _, part := range strings.Split(s, ";") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.ToLower(kv[0]) {
		case "https":
			https = kv[1]
		case "http":
			http = kv[1]
		}
	}
	if https != "" {
		return https
	}
	return http
}
