package main

// Google sign-in for local mode. Uses the same public "installed app" OAuth client and the same
// copy-paste flow as Google's own google-colab-cli (Apache-2.0): the user approves in a browser,
// Google shows an authorization code on sdk.cloud.google.com, and the user pastes it back here.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	gClientID     = "764086051850-6qr4p6gpi6hn506pt8ejuq83di341hur.apps.googleusercontent.com"
	gClientSecret = "d-FL95Q19q7MQmFpd7hHD0Ty"
	gRedirect     = "https://sdk.cloud.google.com/applicationdefaultauthcode.html"
	gAuthURL      = "https://accounts.google.com/o/oauth2/auth"
)

// gTokenURL can be pointed at a fake server for UI tests (CKWB_GOOGLE_TOKEN_URL, like CKWB_HF_BASE).
var gTokenURL = firstNonEmpty(os.Getenv("CKWB_GOOGLE_TOKEN_URL"), "https://oauth2.googleapis.com/token")

var gScopes = []string{
	"openid",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/colaboratory",
	"https://www.googleapis.com/auth/drive.file",
}

type gToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	Email        string    `json:"email"`
}

var (
	gMu       sync.Mutex
	gTok      *gToken
	gVerifier string
)

func gTokenPath() string { return filepath.Join(dataDir, "google_token.json") }

func loadGoogleToken() {
	gMu.Lock()
	defer gMu.Unlock()
	b, err := os.ReadFile(gTokenPath())
	if err != nil {
		return
	}
	var t gToken
	if json.Unmarshal(b, &t) == nil && t.RefreshToken != "" {
		gTok = &t
	}
}

func googleSignedIn() (bool, string) {
	gMu.Lock()
	defer gMu.Unlock()
	if gTok == nil {
		return false, ""
	}
	return true, gTok.Email
}

func googleSignOut() {
	gMu.Lock()
	gTok = nil
	gMu.Unlock()
	lg.resetCCU()
	os.Remove(gTokenPath())
}

// googleAuthURL starts a sign-in (PKCE) and returns the consent URL.
func googleAuthURL() string {
	b := make([]byte, 48)
	rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(v))
	gMu.Lock()
	gVerifier = v
	gMu.Unlock()
	q := url.Values{
		"client_id":             {gClientID},
		"redirect_uri":          {gRedirect},
		"response_type":         {"code"},
		"scope":                 {strings.Join(gScopes, " ")},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"token_usage":           {"remote"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	return gAuthURL + "?" + q.Encode()
}

func tokenRequest(ctx context.Context, form url.Values) (*gToken, error) {
	form.Set("client_id", gClientID)
	form.Set("client_secret", gClientSecret)
	req, _ := http.NewRequestWithContext(ctx, "POST", gTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Transport: transport, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上 Google：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		IDToken      string `json:"id_token"`
		Error        string `json:"error"`
		Desc         string `json:"error_description"`
	}
	json.Unmarshal(raw, &r)
	if resp.StatusCode >= 400 || r.AccessToken == "" {
		msg := r.Desc
		if msg == "" {
			msg = r.Error
		}
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		return nil, errors.New("Google 拒绝了授权：" + msg)
	}
	t := &gToken{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, Expiry: time.Now().Add(time.Duration(r.ExpiresIn-60) * time.Second)}
	if r.IDToken != "" {
		if parts := strings.Split(r.IDToken, "."); len(parts) == 3 {
			if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
				var c struct {
					Email string `json:"email"`
				}
				json.Unmarshal(b, &c)
				t.Email = c.Email
			}
		}
	}
	return t, nil
}

// googleExchange turns the pasted authorization code into a stored refresh token.
func googleExchange(ctx context.Context, code string) (string, error) {
	code = strings.TrimSpace(code)
	if i := strings.Index(code, "code="); i >= 0 { // pasted a whole URL
		code = code[i+5:]
		if j := strings.IndexAny(code, "&#"); j >= 0 {
			code = code[:j]
		}
		code, _ = url.QueryUnescape(code)
	}
	gMu.Lock()
	v := gVerifier
	gMu.Unlock()
	if v == "" {
		return "", errors.New("请先点「登录 Google」打开授权页")
	}
	t, err := tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {gRedirect}, "code_verifier": {v}})
	if err != nil {
		return "", err
	}
	if t.RefreshToken == "" {
		return "", errors.New("Google 没有返回长期令牌，请重新登录一次")
	}
	gMu.Lock()
	gTok = t
	gMu.Unlock()
	lg.resetCCU()
	writeJSON(gTokenPath(), t)
	return t.Email, nil
}

// googleAccess returns a valid access token, refreshing it when needed.
func googleAccess(ctx context.Context) (string, error) {
	gMu.Lock()
	t := gTok
	gMu.Unlock()
	if t == nil {
		return "", errors.New("本地模式还没登录 Google（设置 → 本地模式 → 登录）")
	}
	if time.Now().Before(t.Expiry) && t.AccessToken != "" {
		return t.AccessToken, nil
	}
	nt, err := tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}})
	if err != nil {
		return "", err
	}
	nt.RefreshToken, nt.Email = t.RefreshToken, t.Email
	gMu.Lock()
	gTok = nt
	gMu.Unlock()
	writeJSON(gTokenPath(), nt)
	return nt.AccessToken, nil
}
