package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mockColab(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	withTempConfig(t, Config{Mode: "local", Proxy: "direct"})
	srv := httptest.NewServer(handler)
	oldDomain, oldClient := colabDomain, colabHTTP
	colabDomain, colabHTTP = srv.URL, srv.Client()
	gMu.Lock()
	gTok = &gToken{AccessToken: "test-token", RefreshToken: "test-refresh", Expiry: time.Now().Add(time.Hour)}
	gMu.Unlock()
	t.Cleanup(func() { colabDomain, colabHTTP = oldDomain, oldClient; srv.Close() })
}

func TestColabCCURequiresRealBalance(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       float64
		wantError  bool
	}{
		{"paid", `{"currentBalance":8.63,"consumptionRateHourly":0,"assignmentsCount":0}`, 200, 8.63, false},
		{"real zero", `{"currentBalance":0,"consumptionRateHourly":0,"assignmentsCount":0}`, 200, 0, false},
		{"missing", `{"consumptionRateHourly":0,"assignmentsCount":0}`, 200, 0, true},
		{"null balance", `{"currentBalance":null,"consumptionRateHourly":0,"assignmentsCount":0}`, 200, 0, true},
		{"null object", `null`, 200, 0, true},
		{"empty object", `{}`, 200, 0, true},
		{"empty body", ``, 200, 0, true},
		{"login page", `<html>Sign in</html>`, 200, 0, true},
		{"unauthorized", `{}`, 401, 0, true},
		{"truncated", `{"currentBalance":8.63`, 200, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockColab(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/tun/m/ccu-info" || r.Header.Get("Authorization") != "Bearer test-token" {
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, ")]}'\n"+tc.body)
			})
			c, err := colabCCU(context.Background())
			if (err != nil) != tc.wantError || c.Balance != tc.want {
				t.Fatalf("balance=%v err=%v", c.Balance, err)
			}
		})
	}
}

func TestCCUQueryUpdatesCacheAndClearsFailures(t *testing.T) {
	body := `{"currentBalance":8.63,"consumptionRateHourly":0,"assignmentsCount":0}`
	mockColab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tun/m/assignments" {
			io.WriteString(w, `{"assignments":[]}`)
		} else {
			io.WriteString(w, body)
		}
	})
	oldGate := lg
	lg = &LocalGate{state: "sleeping", ccu: &CCUInfo{Balance: 0}}
	t.Cleanup(func() { lg = oldGate })
	api := http.NewServeMux()
	localRoutes(api)
	query := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		api.ServeHTTP(w, httptest.NewRequest("POST", "/api/local/ccu", strings.NewReader(`{}`)))
		return w
	}
	if w := query(); w.Code != 200 || lg.ccu == nil || lg.ccu.Balance != 8.63 {
		t.Fatalf("manual query did not replace cached zero: %d %s", w.Code, w.Body.String())
	}
	body = `{}`
	if w := query(); w.Code != 502 || lg.ccu != nil || lg.ccuError == "" {
		t.Fatalf("failed query retained stale balance: %d %s", w.Code, w.Body.String())
	}
	cu := lg.status()["cu"].(map[string]any)
	if cu["balance"] != nil || cu["error"] == "" {
		t.Fatalf("failed query looks like zero: %v", cu)
	}
}

func TestCCUIgnoresResponseFromPreviousLogin(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	mockColab(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"currentBalance":0,"consumptionRateHourly":0,"assignmentsCount":0}`)
	})
	g := &LocalGate{state: "sleeping"}
	done := make(chan error, 1)
	go func() { _, err := g.queryCCU(context.Background()); done <- err }()
	<-entered
	g.resetCCU()
	close(release)
	if err := <-done; err == nil || g.ccu != nil {
		t.Fatal("old login response overwrote the new account's unknown balance")
	}
}

func TestCCULoginAndLogoutClearCache(t *testing.T) {
	withTempConfig(t, Config{Mode: "local"})
	fakeGoogleToken(t)
	oldGate := lg
	lg = &LocalGate{ccu: &CCUInfo{Balance: 0}, ccuAt: time.Now()}
	t.Cleanup(func() { lg = oldGate })
	googleAuthURL()
	if _, err := googleExchange(context.Background(), "4/good-code"); err != nil {
		t.Fatal(err)
	}
	if lg.ccu != nil || !lg.ccuAt.IsZero() {
		t.Fatal("login kept old cache")
	}
	lg.ccu = &CCUInfo{Balance: 9}
	googleSignOut()
	if lg.ccu != nil {
		t.Fatal("logout kept old cache")
	}
}

func TestColabAssignCancellationDuringRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockColab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"token":"xsrf"}`)
			return
		}
		w.WriteHeader(503)
		cancel()
	})
	start := time.Now()
	if _, err := colabAssign(ctx, "G4"); err == nil {
		t.Fatal("expected cancellation")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation waited for the retry delay")
	}
}

func TestCanceledBootPreservesStopState(t *testing.T) {
	mockColab(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("already canceled boot made a request")
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(fmt.Errorf("已手动停止"))
	g := &LocalGate{state: "sleeping", accel: "G4"}
	g.boot(ctx, &LocalProfile{}, nil, nil)
	if g.state != "sleeping" || !strings.Contains(strings.Join(g.logs, "\n"), "已手动停止") {
		t.Fatalf("cancellation became a startup failure: %s %v", g.state, g.logs)
	}
}

func TestG4BalancePreflight(t *testing.T) {
	for _, balance := range []float64{0, 8.63, 9.99, 10, 12} {
		t.Run(fmt.Sprint(balance), func(t *testing.T) {
			assignCalls := 0
			mockColab(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/tun/m/ccu-info" {
					fmt.Fprintf(w, `{"currentBalance":%v,"consumptionRateHourly":0,"assignmentsCount":0}`, balance)
					return
				}
				assignCalls++
				io.WriteString(w, `{"endpoint":"fake","runtimeProxyInfo":{"url":"http://fake.invalid"}}`)
			})
			g := &LocalGate{state: "allocating", accel: "G4"}
			a, err := g.allocateGPU(context.Background(), "G4")
			if balance < 10 {
				if err == nil || !strings.Contains(err.Error(), "尚未申请 GPU") || assignCalls != 0 || a != nil {
					t.Fatalf("low balance reached allocation: calls=%d err=%v", assignCalls, err)
				}
			} else if err != nil || a == nil || assignCalls != 1 {
				t.Fatalf("sufficient balance rejected: calls=%d err=%v", assignCalls, err)
			}
		})
	}
}

func TestG4FailedBalanceCheckDoesNotAllocate(t *testing.T) {
	mockColab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tun/m/ccu-info" {
			t.Error("allocated without a known balance")
		}
		io.WriteString(w, `{}`)
	})
	g := &LocalGate{state: "allocating", accel: "G4"}
	g.boot(context.Background(), &LocalProfile{}, nil, nil)
	if g.state != "failed" || !strings.Contains(g.reason, "无法核对") {
		t.Fatalf("invalid balance left boot pending: %s %s", g.state, g.reason)
	}
}

func TestColabAllocationTimeoutLeavesFailedState(t *testing.T) {
	mockColab(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	old := colabAssignTimeout
	colabAssignTimeout = 30 * time.Millisecond
	t.Cleanup(func() { colabAssignTimeout = old })
	g := &LocalGate{state: "allocating", accel: "L4"}
	g.boot(context.Background(), &LocalProfile{}, nil, nil)
	if g.state != "failed" || !strings.Contains(g.reason, "申请 GPU 超时") {
		t.Fatalf("allocation stuck after timeout: %s %s", g.state, g.reason)
	}
}
