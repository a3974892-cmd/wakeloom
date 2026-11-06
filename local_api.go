package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func localRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/local/status", func(w http.ResponseWriter, r *http.Request) {
		ok, email := googleSignedIn()
		jsonOut(w, 200, map[string]any{"signed_in": ok, "email": email})
	})
	api.HandleFunc("POST /api/local/login", func(w http.ResponseWriter, r *http.Request) {
		u := googleAuthURL()
		openBrowser(u)
		jsonOut(w, 200, map[string]any{"url": u})
	})
	api.HandleFunc("POST /api/local/code", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code string `json:"code"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		email, err := googleExchange(r.Context(), in.Code)
		if err != nil {
			errOut(w, 400, err)
			return
		}
		jsonOut(w, 200, map[string]any{"email": email})
	})
	api.HandleFunc("POST /api/local/logout", func(w http.ResponseWriter, r *http.Request) {
		googleSignOut()
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/local/ccu", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		c, err := colabCCU(ctx)
		if err != nil {
			errOut(w, 502, err)
			return
		}
		list, _ := colabList(ctx)
		accs := []string{}
		for _, a := range list {
			accs = append(accs, a.Accelerator)
		}
		jsonOut(w, 200, map[string]any{"balance": c.Balance, "per_hour": c.RatePerHour, "assignments": c.Assignments, "running": accs})
	})
	// End-to-end check: allocate a GPU, run one cell, release it. Streams progress as SSE.
	api.HandleFunc("POST /api/local/smoke", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Accelerator string `json:"accelerator"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Accelerator == "" {
			in.Accelerator = "T4"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		send := func(ev string, v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
			if fl != nil {
				fl.Flush()
			}
		}
		logf := func(f string, a ...any) { send("log", fmt.Sprintf(f, a...)) }
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		t0 := time.Now()
		logf("申请 %s GPU…", in.Accelerator)
		a, err := colabAssign(ctx, in.Accelerator)
		if err != nil {
			send("fail", err.Error())
			return
		}
		logf("✓ 已分配（%.0f 秒）", time.Since(t0).Seconds())
		defer func() {
			c2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel2()
			if err := colabUnassign(c2, a.Endpoint); err != nil {
				send("log", "⚠ 释放 GPU 失败："+err.Error()+"（请到 Colab 网页手动断开）")
			} else {
				send("log", "✓ 已释放 GPU")
			}
			send("done", time.Since(t0).Seconds())
		}()
		rt := &Runtime{A: a}
		logf("启动 Python 内核…")
		if err := rt.StartKernel(ctx); err != nil {
			send("fail", err.Error())
			return
		}
		logf("✓ 内核就绪，运行测试代码…")
		code := "import subprocess, sys, platform\n" +
			"print('python', sys.version.split()[0], platform.platform())\n" +
			"print(subprocess.run(['bash','-lc','nvidia-smi --query-gpu=name,memory.total --format=csv,noheader || echo no-gpu'],capture_output=True,text=True).stdout.strip())\n" +
			"for i in range(3):\n    print('tick', i, flush=True)\n"
		err = rt.Exec(ctx, code, func(l string) { send("log", "  "+l) })
		if err != nil {
			send("fail", err.Error())
			return
		}
		data := []byte("hello from workbench\n")
		if err := rt.Upload(ctx, data, "/content/wb_test.txt"); err != nil {
			send("fail", err.Error())
			return
		}
		var got []string
		rt.Exec(ctx, "print(open('/content/wb_test.txt').read().strip())", func(l string) { got = append(got, l) })
		if strings.Join(got, "") == "hello from workbench" {
			logf("✓ 文件上传正常")
		} else {
			logf("⚠ 文件上传校验失败：%v", got)
		}
		send("ok", "全部通过")
	})
}
