package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTempConfig gives a test its own data dir and config.
func withTempConfig(t *testing.T, c Config) {
	t.Helper()
	oldDir, oldCfg := dataDir, getConfig()
	dataDir = t.TempDir()
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	gMu.Lock()
	oldTok := gTok
	gTok = nil
	gMu.Unlock()
	t.Cleanup(func() {
		dataDir = oldDir
		cfgMu.Lock()
		cfg = oldCfg
		cfgMu.Unlock()
		gMu.Lock()
		gTok = oldTok
		gMu.Unlock()
	})
}

func TestNetCheck(t *testing.T) {
	withTempConfig(t, Config{Proxy: "direct"})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", 302) }))
	defer up.Close()
	old := netTargets
	t.Cleanup(func() { netTargets = old })
	netTargets = []netTarget{{"a.example", up.URL, "x"}, {"b.example", up.URL, "y"}}
	r := netCheck(context.Background())
	if r["ok"] != true || r["advice"] != "" || r["proxy"] != "直连" {
		t.Fatalf("all up: %v", r)
	}
	if getConfig().Checklist["network"] == 0 {
		t.Error("checklist not ticked")
	}
	netTargets = append(netTargets, netTarget{"down.example", "http://127.0.0.1:1/", "z"})
	r = netCheck(context.Background())
	items := r["items"].([]netResult)
	if r["ok"] != false || !strings.Contains(r["advice"].(string), "代理") || items[2].OK || items[2].Error == "" || !items[0].OK {
		t.Fatalf("one down: %+v", r)
	}
}

func fakeGoogleToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "4/good-code" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant","error_description":"Bad Request"}`))
			return
		}
		claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"me@example.com"}`))
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "refresh_token": "rt", "expires_in": 3600, "id_token": "x." + claims + ".y"})
	}))
	old := gTokenURL
	gTokenURL = srv.URL
	t.Cleanup(func() { gTokenURL = old; srv.Close() })
}

func TestLocalLoginMarksChecklist(t *testing.T) {
	withTempConfig(t, Config{Mode: "local"})
	fakeGoogleToken(t)
	googleAuthURL() // sets the PKCE verifier like "登录 Google" does
	api := http.NewServeMux()
	localRoutes(api)
	call := func(code string) (int, string) {
		w := httptest.NewRecorder()
		api.ServeHTTP(w, httptest.NewRequest("POST", "/api/local/code", strings.NewReader(`{"code":"`+code+`"}`)))
		return w.Code, w.Body.String()
	}
	if c, b := call("4/bad"); c != 400 || !strings.Contains(b, "Google 拒绝了授权") {
		t.Fatalf("bad code: %d %s", c, b)
	}
	if getConfig().Checklist["login"] != 0 {
		t.Error("failed login ticked the checklist")
	}
	// a whole redirect URL pasted from the browser works too
	if c, b := call("https://sdk.cloud.google.com/applicationdefaultauthcode.html?code=4%2Fgood-code&scope=x"); c != 200 || !strings.Contains(b, "me@example.com") {
		t.Fatalf("good code: %d %s", c, b)
	}
	if ok, email := googleSignedIn(); !ok || email != "me@example.com" || getConfig().Checklist["login"] == 0 {
		t.Error("not signed in / not ticked")
	}
	st := onboardingState()
	if st["signed_in"] != true || st["email"] != "me@example.com" {
		t.Error(st)
	}
}

func TestGatewayTestMarksChecklist(t *testing.T) {
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":"bad_agent_token"}`, 401)
			return
		}
		jsonOut(w, 200, map[string]any{"state": "sleeping"})
	}))
	defer gate.Close()
	withTempConfig(t, Config{GateURL: gate.URL, Token: "wrong"})
	srv := httptest.NewServer(newServer())
	defer srv.Close()
	post := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/gate/test", strings.NewReader("{}"))
		req.Header.Set("X-Key", apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if post() != 502 || getConfig().Checklist["login"] != 0 {
		t.Fatal("bad token accepted")
	}
	cfgMu.Lock()
	cfg.Token = "tok"
	cfgMu.Unlock()
	if post() != 200 || getConfig().Checklist["login"] == 0 {
		t.Fatal("gateway test did not tick login")
	}
}

