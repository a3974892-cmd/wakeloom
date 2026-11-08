package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAIAttachmentLargeFilesPersistAndSelectTail(t *testing.T) {
	withTempConfig(t, Config{})
	// Exceeds the former 10 MB per file and 31 MB request limits with disk-backed content.
	files := map[string][]byte{}
	for i := 0; i < 3; i++ {
		files[fmt.Sprintf("large%d.txt", i)] = []byte(strings.Repeat("ordinary text ", 850000) + "\nUNIQUE_TAIL_ZEBRA 具体结论是保留旧数据。")
	}
	a, w := aiTestUpload(t, files)
	if w.Code != 200 || a.Files != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	stored, e := loadAIAttachment(a.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range stored.Parts {
		if p.Text != "" || p.TextID == "" {
			t.Fatal("large text retained in manifest")
		}
		name, _ := aiContentPath(p.TextID, ".txt")
		st, e := os.Stat(name)
		if e != nil || st.Size() <= 10<<20 {
			t.Fatal(st, e)
		}
	}
	msgs := []map[string]any{{"role": "user", "content": "Find UNIQUE_TAIL_ZEBRA", "attachments": []map[string]string{{"id": a.ID}}}}
	_, usage, e := aiExpandStoredAttachments(context.Background(), msgs, 0)
	if e != nil {
		t.Fatal(e)
	}
	text := msgs[0]["content"].(string)
	if !strings.Contains(text, "UNIQUE_TAIL_ZEBRA") || utf8.RuneCountInString(text) > aiTextLimit || len(usage) != 1 || usage[0].Chars >= usage[0].TotalChars {
		t.Fatal("tail selection or disclosure incorrect", usage)
	}
}

func TestAIAttachmentManyFilesAndAppend(t *testing.T) {
	withTempConfig(t, Config{})
	files := map[string][]byte{}
	for i := 0; i < 205; i++ {
		files[fmt.Sprintf("docs/file%03d.txt", i)] = []byte(fmt.Sprintf("Document number %d", i))
	}
	a, w := aiTestUpload(t, map[string][]byte{"all.zip": aiTestZIP(t, files)})
	if w.Code != 200 || a.Files != 205 {
		t.Fatal(w.Code, w.Body.String())
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	f, _ := mw.CreateFormFile("files", "extra.txt")
	f.Write([]byte("EXTRA_DOCUMENT"))
	mw.WriteField("attachment_id", a.ID)
	mw.Close()
	r := httptest.NewRequest("POST", "/", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w = httptest.NewRecorder()
	aiAttachmentUpload(w, r)
	var appended aiAttachment
	json.Unmarshal(w.Body.Bytes(), &appended)
	if w.Code != 200 || appended.ID != a.ID || appended.Files != 206 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAIAttachmentStreamingTextBoundaries(t *testing.T) {
	for _, s := range []string{strings.Repeat("a", 65535) + "中🙂文", "\ufeff中文\nUTF8"} {
		var b bytes.Buffer
		sink := &aiTextSink{writer: bufio.NewWriter(&b)}
		if e := aiCopyText(strings.NewReader(s), sink); e != nil {
			t.Fatal(e)
		}
		sink.writer.Flush()
		if b.String() != strings.TrimPrefix(s, "\ufeff") {
			t.Fatal("UTF8 boundary corrupted")
		}
	}
	for _, b := range [][]byte{{255, 254, 0x3d, 0xd8, 0, 0xde}, {254, 255, 0xd8, 0x3d, 0xde, 0}} {
		var out bytes.Buffer
		sink := &aiTextSink{writer: bufio.NewWriter(&out)}
		if e := aiCopyText(bytes.NewReader(b), sink); e != nil {
			t.Fatal(e)
		}
		sink.writer.Flush()
		if out.String() != "😀" {
			t.Fatal(out.String())
		}
	}
}

func TestAIAttachmentImagesAndTextShareBudget(t *testing.T) {
	withTempConfig(t, Config{})
	a, w := aiTestUpload(t, map[string][]byte{"text.txt": []byte(strings.Repeat("sample ", 3000))})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	stored, _ := loadAIAttachment(a.ID)
	for i := 0; i < 10; i++ {
		aiAddStoredPart(stored, aiAttachmentPart{Name: fmt.Sprintf("%d.png", i), Image: "AA=="})
	}
	p, _ := aiAttachmentPath(a.ID)
	writeJSON(p, stored)
	msgs := []map[string]any{{"role": "user", "content": "sample", "images": []any{"BB=="}, "attachments": []map[string]string{{"id": a.ID}}}}
	_, usage, e := aiExpandStoredAttachments(context.Background(), msgs, 0)
	if e != nil {
		t.Fatal(e)
	}
	if len(msgs[0]["images"].([]string)) != 8 || usage[0].OmittedImages != 3 || utf8.RuneCountInString(msgs[0]["content"].(string)) > aiTextLimit {
		t.Fatal(usage)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e = aiEachPartChunk(ctx, stored.Parts[0], func(string, int, int) {})
	if e == nil {
		t.Fatal("canceled scan continued")
	}
}
