package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllocationWaitsBeyondSharedHeaderTimeout(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", h2), func(t *testing.T) {
			withTempConfig(t, Config{Mode: "local", Proxy: "direct"})
			gMu.Lock()
			gTok = &gToken{AccessToken: "fixture-token", RefreshToken: "fixture-refresh", Expiry: time.Now().Add(time.Hour)}
			gMu.Unlock()
			var posts atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantProtocol := map[bool]int{false: 1, true: 2}[h2]
				if r.URL.Path == "/tun/m/assign" {
					wantProtocol = 1
				}
				if r.ProtoMajor != wantProtocol {
					t.Errorf("unexpected protocol %s", r.Proto)
				}
				if r.Method == "GET" && r.URL.Path == "/tun/m/assign" {
					fmt.Fprint(w, `{"token":"xsrf"}`)
					return
				}
				if r.Method == "POST" {
					posts.Add(1)
				}
				select {
				case <-r.Context().Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				fmt.Fprint(w, `{"endpoint":"test","runtimeProxyInfo":{"url":"https://runtime.invalid"}}`)
			}))
			srv.EnableHTTP2 = h2
			srv.StartTLS()
			defer srv.Close()
			oldDomain, oldClient := colabDomain, colabHTTP
			colabDomain, colabHTTP = srv.URL, srv.Client()
			t.Cleanup(func() { colabDomain, colabHTTP = oldDomain, oldClient })
			tr := colabHTTP.Transport.(*http.Transport)
			tr.ResponseHeaderTimeout = 20 * time.Millisecond
			// Show that the ordinary client reproduces the reported header timeout.
			if err := colabDo(context.Background(), "GET", srv.URL+"/slow", nil, nil); err == nil || !strings.Contains(err.Error(), "timeout awaiting response headers") {
				t.Fatalf("expected shared header timeout, got %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			a, err := colabAssign(ctx, "G4")
			if err != nil || a == nil || a.Endpoint != "test" {
				t.Fatalf("allocation failed before its deadline: %v %v", a, err)
			}
			if posts.Load() != 1 || tr.ResponseHeaderTimeout != 20*time.Millisecond {
				t.Fatal("allocation retried its POST or changed the shared timeout")
			}
		})
	}
}

func TestAllocationRecoversOnlySameNotebookAfterLostResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       bool
	}{
		{"allocated", `{"endpoint":"mine","runtimeProxyInfo":{"url":"https://runtime.invalid","token":"runtime-token"}}`, 200, true},
		{"not confirmed", `{"token":"new-xsrf"}`, 200, false},
		{"incomplete", `{"endpoint":"mine"}`, 200, false},
		{"query failed", `{}`, 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts, gets atomic.Int32
			var query string
			mockColab(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/tun/m/assign" {
					t.Errorf("must not inspect unrelated runtimes: %s", r.URL.Path)
				}
				if r.Method == "GET" && gets.Add(1) == 1 {
					query = r.URL.RawQuery
					fmt.Fprint(w, `{"token":"xsrf"}`)
					return
				}
				if query != r.URL.RawQuery || r.URL.Query().Get("nbh") == "" {
					t.Error("notebook or accelerator changed")
				}
				if r.Method == "POST" {
					posts.Add(1)
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					// Simulate a truncated response after the server may have committed.
					fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{")
					conn.Close()
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			a, err := colabAssign(context.Background(), "G4")
			if tc.want {
				if err != nil || a == nil || a.Endpoint != "mine" || a.Accelerator != "G4" || a.Proxy.Token != "runtime-token" {
					t.Fatalf("recovery failed: %v %v", a, err)
				}
			} else if a != nil || err == nil || !strings.Contains(err.Error(), "暂未确认分配结果") {
				t.Fatalf("uncertain outcome hidden: %v %v", a, err)
			}
			if posts.Load() != 1 || gets.Load() != 2 {
				t.Fatalf("unexpected calls: POST=%d GET=%d", posts.Load(), gets.Load())
			}
		})
	}
}

