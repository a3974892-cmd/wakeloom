package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

var aiAttachmentStoreMu sync.Mutex

type aiContextFile struct {
	ctx  context.Context
	file interface {
		io.Reader
		io.ReaderAt
		io.Seeker
	}
}

func (f aiContextFile) Read(b []byte) (int, error) {
	if e := f.ctx.Err(); e != nil {
		return 0, e
	}
	return f.file.Read(b)
}
func (f aiContextFile) ReadAt(b []byte, offset int64) (int, error) {
	if e := f.ctx.Err(); e != nil {
		return 0, e
	}
	return f.file.ReadAt(b, offset)
}
func (f aiContextFile) Seek(offset int64, whence int) (int64, error) {
	if e := f.ctx.Err(); e != nil {
		return 0, e
	}
	return f.file.Seek(offset, whence)
}

func aiNewContentID() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func aiContentPath(id, ext string) (string, error) {
	b, e := hex.DecodeString(id)
	if e != nil || len(b) != 16 {
		return "", errors.New("附件内容编号无效")
	}
	return filepath.Join(dataDir, "ai-attachment-content", id+ext), nil
}

type aiTextSink struct {
	writer *bufio.Writer
	chars  int
}

func (s *aiTextSink) write(text string) error {
	s.chars += utf8.RuneCountInString(text)
	_, e := s.writer.WriteString(text)
	return e
}

func aiCopyText(reader io.Reader, sink *aiTextSink) error {
	r := bufio.NewReader(reader)
	bom, _ := r.Peek(3)
	if bytes.HasPrefix(bom, []byte{255, 254}) || bytes.HasPrefix(bom, []byte{254, 255}) {
		little := bom[0] == 255
		r.Discard(2)
		var pair [2]byte
		var high uint16
		for {
			_, e := io.ReadFull(r, pair[:])
			if e == io.EOF {
				break
			}
			if e != nil {
				return errors.New("UTF-16 文本不完整")
			}
			v := uint16(pair[0])<<8 | uint16(pair[1])
			if little {
				v = uint16(pair[1])<<8 | uint16(pair[0])
			}
			var ru rune
			if high != 0 {
				ru = utf16.DecodeRune(rune(high), rune(v))
				high = 0
				if ru == utf8.RuneError {
					return errors.New("UTF-16 文本编码无效")
				}
			} else if v >= 0xD800 && v <= 0xDBFF {
				high = v
				continue
			} else if v >= 0xDC00 && v <= 0xDFFF {
				return errors.New("UTF-16 文本编码无效")
			} else {
				ru = rune(v)
			}
			if e = sink.write(string(ru)); e != nil {
				return e
			}
		}
		if high != 0 {
			return errors.New("UTF-16 文本不完整")
		}
		return nil
	}
	if bytes.HasPrefix(bom, []byte{239, 187, 191}) {
		r.Discard(3)
	}
	buf := make([]byte, 64<<10)
	tail := []byte{}
	for {
		n, e := r.Read(buf)
		if n > 0 {
			block := append(tail, buf[:n]...)
			cut := len(block)
			if e != io.EOF {
				for cut > 0 && len(block)-cut < utf8.UTFMax && !utf8.Valid(block[:cut]) {
					cut--
				}
			}
			if !utf8.Valid(block[:cut]) || bytes.IndexByte(block[:cut], 0) >= 0 {
				return errors.New("无法识别文字编码，请另存为 UTF-8")
			}
			if err := sink.write(string(block[:cut])); err != nil {
				return err
			}
			tail = append([]byte{}, block[cut:]...)
		}
		if e == io.EOF {
			if !utf8.Valid(tail) {
				return errors.New("UTF-8 文本不完整")
			}
			return sink.write(string(tail))
		}
		if e != nil {
			return e
		}
	}
}

func aiStoreText(name string, read func(*aiTextSink) error) (aiAttachmentPart, error) {
	id, e := aiNewContentID()
	if e != nil {
		return aiAttachmentPart{}, e
	}
	p, _ := aiContentPath(id, ".txt")
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return aiAttachmentPart{}, e
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return aiAttachmentPart{}, e
	}
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(p)
		}
	}()
	sink := &aiTextSink{writer: bufio.NewWriter(f)}
	e = read(sink)
	if flush := sink.writer.Flush(); e == nil {
		e = flush
	}
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	if e != nil || sink.chars == 0 {
		if e == nil {
			e = errors.New("文件没有可读取的文字")
		}
		return aiAttachmentPart{}, e
	}
	keep = true
	return aiAttachmentPart{Name: name, TextID: id, Chars: sink.chars}, nil
}

