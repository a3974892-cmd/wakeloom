package main

import (
	"archive/zip"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed gateway/enable-qwen-vision.py
var aiVisionInstaller []byte

//go:embed gateway/gate-ai.py
var aiVisionGatewayPatch []byte

//go:embed gateway/enable-vision-on-vps.py
var aiVisionVPSInstaller []byte

var aiVisionExecute = func(ctx context.Context, rt *Runtime, onLine func(string)) error {
	if e := rt.Upload(ctx, aiVisionInstaller, "/content/wb_enable_vision.py"); e != nil {
		return e
	}
	return rt.Exec(ctx, "import runpy\n_wb_vision=runpy.run_path('/content/wb_enable_vision.py',run_name='__main__')\ndel _wb_vision\n", onLine)
}

type aiVisionJobState struct {
	Running bool   `json:"running"`
	Done    bool   `json:"done"`
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

var aiVisionJob struct {
	sync.Mutex
	state aiVisionJobState
}

func aiVisionInstalling() bool {
	aiVisionJob.Lock()
	defer aiVisionJob.Unlock()
	return aiVisionJob.state.Running
}

func aiVisionStatus(w http.ResponseWriter, r *http.Request) {
	aiVisionJob.Lock()
	state := aiVisionJob.state
	aiVisionJob.Unlock()
	lg.mu.Lock()
	ready, supported := lg.state == "ready" && lg.rt != nil, lg.profile == "qwen38-27b-chat" || lg.profile == "qwen38-27b-vision"
	lg.mu.Unlock()
	jsonOut(w, 200, map[string]any{"job": state, "local": localMode(), "can_install": localMode() && ready && supported})
}

func aiVisionEnable(w http.ResponseWriter, r *http.Request) {
	if !localMode() {
		errOut(w, 409, errors.New("网关模式请先更新 VPS 看图组件；本机不能通过聊天接口安装服务器组件"))
		return
	}
	lg.mu.Lock()
	rt, profile, ready := lg.rt, lg.profile, lg.state == "ready"
	lg.mu.Unlock()
	if !ready || rt == nil {
		errOut(w, 409, errors.New("请先唤醒 Qwen3.8 27B 并等待就绪"))
		return
	}
	if profile != "qwen38-27b-chat" && profile != "qwen38-27b-vision" {
		errOut(w, 400, errors.New("此补装入口用于内置 Qwen3.8 27B；其他模型请使用匹配的视觉配置"))
		return
	}
	aiVisionJob.Lock()
	if aiVisionJob.state.Running {
		state := aiVisionJob.state
		aiVisionJob.Unlock()
		jsonOut(w, 202, state)
		return
	}
	aiVisionJob.state = aiVisionJobState{Running: true, Phase: "prepare", Message: "正在准备视觉组件"}
	state := aiVisionJob.state
	aiVisionJob.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()
		keepAliveDone := make(chan struct{})
		defer close(keepAliveDone)
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-keepAliveDone:
					return
				case <-ticker.C:
					lg.mu.Lock()
					if lg.rt == rt {
						lg.lastActivity = time.Now()
					}
					lg.mu.Unlock()
				}
			}
		}()
		lg.execMu.Lock()
		defer lg.execMu.Unlock()
		lg.mu.Lock()
		same := lg.rt == rt && lg.state == "ready"
		lg.lastActivity = time.Now()
		lg.mu.Unlock()
		var err error
		if !same {
			err = errors.New("GPU 会话已经变化，请在新会话就绪后重试")
		} else {
			confirmed := false
			err = aiVisionExecute(ctx, rt, func(line string) {
				lg.mu.Lock()
				lg.lastActivity = time.Now()
				lg.mu.Unlock()
				if !strings.HasPrefix(line, "WB_VISION ") {
					return
				}
				var p struct {
					Phase string `json:"phase"`
					Text  string `json:"text"`
				}
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "WB_VISION ")), &p) != nil {
					return
				}
				confirmed = confirmed || p.Phase == "ready"
				aiVisionJob.Lock()
				aiVisionJob.state.Phase, aiVisionJob.state.Message = p.Phase, p.Text
				aiVisionJob.Unlock()
			})
			if err == nil && !confirmed {
				err = errors.New("未收到视觉推理检查完成结果，请刷新模型状态后重试")
			}
		}
		aiVisionJob.Lock()
		aiVisionJob.state.Running = false
		aiVisionJob.state.Done = err == nil
		if err != nil {
			diagEvent("error", "local", "视觉组件安装："+err.Error())
			aiVisionJob.state.Error = err.Error()
			aiVisionJob.state.Message = "补装未完成，原文字模型仍保留"
		} else {
			aiVisionJob.state.Message = "视觉组件已启用，图片推理检查通过"
		}
		aiVisionJob.Unlock()
	}()
	jsonOut(w, 202, state)
}

func aiVisionDownload(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, file := range []struct {
		name string
		data []byte
	}{{"gate-ai.py", aiVisionGatewayPatch}, {"enable-qwen-vision.py", aiVisionInstaller}, {"enable-vision-on-vps.py", aiVisionVPSInstaller}, {"使用说明.txt", []byte("当前运行中的 Qwen3.8 27B 补装看图：将本包全部文件放在 VPS 的同一目录，运行 python3 enable-vision-on-vps.py /opt/comfy-colab-kit（替换为实际目录）。脚本通过网关已有 Colab CLI 连接当前 VM，不停止 GPU 或重建网关，保留原文字模型，完成真实图片推理检查后才切换。\n\n后续新会话也使用看图配置：等当前会话结束后，再按 gateway/README.md 应用 gate-ai.py 并重建网关容器；下次选择 Qwen3.8 对话与看图。\n\n本地模式无需 VPS：在新版程序 AI 页点击启用看图，下载并启用即可。真实部署结果以运行输出为准。\n")}} {
		f, e := z.Create(file.name)
		if e != nil {
			errOut(w, 500, e)
			return
		}
		if _, e = f.Write(file.data); e != nil {
			errOut(w, 500, e)
			return
		}
	}
	if e := z.Close(); e != nil {
		errOut(w, 500, e)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "Wakeloom-vision-update.zip"))
	w.Write(buf.Bytes())
}