func TestAllocationHTTP1PreservesConnectProxy(t *testing.T) {
	withTempConfig(t, Config{Mode: "local", Proxy: "direct"})
	gMu.Lock()
	gTok = &gToken{AccessToken: "fixture-token", Expiry: time.Now().Add(time.Hour)}
	gMu.Unlock()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("allocation used %s", r.Proto)
		}
		if r.Method == "GET" {
			fmt.Fprint(w, `{"token":"xsrf"}`)
			return
		}
		fmt.Fprint(w, `{"endpoint":"proxied","runtimeProxyInfo":{"url":"https://runtime.invalid"}}`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != srv.Listener.Addr().String() {
			t.Error("unexpected proxy destination")
			http.Error(w, "invalid", 400)
			return
		}
		upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			t.Error(err)
			http.Error(w, "dial", 502)
			return
		}
		defer upstream.Close()
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		connects.Add(1)
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { io.Copy(upstream, rw); upstream.Close() }()
		io.Copy(conn, upstream)
	}))
	defer proxy.Close()
	oldDomain, oldClient := colabDomain, colabHTTP
	colabDomain, colabHTTP = srv.URL, srv.Client()
	t.Cleanup(func() { colabDomain, colabHTTP = oldDomain, oldClient })
	proxyURL, _ := url.Parse(proxy.URL)
	tr := colabHTTP.Transport.(*http.Transport)
	tr.Proxy = http.ProxyURL(proxyURL)
	a, err := colabAssign(context.Background(), "L4")
	if err != nil || a == nil || a.Endpoint != "proxied" || connects.Load() < 1 {
		t.Fatalf("proxy compatibility failed: %v %v connects=%d", a, err, connects.Load())
	}
}

func TestAllocationDoesNotRetryUncertainTransportFailure(t *testing.T) {
	var posts atomic.Int32
	mockColab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"token":"xsrf"}`)
			return
		}
		posts.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close() // The server may have allocated; replay is not safe.
	})
	if _, err := colabAssign(context.Background(), "G4"); err == nil || posts.Load() != 1 {
		t.Fatalf("uncertain request retried: count=%d error=%v", posts.Load(), err)
	}
}

func TestAllocationPostHonorsTotalDeadline(t *testing.T) {
	var posts atomic.Int32
	mockColab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"token":"xsrf"}`)
			return
		}
		posts.Add(1)
		<-r.Context().Done()
	})
	old := colabAssignTimeout
	colabAssignTimeout = 80 * time.Millisecond
	t.Cleanup(func() { colabAssignTimeout = old })
	start := time.Now()
	_, err := colabAssign(context.Background(), "G4")
	if err == nil || !strings.Contains(err.Error(), "申请 GPU 超时") || posts.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("total deadline not enforced: %v, POSTs=%d", err, posts.Load())
	}
}

func TestAllocationRecoveryHonorsOriginalDeadline(t *testing.T) {
	var gets, posts atomic.Int32
	mockColab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if gets.Add(1) == 1 {
				fmt.Fprint(w, `{"token":"xsrf"}`)
			} else {
				<-r.Context().Done()
			}
			return
		}
		posts.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	old := colabAssignTimeout
	colabAssignTimeout = 100 * time.Millisecond
	t.Cleanup(func() { colabAssignTimeout = old })
	start := time.Now()
	_, err := colabAssign(context.Background(), "G4")
	if err == nil || !strings.Contains(err.Error(), "申请 GPU 超时") || gets.Load() != 2 || posts.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("recovery exceeded total deadline: %v GET=%d POST=%d", err, gets.Load(), posts.Load())
	}
}

func TestAllocationRejectionDoesNotRunRecovery(t *testing.T) {
	var gets, posts atomic.Int32
	mockColab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			gets.Add(1)
			fmt.Fprint(w, `{"token":"xsrf"}`)
			return
		}
		posts.Add(1)
		http.Error(w, "quota", http.StatusPreconditionFailed)
	})
	_, err := colabAssign(context.Background(), "G4")
	if err == nil || !strings.Contains(err.Error(), "Colab 拒绝分配") || gets.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("definite rejection masked: %v GET=%d POST=%d", err, gets.Load(), posts.Load())
	}
}