func aiStorePDF(name string, in io.ReaderAt, size int64) (part aiAttachmentPart, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("PDF 无法解析，请另存为文本或 Word 后添加")
		}
	}()
	r, e := pdf.NewReader(in, size)
	if e != nil {
		return part, errors.New("PDF 无法读取，可能已损坏或需要密码")
	}
	return aiStoreText(name, func(s *aiTextSink) error {
		for i := 1; i <= r.NumPage(); i++ {
			text, e := r.Page(i).GetPlainText(nil)
			if e != nil {
				return fmt.Errorf("PDF 第 %d 页无法读取", i)
			}
			if e = s.write(text + "\n"); e != nil {
				return e
			}
		}
		if s.chars <= r.NumPage() {
			return errors.New("PDF 没有可提取文字，扫描件请先识别文字")
		}
		return nil
	})
}

func aiStoreDOCX(name string, in io.ReaderAt, size int64) (aiAttachmentPart, error) {
	z, e := zip.NewReader(in, size)
	if e != nil {
		return aiAttachmentPart{}, errors.New("Word 文件损坏")
	}
	for _, f := range z.File {
		if f.Name != "word/document.xml" {
			continue
		}
		r, e := f.Open()
		if e != nil {
			return aiAttachmentPart{}, e
		}
		defer r.Close()
		return aiStoreText(name, func(s *aiTextSink) error {
			d := xml.NewDecoder(r)
			inText := false
			for {
				t, e := d.Token()
				if e == io.EOF {
					return nil
				}
				if e != nil {
					return e
				}
				text := ""
				switch t := t.(type) {
				case xml.StartElement:
					if t.Name.Local == "t" {
						inText = true
					}
					if t.Name.Local == "tab" {
						text = "\t"
					}
					if t.Name.Local == "br" {
						text = "\n"
					}
				case xml.EndElement:
					if t.Name.Local == "t" {
						inText = false
					}
					if t.Name.Local == "p" || t.Name.Local == "tr" {
						text = "\n"
					}
				case xml.CharData:
					if inText {
						text = string(t)
					}
				}
				if e = s.write(text); e != nil {
					return e
				}
			}
		})
	}
	return aiAttachmentPart{}, errors.New("没有找到 Word 正文，请使用 DOCX")
}

func aiStoreImage(name string, in io.ReadSeeker) (aiAttachmentPart, error) {
	encoded, e := aiImageReader(in)
	if e != nil {
		return aiAttachmentPart{}, e
	}
	b, e := base64.StdEncoding.DecodeString(encoded)
	if e != nil {
		return aiAttachmentPart{}, e
	}
	id, e := aiNewContentID()
	if e != nil {
		return aiAttachmentPart{}, e
	}
	p, _ := aiContentPath(id, ".jpg")
	if e = os.MkdirAll(filepath.Dir(p), 0700); e == nil {
		e = os.WriteFile(p, b, 0600)
	}
	if e != nil {
		return aiAttachmentPart{}, e
	}
	return aiAttachmentPart{Name: name, ImageID: id}, nil
}

func aiAttachmentFileKind(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".zip":
		return "zip"
	case ".pdf":
		return "pdf"
	case ".docx":
		return "docx"
	case ".png", ".jpg", ".jpeg", ".gif":
		return "image"
	case ".apk", ".aab", ".jar", ".exe", ".dll", ".sys", ".msi", ".so", ".dex", ".bin", ".elf":
		return "binary"
	}
	if aiTextExtension(ext) || path.Base(name) == "README" || path.Base(name) == "LICENSE" || path.Base(name) == "Dockerfile" {
		return "text"
	}
	return "auto"
}

func aiAddStoredPart(a *aiAttachment, p aiAttachmentPart) {
	if p.BinaryID != "" {
		a.BinaryFiles++
	}
	if p.ImageID != "" || p.Image != "" {
		a.ImageParts = append(a.ImageParts, len(a.Parts))
		a.Images++
	}
	a.Chars += p.Chars
	a.Files++
	a.Parts = append(a.Parts, p)
}

