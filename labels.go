package main

// Chinese labels, explanations and recommended values for workflow parameters.
// Generic names come from the input name / node class; workflow-specific roles (e.g. the
// MiniMax H3 template's step switch and Lightning LoRA toggle) are recognised from the graph
// wiring, so they work whatever the nodes happen to be titled.

import "strings"

type inputInfo struct{ label, hint string }

var inputCN = map[string]inputInfo{
	"text":             {"提示词", ""},
	"prompt":           {"提示词", ""},
	"positive":         {"正向提示词", ""},
	"negative":         {"反向提示词", "不想出现的内容"},
	"seed":             {"随机种子", "同一种子 + 同样参数 ≈ 同样结果"},
	"noise_seed":       {"随机种子", "同一种子 + 同样参数 ≈ 同样结果"},
	"steps":            {"采样步数", "越多越细致也越慢"},
	"cfg":              {"引导强度 CFG", "越高越贴合提示词，过高会生硬"},
	"denoise":          {"重绘强度", "文生视频保持 1"},
	"width":            {"宽度", "像素"},
	"height":           {"高度", "像素"},
	"megapixels":       {"总像素（百万）", "决定分辨率：0.98 ≈ 1344×768（官方 768p）"},
	"aspect_ratio":     {"画面比例", ""},
	"multiple":         {"尺寸倍数", "宽高取它的整数倍"},
	"fps":              {"帧率", "每秒帧数"},
	"frame_rate":       {"帧率", "每秒帧数"},
	"length":           {"帧数", ""},
	"num_frames":       {"帧数", ""},
	"batch_size":       {"批量", "一次出几张"},
	"sampler_name":     {"采样器", ""},
	"scheduler":        {"调度器", ""},
	"filename_prefix":  {"文件名前缀", ""},
	"format":           {"输出格式", ""},
	"codec":            {"视频编码", ""},
	"format.codec":     {"视频编码", ""},
	"image":            {"图片", ""},
	"audio":            {"音频", ""},
	"video":            {"视频", ""},
	"strength_model":   {"LoRA 强度", ""},
	"strength_clip":    {"LoRA 文本强度", ""},
	"lora_name":        {"LoRA 文件", ""},
	"unet_name":        {"主模型文件", ""},
	"ckpt_name":        {"模型文件", ""},
	"clip_name":        {"文本编码器文件", ""},
	"vae_name":         {"VAE 文件", ""},
	"weight_dtype":     {"权重精度", ""},
	"crf":              {"画质 CRF", "越小越清晰、文件越大"},
	"upscale_method":   {"放大算法", ""},
	"resolution":       {"分辨率", ""},
	"max_resolution":   {"最长边上限", ""},
	"color_correction": {"色彩校正", ""},
}

var classCN = map[string]string{
	"MiniMaxH3ImageToVideo": "MiniMax H3 视频", "ResolutionSelector": "分辨率选择器", "RandomNoise": "随机噪波",
	"BasicScheduler": "调度器", "CreateVideo": "合成视频", "SaveVideo": "保存视频", "KSamplerSelect": "采样器",
	"LoraLoaderModelOnly": "LoRA 加载", "UNETLoader": "主模型加载", "CLIPLoader": "文本编码器加载", "VAELoader": "VAE 加载",
	"LoadImage": "加载图片", "LoadAudio": "加载音频", "LoadVideo": "加载视频", "CLIPTextEncode": "文本编码", "KSampler": "K 采样器",
	"PrimitiveInt": "整数", "PrimitiveFloat": "小数", "PrimitiveBoolean": "开关", "PrimitiveString": "文本", "ComfySwitchNode": "条件切换",
	"SeedVR2VideoUpscaler": "SeedVR2 放大", "EmptyLatentImage": "空白画布", "SaveImage": "保存图片",
}

// role describes what a literal input really controls, derived from the graph wiring.
type role struct {
	label, hint string
	rec         any
	primary     bool
}

type consumer struct{ node, input string }

