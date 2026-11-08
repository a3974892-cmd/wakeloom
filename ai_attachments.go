package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

const aiUploadLimit = 32 << 20
const aiFileLimit = 10 << 20
const aiTextLimit = 16000

type aiAttachmentPart struct {
	Name    string `json:"name"`
	Text    string `json:"text,omitempty"`
	Image   string `json:"image,omitempty"` // JPEG base64; never a path supplied by the browser.
	TextID  string `json:"text_id,omitempty"`
	ImageID string `json:"image_id,omitempty"`
	Chars   int    `json:"chars,omitempty"`
}

type aiAttachment struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Files        int                `json:"files"`
	Images       int                `json:"images"`
	ImageParts   []int              `json:"image_parts,omitempty"`
	Chars        int                `json:"chars"`
	Skipped      []string           `json:"skipped,omitempty"`
	SkippedTotal int                `json:"skipped_total,omitempty"`
	Parts        []aiAttachmentPart `json:"parts,omitempty"`
}

func aiAttachmentPath(id string) (string, error) {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != 16 {
		return "", errors.New("附件编号无效")
	}
	return filepath.Join(dataDir, "ai-attachments", id+".json"), nil
}

func loadAIAttachment(id string) (*aiAttachment, error) {
	p, err := aiAttachmentPath(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, errors.New("附件已不存在，请重新添加")
	}
	defer f.Close()
	var a aiAttachment
	if err := json.NewDecoder(f).Decode(&a); err != nil {
		return nil, errors.New("附件无法读取，请重新添加")
	}
	return &a, nil
}

func aiSafeAttachmentName(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || len(name) > 512 || strings.ContainsAny(name, ":\x00\r\n") || strings.HasPrefix(name, "/") {
		return "", errors.New("文件名无效")
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == ".." || segment == "." || segment == "" {
			return "", errors.New("文件路径无效")
		}
	}
	return name, nil
}

func aiAttachmentSkip(name string) bool {
	for _, s := range strings.Split(name, "/") {
		if s == ".git" || s == "node_modules" || s == "__pycache__" || s == ".venv" || s == "__MACOSX" {
			return true
		}
	}
	return false
}

func aiTextExtension(ext string) bool {
	return strings.Contains("|.txt|.md|.markdown|.csv|.tsv|.json|.jsonl|.yaml|.yml|.xml|.html|.htm|.css|.js|.mjs|.cjs|.jsx|.ts|.tsx|.py|.go|.rs|.java|.c|.h|.cpp|.hpp|.cs|.sql|.sh|.ps1|.bat|.toml|.ini|.log|.srt|.vtt|.svg|", "|"+ext+"|")
}

func aiReadBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("文件解压后过大，请拆分后添加")
	}
	return b, nil
}

func aiDecodeText(b []byte) (string, error) {
	if len(b) >= 2 && ((b[0] == 255 && b[1] == 254) || (b[0] == 254 && b[1] == 255)) {
		if len(b)%2 != 0 {
			return "", errors.New("UTF-16 文本不完整")
		}
		u := make([]uint16, (len(b)-2)/2)
		for i := range u {
			if b[0] == 255 {
				u[i] = uint16(b[i*2+2]) | uint16(b[i*2+3])<<8
			} else {
				u[i] = uint16(b[i*2+2])<<8 | uint16(b[i*2+3])
			}
		}
		return string(utf16.Decode(u)), nil
	}
	b = bytes.TrimPrefix(b, []byte{239, 187, 191})
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return "", errors.New("无法识别文字编码，请另存为 UTF-8 文本")
	}
	return string(b), nil
}

