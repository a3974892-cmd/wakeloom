package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAIProgramAttachmentsPersistAndSendReport(t *testing.T) {
	withTempConfig(t, Config{})
	pe := make([]byte, 512)
	copy(pe, "MZ")
	binary.LittleEndian.PutUint32(pe[60:], 64)
	copy(pe[64:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(pe[68:], 0x8664)
	binary.LittleEndian.PutUint16(pe[70:], 1)
	copy(pe[88:], ".text")
	copy(pe[256:], "https://example.invalid/program-support\x00PROGRAM_SAMPLE_MARKER\x00")
	apk := aiTestZIP(t, map[string][]byte{"AndroidManifest.xml": []byte("\x03\x00android.permission.CAMERA\x00sample.package\x00"), "classes.dex": []byte("dex\n039\x00com/example/Widget\x00"), "../never-write.exe": []byte("MZ")})
	for name, raw := range map[string][]byte{"sample.exe": pe, "sample.apk": apk, "library.so": []byte("\x7fELF\x00SAMPLE_ELF_STRING\x00"), "unknown.payload": {0, 1, 2, 255}} {
		a, w := aiTestUpload(t, map[string][]byte{name: raw})
		if w.Code != 200 || a.BinaryFiles != 1 || a.Images != 0 {
			t.Fatal(name, w.Code, w.Body.String())
		}
		stored, e := loadAIAttachment(a.ID)
		if e != nil {
			t.Fatal(e)
		}
		p := stored.Parts[0]
		dest, e := aiContentPath(p.BinaryID, ".bin")
		if e != nil {
			t.Fatal(e)
		}
		actual, e := os.ReadFile(dest)
		if e != nil || !bytes.Equal(actual, raw) {
			t.Fatal("original changed", e)
		}
		report, e := aiPartFullText(p)
		if e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(report, fmt.Sprintf("%x", sha256.Sum256(raw))) || !strings.Contains(report, "未反编译") {
			t.Fatal(report)
		}
		switch name {
		case "sample.exe":
			if !strings.Contains(report, "x64") || !strings.Contains(report, ".text") || !strings.Contains(report, "PROGRAM_SAMPLE_MARKER") {
				t.Fatal(report)
			}
		case "sample.apk":
			if !strings.Contains(report, "android.permission.CAMERA") || !strings.Contains(report, "com/example/Widget") {
				t.Fatal(report)
			}
		}
		msgs := []map[string]any{{"role": "user", "content": "分析文件结构", "attachments": []map[string]string{{"id": a.ID}}}}
		images, usage, e := aiExpandStoredAttachments(context.Background(), msgs, 0)
		if e != nil || images || len(usage) != 1 || !strings.Contains(msgs[0]["content"].(string), "程序静态报告") {
			t.Fatal(e, usage, msgs)
		}
		if _, exists := msgs[0]["attachments"]; exists {
			t.Fatal("local references leaked")
		}
	}
	if _, e := os.Stat(filepath.Join(dataDir, "never-write.exe")); !os.IsNotExist(e) {
		t.Fatal("APK extracted path")
	}
}

func TestAIAttachmentUnknownTextAndArchivePrograms(t *testing.T) {
	withTempConfig(t, Config{})
	a, w := aiTestUpload(t, map[string][]byte{"LICENSE.custom": []byte("CUSTOM_TEXT_CONTENT 中文"), "noextension": []byte("NO_EXTENSION_TEXT"), "component.vue": []byte("<template>TEXT_COMPONENT</template>"), "programs.zip": aiTestZIP(t, map[string][]byte{"tools/file.exe": {0, 1, 2, 3}, "config.properties": []byte("config_value=readable")})})
	if w.Code != 200 || a.Files != 5 || a.BinaryFiles != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	stored, _ := loadAIAttachment(a.ID)
	var all strings.Builder
	for _, p := range stored.Parts {
		text, e := aiPartFullText(p)
		if e != nil {
			t.Fatal(e)
		}
		all.WriteString(text)
	}
	for _, want := range []string{"CUSTOM_TEXT_CONTENT", "NO_EXTENSION_TEXT", "TEXT_COMPONENT", "config_value=readable"} {
		if !strings.Contains(all.String(), want) {
			t.Fatal(want)
		}
	}
	// A long binary file with a textual prefix must fall back after text decoding fails.
	raw := append(bytes.Repeat([]byte("a"), 70000), 0, 255)
	a, w = aiTestUpload(t, map[string][]byte{"opaque.custom": raw})
	if w.Code != 200 || a.BinaryFiles != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAIProgramCancellationAndRollback(t *testing.T) {
	withTempConfig(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := aiStoreBinary("cancel.exe", aiContextFile{ctx: ctx, file: bytes.NewReader([]byte("MZ"))}, 2)
	if e == nil {
		t.Fatal("cancel ignored")
	}
	_, w := aiTestUpload(t, map[string][]byte{"one.exe": []byte("MZ"), "bad.txt": {0, 255}})
	if w.Code != 400 {
		t.Fatal(w.Body.String())
	}
	files, _ := os.ReadDir(filepath.Join(dataDir, "ai-attachment-content"))
	if len(files) != 0 {
		t.Fatal("orphan content", files)
	}
}

func TestAIProgramBoundedReportAndMalformedPE(t *testing.T) {
	withTempConfig(t, Config{})
	data := make([]byte, 64)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[60:], 0xffffffff)
	a, w := aiTestUpload(t, map[string][]byte{"bad.exe": data})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	stored, _ := loadAIAttachment(a.ID)
	report, _ := aiPartFullText(stored.Parts[0])
	if !strings.Contains(report, "偏移超出") {
		t.Fatal(report)
	}
	apk := aiTestZIP(t, map[string][]byte{"classes.dex": bytes.Repeat([]byte("STRING_DATA_\x00"), 200000)})
	a, w = aiTestUpload(t, map[string][]byte{"big.apk": apk})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	stored, _ = loadAIAttachment(a.ID)
	report, _ = aiPartFullText(stored.Parts[0])
	if len(report) > 100000 || !strings.Contains(report, "1048576/") {
		t.Fatal("unbounded or undisclosed sampling", len(report))
	}
}