func TestMigrateOnboardingAndConfigSave(t *testing.T) {
	// a brand-new install sees the guide
	withTempConfig(t, Config{})
	migrateOnboarding(false)
	if getConfig().Onboarded {
		t.Error("new user marked onboarded")
	}
	// an existing user with a gateway does not
	withTempConfig(t, Config{GateURL: "https://gate.example"})
	migrateOnboarding(true)
	c := getConfig()
	if !c.Onboarded || c.Checklist["wake"] != -1 {
		t.Errorf("old gateway user: %+v", c)
	}
	st := onboardingState()
	if st["all_done"] != true {
		t.Error("checklist shown to an existing user")
	}
	// existing local-mode user (signed in)
	withTempConfig(t, Config{Mode: "local"})
	gMu.Lock()
	gTok = &gToken{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour), Email: "x@y"}
	gMu.Unlock()
	migrateOnboarding(true)
	if !getConfig().Onboarded {
		t.Error("signed-in user not migrated")
	}
	// a config file with neither: guide shows
	withTempConfig(t, Config{OutDir: "/x"})
	migrateOnboarding(true)
	if getConfig().Onboarded {
		t.Error("unconfigured config marked onboarded")
	}

	// saving the settings form keeps the guide's state
	withTempConfig(t, Config{Onboarded: true, OnboardStep: "full:5", Checklist: map[string]int64{"wake": 5}, CURates: map[string]float64{"L4": 4.1}})
	srv := httptest.NewServer(newServer())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/config", strings.NewReader(`{"gate_url":"https://g","out_dir":"/o"}`))
	req.Header.Set("X-Key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err, resp.StatusCode)
	}
	c = getConfig()
	if !c.Onboarded || c.OnboardStep != "full:5" || c.Checklist["wake"] != 5 || c.CURates["L4"] != 4.1 || c.GateURL != "https://g" {
		t.Errorf("settings save lost guide state: %+v", c)
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err != nil || !strings.Contains(string(b), `"onboarded": true`) {
		t.Errorf("not persisted: %s %v", b, err)
	}
}

func TestCURateLearning(t *testing.T) {
	withTempConfig(t, Config{})
	if r, m := cuRate("L4"); r != cuRateGuess["L4"] || m {
		t.Error(r, m)
	}
	learnCURate(map[string]any{"state": "ready", "accelerator": "l4", "cu": map[string]any{"per_hour": 4.23}})
	learnCURate(map[string]any{"state": "allocating", "accelerator": "G4", "cu": map[string]any{"per_hour": 50.0}})
	if r, m := cuRate("L4"); r != 4.23 || !m {
		t.Error("not learned", r, m)
	}
	if _, m := cuRate("G4"); m {
		t.Error("learned from a GPU that was not ready")
	}
}

func TestOnboardingAPI(t *testing.T) {
	withTempConfig(t, Config{})
	srv := httptest.NewServer(newServer())
	defer srv.Close()
	post := func(body string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+"/api/onboarding", strings.NewReader(body))
		req.Header.Set("X-Key", apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	st := post(`{"step":"simple:2"}`)
	if st["step"] != "simple:2" || st["onboarded"] != false {
		t.Fatal(st)
	}
	post(`{"mark":"generate"}`)
	post(`{"mark":"bogus"}`)
	st = post(`{"onboarded":true,"checklist_hidden":true}`)
	items := st["checklist"].([]any)
	if st["onboarded"] != true || st["checklist_hidden"] != true || items[3].(map[string]any)["done"] != true || len(getConfig().Checklist) != 1 {
		t.Fatal(st, getConfig().Checklist)
	}
}