func aiAddStoredFile(a *aiAttachment, name string, in interface {
	io.Reader
	io.ReaderAt
	io.Seeker
}, size int64) error {
	var e error
	name, e = aiSafeAttachmentName(name)
	if e != nil {
		return e
	}
	kind := aiAttachmentFileKind(name)
	automatic := kind == "auto"
	if kind == "auto" {
		kind = aiDetectAttachmentKind(in, size)
	}
	if aiAttachmentSkip(name) || kind == "" {
		a.Skipped = append(a.Skipped, name)
		return nil
	}
	if kind == "zip" {
		z, e := zip.NewReader(in, size)
		if e != nil {
			return errors.New("ZIP 无法读取，请确认完整且没有加密")
		}
		for _, f := range z.File {
			if f.FileInfo().IsDir() {
				continue
			}
			child := path.Base(name) + "/" + f.Name
			if _, e := aiSafeAttachmentName(child); e != nil {
				return e
			}
			if f.Mode()&os.ModeSymlink != 0 {
				a.Skipped = append(a.Skipped, child+"（符号链接）")
				continue
			}
			k := aiAttachmentFileKind(child)
			if k == "" || k == "zip" || aiAttachmentSkip(child) {
				a.Skipped = append(a.Skipped, child)
				continue
			}
			r, e := f.Open()
			if e != nil {
				return e
			}
			if k == "text" {
				p, e := aiStoreText(child, func(s *aiTextSink) error { return aiCopyText(r, s) })
				r.Close()
				if e != nil {
					return fmt.Errorf("%s：%w", child, e)
				}
				aiAddStoredPart(a, p)
				continue
			}
			// ReaderAt is needed by PDF/DOCX; keep binary entries on disk rather than in RAM.
			tmp, e := os.CreateTemp("", "wakeloom-attachment-*")
			if e != nil {
				r.Close()
				return e
			}
			_, e = io.Copy(tmp, r)
			r.Close()
			if e == nil {
				st, _ := tmp.Stat()
				_, e = tmp.Seek(0, io.SeekStart)
				if e == nil {
					e = aiAddStoredFile(a, child, tmp, st.Size())
				}
			}
			tmp.Close()
			os.Remove(tmp.Name())
			if e != nil {
				return e
			}
		}
		return nil
	}
	var p aiAttachmentPart
	switch kind {
	case "text":
		p, e = aiStoreText(name, func(s *aiTextSink) error { return aiCopyText(in, s) })
	case "pdf":
		p, e = aiStorePDF(name, in, size)
	case "docx":
		p, e = aiStoreDOCX(name, in, size)
	case "image":
		p, e = aiStoreImage(name, in)
	case "binary":
		p, e = aiStoreBinary(name, in, size)
	}
	if e != nil && automatic && kind == "text" {
		p, e = aiStoreBinary(name, in, size)
	}
	if e != nil {
		return fmt.Errorf("%s：%w", name, e)
	}
	aiAddStoredPart(a, p)
	return nil
}

func aiAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	// Multipart spills to temporary disk after 1 MB; there is no fixed file-size or file-count ceiling.
	if e := r.ParseMultipartForm(1 << 20); e != nil {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
		errOut(w, 400, fmt.Errorf("附件读取失败：%w", e))
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		errOut(w, 400, errors.New("请选择文件"))
		return
	}
	var names []string
	if raw := r.FormValue("paths"); raw != "" {
		if json.Unmarshal([]byte(raw), &names) != nil || len(names) != len(files) {
			errOut(w, 400, errors.New("文件列表无效"))
			return
		}
	}
	aiAttachmentStoreMu.Lock()
	defer aiAttachmentStoreMu.Unlock()
	if r.Context().Err() != nil {
		return
	}
	a := &aiAttachment{}
	if id := r.FormValue("attachment_id"); id != "" {
		var e error
		a, e = loadAIAttachment(id)
		if e != nil {
			errOut(w, 400, e)
			return
		}
	} else {
		id, e := aiNewContentID()
		if e != nil {
			errOut(w, 500, e)
			return
		}
		a.ID = id
		a.Name = path.Base(files[0].Filename)
		if len(files) > 1 {
			a.Name = fmt.Sprintf("%d 个文件", len(files))
		}
		if len(names) > 0 && strings.Contains(names[0], "/") {
			a.Name = strings.Split(names[0], "/")[0]
		}
		if label := r.FormValue("label"); label != "" {
			a.Name = label
		}
	}
	originalParts := len(a.Parts)
	committed := false
	defer func() {
		if !committed {
			for _, part := range a.Parts[originalParts:] {
				for ext, id := range map[string]string{".txt": part.TextID, ".jpg": part.ImageID, ".bin": part.BinaryID} {
					if id != "" {
						if p, e := aiContentPath(id, ext); e == nil {
							os.Remove(p)
						}
					}
				}
			}
		}
	}()
	for i, f := range files {
		name := f.Filename
		if len(names) > 0 {
			name = names[i]
		}
		in, e := f.Open()
		if e != nil {
			errOut(w, 400, e)
			return
		}
		e = aiAddStoredFile(a, name, aiContextFile{ctx: r.Context(), file: in}, f.Size)
		in.Close()
		if e != nil {
			errOut(w, 400, e)
			return
		}
	}
	if a.Files == 0 {
		errOut(w, 400, errors.New("没有可读取的文件；开发缓存目录会略过。可添加文档、代码、图片、程序文件或 ZIP"))
		return
	}
	a.SkippedTotal = len(a.Skipped)
	p, _ := aiAttachmentPath(a.ID)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		errOut(w, 500, e)
		return
	}
	if e := writeJSON(p, a); e != nil {
		errOut(w, 500, e)
		return
	}
	committed = true
	a.Parts = nil
	if len(a.ImageParts) > 4 {
		a.ImageParts = a.ImageParts[:4]
	}
	if len(a.Skipped) > 100 {
		a.Skipped = a.Skipped[:100]
	}
	jsonOut(w, 200, a)
}
