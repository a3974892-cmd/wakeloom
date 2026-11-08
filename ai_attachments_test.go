package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func aiTestZIP(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, raw := range files {
		f, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(raw); e != nil {
			t.Fatal(e)
		}
	}
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}

func aiTestPDF() []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	stream := "BT /F1 12 Tf 10 200 Td (Attachment PDF test) Tj ET"
	objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, o := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, n := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", n)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

func aiTestUpload(t *testing.T, files map[string][]byte) (aiAttachment, *httptest.ResponseRecorder) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	names := []string{}
	for name, raw := range files {
		f, e := mw.CreateFormFile("files", filepath.Base(name))
		if e != nil {
			t.Fatal(e)
		}
		f.Write(raw)
		names = append(names, name)
	}
	raw, _ := json.Marshal(names)
	mw.WriteField("paths", string(raw))
	mw.Close()
	r := httptest.NewRequest("POST", "/api/ai/attachments", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	aiAttachmentUpload(w, r)
	var a aiAttachment
	json.Unmarshal(w.Body.Bytes(), &a)
	return a, w
}

func TestAIAttachmentFolderArchiveAndDocuments(t *testing.T) {
	withTempConfig(t, Config{})
	doc := aiTestZIP(t, map[string][]byte{"word/document.xml": []byte(`<w:document xmlns:w="urn:word"><w:body><w:p><w:r><w:t>中文 Word 内容</w:t></w:r></w:p></w:body></w:document>`)})
	z := aiTestZIP(t, map[string][]byte{"notes.md": []byte("ZIP 中的笔记"), "program.exe": {0, 1, 2}})
	a, w := aiTestUpload(t, map[string][]byte{"资料/readme.txt": []byte("本地文字"), "资料/report.docx": doc, "资料/report.pdf": aiTestPDF(), "资料/package.zip": z})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if a.Files != 4 || a.Name != "资料" || len(a.Skipped) != 1 || len(a.Parts) != 0 {
		t.Fatalf("summary: %+v", a)
	}
	// The persisted content, not browser storage, must remain available on subsequent requests.
	messages := []map[string]any{{"role": "user", "content": "总结这些资料", "attachments": []map[string]string{{"id": a.ID}}}}
	images, e := aiExpandAttachments(messages)
	if e != nil || images {
		t.Fatal(e)
	}
	got := messages[0]["content"].(string)
	for _, want := range []string{"本地文字", "中文 Word 内容", "Attachment PDF test", "ZIP 中的笔记", "资料/report.docx"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	if _, ok := messages[0]["attachments"]; ok {
		t.Fatal("local references leaked to Ollama")
	}
}

func TestAIAttachmentRejectsUnsafeOrUnreadableFiles(t *testing.T) {
	withTempConfig(t, Config{})
	for _, tc := range []struct {
		name  string
		files map[string][]byte
		want  string
	}{
		{"traversal", map[string][]byte{"bad.zip": aiTestZIP(t, map[string][]byte{"../escape.txt": []byte("bad")})}, "路径无效"},
		{"binary", map[string][]byte{"only.exe": {1, 2, 3}}, "没有可读取"},
		{"bad pdf", map[string][]byte{"bad.pdf": []byte("not pdf")}, "PDF"},
		{"invalid encoding", map[string][]byte{"bad.txt": {255, 1, 0, 4}}, "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w := aiTestUpload(t, tc.files)
			if w.Code != 400 || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dataDir, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("archive escaped")
	}
	if _, e := aiAttachmentPath("../../config"); e == nil {
		t.Fatal("unsafe attachment ID")
	}
	if _, e := aiExpandAttachments([]map[string]any{{"role": "user", "attachments": []map[string]string{{"id": strings.Repeat("0", 32)}}}}); e == nil {
		t.Fatal("missing attachment silently dropped")
	}
}

func TestAIAttachmentImagesRequireVisionAndReachModel(t *testing.T) {
	withTempConfig(t, Config{})
	var img bytes.Buffer
	png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 16, 16)))
	a, w := aiTestUpload(t, map[string][]byte{"image.png": img.Bytes()})
	if w.Code != 200 || a.Images != 1 || len(a.ImageParts) != 1 {
		t.Fatal(w.Body.String())
	}
	vision := false
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wbllm/api/show":
			caps := []string{"completion"}
			if vision {
				caps = append(caps, "vision")
			}
			json.NewEncoder(w).Encode(map[string]any{"capabilities": caps})
		case "/wbllm/api/chat":
			calls++
			var payload struct {
				Messages []struct {
					Images []string `json:"images"`
				} `json:"messages"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			if len(payload.Messages[0].Images) != 1 || payload.Messages[0].Images[0] == "" {
				t.Error("image not forwarded")
			}
			fmt.Fprintln(w, `{"message":{"content":"图片已收到"},"done":true}`)
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cfg := getConfig()
	cfg.Mode = "gateway"
	cfg.GateURL = srv.URL
	cfg.Proxy = "direct"
	saveConfig(cfg)
	raw, _ := json.Marshal(map[string]any{"model": "test", "messages": []map[string]any{{"role": "user", "content": "描述图片", "attachments": []map[string]string{{"id": a.ID}}}}})
	for _, enabled := range []bool{false, true} {
		vision = enabled
		w := httptest.NewRecorder()
		aiChatHandler(w, httptest.NewRequest("POST", "/api/ai/chat", bytes.NewReader(raw)))
		if enabled {
			if w.Code != 200 || !strings.Contains(w.Body.String(), "图片已收到") {
				t.Fatal(w.Code, w.Body.String())
			}
		} else if w.Code != 400 || !strings.Contains(w.Body.String(), "未启用图片理解") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("model invoked %d times", calls)
	}
	// Preview is protected by the same local API key as all other private files.
	h := newServer()
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, httptest.NewRequest("GET", "/api/ai/attachments/"+a.ID+"/preview/0", nil))
	if bad.Code == 200 {
		t.Fatal("unauthenticated preview")
	}
	r := httptest.NewRequest("GET", "/api/ai/attachments/"+a.ID+"/preview/0", nil)
	r.Header.Set("X-Key", apiKey)
	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, r)
	if ok.Code != 200 || ok.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal(ok.Code, ok.Body.String())
	}
}

func TestAIExistingTextConversationStillWorks(t *testing.T) {
	text := strings.Repeat("old message ", 3000)
	msgs := []map[string]any{{"role": "user", "content": text}}
	if _, err := aiExpandAttachments(msgs); err != nil {
		t.Fatal(err)
	}
	if msgs[0]["content"] != text {
		t.Fatal("old conversation changed")
	}
}