// fieldRoles returns roles keyed by "<node>.<input>" for parameters whose meaning only shows in
// the wiring (Primitive nodes feeding switches, schedulers, math expressions, …).
func fieldRoles(g map[string]map[string]any) map[string]role {
	cons := map[string][]consumer{} // node id -> who reads its output
	for id, n := range g {
		in, _ := n["inputs"].(map[string]any)
		for k, v := range in {
			if isLink(v) {
				src := v.([]any)[0].(string)
				cons[src] = append(cons[src], consumer{id, k})
			}
		}
	}
	cls := func(id string) string { s, _ := g[id]["class_type"].(string); return s }
	// what does node id ultimately feed (following switches)?
	var feeds func(id string, depth int) []consumer
	feeds = func(id string, depth int) []consumer {
		var out []consumer
		for _, c := range cons[id] {
			if cls(c.node) == "ComfySwitchNode" && (c.input == "on_true" || c.input == "on_false") && depth < 3 {
				out = append(out, feeds(c.node, depth+1)...)
			} else {
				out = append(out, c)
			}
		}
		return out
	}
	roles := map[string]role{}
	hasLightningLora := false
	for id := range g {
		if cls(id) == "LoraLoaderModelOnly" {
			if s, _ := g[id]["inputs"].(map[string]any)["lora_name"].(string); strings.Contains(strings.ToLower(s), "turbo") || strings.Contains(strings.ToLower(s), "lightning") || strings.Contains(strings.ToLower(s), "step") {
				hasLightningLora = true
			}
		}
	}
	for id, n := range g {
		c := cls(id)
		if !strings.HasPrefix(c, "Primitive") {
			continue
		}
		key := id + ".value"
		for _, f := range cons[id] {
			// int feeding a step switch: which branch?
			if cls(f.node) == "ComfySwitchNode" && (f.input == "on_true" || f.input == "on_false") {
				for _, d := range feeds(f.node, 1) {
					if d.input == "steps" {
						if f.input == "on_true" {
							roles[key] = role{"步数（加速模式）", "打开「Lightning 加速」时用。加速 LoRA 按 8 步训练，推荐 8", 8, true}
						} else {
							roles[key] = role{"步数（普通模式）", "不加速时用。官方推荐 20；少于 20 容易糊", 20, true}
						}
					}
				}
			}
			if cls(f.node) == "ComfySwitchNode" && f.input == "switch" && c == "PrimitiveBoolean" {
				lbl, hint := "条件开关", ""
				if hasLightningLora {
					lbl, hint = "Lightning 加速（8 步 LoRA）", "开：约快 2.5 倍，画质略降但比「不加速少步数」好；关：官方 20 步，最细致但最慢"
				}
				var rec any
				if hasLightningLora {
					rec = true // CU is precious on Colab: the 8-step LoRA is the best speed/quality trade-off
				}
				roles[key] = role{lbl, hint, rec, true}
			}
			if strings.HasPrefix(cls(f.node), "ComfyMathExpression") && c == "PrimitiveFloat" {
				for _, d := range cons[f.node] {
					if d.input == "length" || d.input == "num_frames" {
						roles[key] = role{"视频时长（秒）", "H3 支持 4–15 秒；越长越慢、越耗 CU", 6.0, true}
					}
				}
			}
			if f.input == "steps" && c == "PrimitiveInt" {
				roles[key] = role{"采样步数", inputCN["steps"].hint, nil, true}
			}
		}
		_ = n
	}
	// recommended values for well-known direct inputs
	for id, n := range g {
		in, _ := n["inputs"].(map[string]any)
		switch cls(id) {
		case "ResolutionSelector":
			if _, ok := in["megapixels"].(float64); ok {
				roles[id+".megapixels"] = role{"总像素（百万）", "0.98 ≈ 1344×768（官方 768p，推荐）；0.5 ≈ 960×544 更快", 0.98, true}
			}
		case "BasicScheduler":
			if _, ok := in["denoise"].(float64); ok {
				roles[id+".denoise"] = role{"重绘强度", "文生视频保持 1", 1.0, false}
			}
		case "CreateVideo":
			if _, ok := in["fps"].(float64); ok {
				roles[id+".fps"] = role{"帧率", "H3 按 24 帧训练，推荐 24", 24.0, true}
			}
		}
	}
	return roles
}

func nodeNameCN(n map[string]any) string {
	t := nodeTitle(n)
	cls, _ := n["class_type"].(string)
	if t == cls || t == "" {
		if cn, ok := classCN[cls]; ok {
			return cn
		}
	}
	return t
}