func aiDOCXText(b []byte) (string, error) {
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", errors.New("Word 文件损坏")
	}
	for _, f := range z.File {
		if f.Name != "word/document.xml" {
			continue
		}
		r, err := f.Open()
		if err != nil {
			return "", err
		}
		raw, err := aiReadBounded(r, aiFileLimit)
		r.Close()
		if err != nil {
			return "", err
		}
		d := xml.NewDecoder(bytes.NewReader(raw))
		var out strings.Builder
		inText := false
		for {
			t, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", errors.New("Word 文档内容损坏")
			}
			switch t := t.(type) {
			case xml.StartElement:
				if t.Name.Local == "t" {
					inText = true
				}
				if t.Name.Local == "tab" {
					out.WriteString("\t")
				}
				if t.Name.Local == "br" {
					out.WriteString("\n")
				}
			case xml.EndElement:
				if t.Name.Local == "t" {
					inText = false
				}
				if t.Name.Local == "p" || t.Name.Local == "tr" {
					out.WriteString("\n")
				}
			case xml.CharData:
				if inText {
					out.Write(t)
				}
			}
			if out.Len() > aiTextLimit*4 {
				return "", errors.New("文档太长，请拆分后添加")
			}
		}
		return out.String(), nil
	}
	return "", errors.New("未找到 Word 正文；请使用 .docx 文件")
}

func aiPDFText(b []byte) (text string, err error) {
	defer func() {
		if recover() != nil {
			text = ""
			err = errors.New("PDF 无法解析，请另存为文本或 Word 后添加")
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", errors.New("PDF 无法读取，可能已损坏或需要密码")
	}
	if r.NumPage() > 100 {
		return "", errors.New("PDF 超过 100 页，请拆分后添加")
	}
	var out strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		s, e := r.Page(i).GetPlainText(nil)
		if e != nil {
			return "", fmt.Errorf("PDF 第 %d 页无法读取", i)
		}
		out.WriteString(s)
		out.WriteByte('\n')
		if out.Len() > aiTextLimit*4 {
			return "", errors.New("PDF 文字太长，请拆分后添加")
		}
	}
	if strings.TrimSpace(out.String()) == "" {
		return "", errors.New("PDF 未包含可提取的文字，扫描件请先做文字识别")
	}
	return out.String(), nil
}

func aiImageData(b []byte) (string, error) {
	return aiImageReader(bytes.NewReader(b))
}

func aiImageReader(reader io.ReadSeeker) (string, error) {
	c, _, err := image.DecodeConfig(reader)
	if err != nil || c.Width <= 0 || c.Height <= 0 || int64(c.Width)*int64(c.Height) > 24000000 {
		return "", errors.New("图片无法读取或超过 2400 万像素")
	}
	if _, err = reader.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	im, _, err := image.Decode(reader)
	if err != nil {
		return "", errors.New("图片内容损坏")
	}
	w, h := c.Width, c.Height
	if w > 1600 || h > 1600 {
		if w >= h {
			h = max(1, h*1600/w)
			w = 1600
		} else {
			w = max(1, w*1600/h)
			h = 1600
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	bounds := im.Bounds()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, a := im.At(bounds.Min.X+x*c.Width/w, bounds.Min.Y+y*c.Height/h).RGBA()
			at := dst.PixOffset(x, y)
			dst.Pix[at] = uint8((r + 65535 - a) >> 8)
			dst.Pix[at+1] = uint8((g + 65535 - a) >> 8)
			dst.Pix[at+2] = uint8((b + 65535 - a) >> 8)
			dst.Pix[at+3] = 255
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

func aiAttachmentPreview(w http.ResponseWriter, r *http.Request) {
	a, err := loadAIAttachment(r.PathValue("id"))
	if err != nil {
		errOut(w, 404, err)
		return
	}
	i, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || i < 0 || i >= len(a.Parts) || (a.Parts[i].Image == "" && a.Parts[i].ImageID == "") {
		http.NotFound(w, r)
		return
	}
	encoded, err := aiReadPartImage(a.Parts[i])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Write(b)
}

func aiModelVision(r *http.Request, model string) (bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	b, _ := json.Marshal(map[string]string{"model": model})
	resp, err := aiRequest(r.WithContext(ctx), "POST", "api/show", b)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var info struct {
		Capabilities []string `json:"capabilities"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&info) != nil || info.Capabilities == nil {
		return false, errors.New("无法确认模型的图片理解能力，请检查连接或更新 Ollama")
	}
	for _, c := range info.Capabilities {
		if c == "vision" {
			return true, nil
		}
	}
	return false, nil
}

// Resolve local references at send time. Older text-only conversation records need no migration.
func aiExpandAttachments(messages []map[string]any) (bool, error) {
	images, _, err := aiExpandStoredAttachments(context.Background(), messages, 0)
	return images, err
}
