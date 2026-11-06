package main

// GPU / system load for the console panel. Both modes: ComfyUI's /system_stats (VRAM, RAM,
// versions) and /queue through the gate. Local mode adds nvidia-smi + psutil samples taken on the
// VM through the kernel (utilization, temperature, power, CPU, disk) while the panel is open.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type NVSample struct {
	At       float64  `json:"at"`
	GPUs     []NVGPU  `json:"gpus"`
	CPU      *float64 `json:"cpu"`
	RAMUsed  float64  `json:"ram_used"`
	RAMTotal float64  `json:"ram_total"`
	DiskUsed float64  `json:"disk_used"`
	DiskTot  float64  `json:"disk_total"`
}

type NVGPU struct {
	Name     string  `json:"name"`
	Util     float64 `json:"util"`
	MemUsed  float64 `json:"mem_used"`  // MiB
	MemTotal float64 `json:"mem_total"` // MiB
	Temp     float64 `json:"temp"`
	Power    float64 `json:"power"`
	PowerMax float64 `json:"power_max"`
}

const statsCode = `import subprocess, json, shutil, time, os
open('/content/kit_heartbeat','w').write(str(time.time()))
g=[]
try:
    out=subprocess.run(['nvidia-smi','--query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,power.limit','--format=csv,noheader,nounits'],capture_output=True,text=True,timeout=8).stdout
    for l in out.strip().splitlines():
        p=[x.strip() for x in l.split(',')]
        f=lambda s: float(s) if s.replace('.','',1).isdigit() else 0.0
        g.append({'name':p[0],'util':f(p[1]),'mem_used':f(p[2]),'mem_total':f(p[3]),'temp':f(p[4]),'power':f(p[5]),'power_max':f(p[6])})
except Exception:
    pass
d={'at':time.time(),'gpus':g,'cpu':None,'ram_used':0,'ram_total':0}
try:
    import psutil; d['cpu']=psutil.cpu_percent(interval=0.3); vm=psutil.virtual_memory(); d['ram_used']=vm.used; d['ram_total']=vm.total
except Exception:
    pass
try:
    du=shutil.disk_usage('/content'); d['disk_used']=du.used; d['disk_total']=du.total
except Exception:
    pass
print('WB_STATS '+json.dumps(d))
print(open('/content/kit_tunnel_url').read().strip() if os.path.exists('/content/kit_tunnel_url') else '')
`

// sampleStats runs statsCode on the VM (doubles as the heartbeat). Returns the tunnel URL line.
func (g *LocalGate) sampleStats(ctx context.Context, rt *Runtime) (string, error) {
	var tunnel string
	err := rt.Exec(ctx, statsCode, func(l string) {
		if strings.HasPrefix(l, "WB_STATS ") {
			var s NVSample
			if json.Unmarshal([]byte(strings.TrimPrefix(l, "WB_STATS ")), &s) == nil {
				g.mu.Lock()
				g.stats = &s
				g.mu.Unlock()
			}
		} else if validTunnel(l) {
			tunnel = l
		}
	})
	return tunnel, err
}

func gpuStatsHandler(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"mode": map[bool]string{true: "local", false: "gateway"}[localMode()], "at": float64(time.Now().UnixNano()) / 1e9}
	st, err := gateState()
	out["state"] = st
	if err != nil || st != "ready" {
		out["ready"] = false
		jsonOut(w, 200, out)
		return
	}
	out["ready"] = true
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	_ = ctx
	var ss map[string]any
	if gateJSON("GET", "/system_stats", nil, &ss) == nil {
		out["system_stats"] = ss
	}
	var q map[string]any
	if gateJSON("GET", "/queue", nil, &q) == nil {
		ql := func(k string) int { a, _ := q[k].([]any); return len(a) }
		out["queue"] = map[string]int{"running": ql("queue_running"), "pending": ql("queue_pending")}
	}
	if localMode() {
		lg.mu.Lock()
		lg.statsWanted = time.Now()
		if lg.stats != nil {
			out["nv"] = lg.stats
		}
		lg.mu.Unlock()
	}
	jsonOut(w, 200, out)
}
