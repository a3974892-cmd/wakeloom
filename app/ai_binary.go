package main

// Static inspection only. Uploaded programs are never loaded or executed.
// PE structure: https://learn.microsoft.com/windows/win32/debug/pe-format
// APK layout: https://developer.android.com/studio/debug/apk-analyzer
import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

type aiBinaryInput interface {
	io.Reader
	io.ReaderAt
	io.Seeker
}

func aiDetectAttachmentKind(in io.ReaderAt, size int64) string {
	b := make([]byte, min(size, 65536))
	n, _ := in.ReadAt(b, 0)
	b = b[:n]
	switch {
	case bytes.HasPrefix(b, []byte("%PDF-")):
		return "pdf"
	case bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G'}), bytes.HasPrefix(b, []byte{255, 216, 255}), bytes.HasPrefix(b, []byte("GIF8")):
		return "image"
	case bytes.HasPrefix(b, []byte("PK\x03\x04")):
		return "binary" // unknown archive: inspect without recursive extraction
	case bytes.HasPrefix(b, []byte{255, 254}), bytes.HasPrefix(b, []byte{254, 255}):
		return "text"
	}
	// Permit any text extension, including extensionless files. Allow a partial
	// UTF-8 rune at the sniff boundary but never a NUL or binary control character.
	for trim := 0; trim < 4 && trim <= len(b); trim++ {
		candidate := b[:len(b)-trim]
		if !utf8.Valid(candidate) {
			continue
		}
		for _, c := range candidate {
			if c == 0 || (c < 32 && c != '\n' && c != '\r' && c != '\t' && c != '\f') {
				return "binary"
			}
		}
		return "text"
	}
	return "binary"
}

func aiStoreBinary(name string, in aiBinaryInput, size int64) (part aiAttachmentPart, err error) {
	id, e := aiNewContentID()
	if e != nil {
		return part, e
	}
	dest, _ := aiContentPath(id, ".bin")
	if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
		return part, e
	}
	f, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return part, e
	}
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(dest)
		}
	}()
	if _, e = in.Seek(0, io.SeekStart); e != nil {
		return part, e
	}
	hash := sha256.New()
	copied, e := io.Copy(io.MultiWriter(f, hash), in)
	closeErr := f.Close()
	if e != nil {
		return part, e
	}
	if closeErr != nil {
		return part, closeErr
	}
	if copied != size {
		return part, fmt.Errorf("文件读取不完整：%d/%d 字节", copied, size)
	}
	var report strings.Builder
	fmt.Fprintf(&report, "程序/二进制静态分析报告\n文件：%s\n大小：%d 字节\nSHA-256：%x\n", name, size, hash.Sum(nil))
	report.WriteString("范围：原文件保存在本机；只向模型发送本报告的选段。未执行程序、未反编译或验证数字签名；可读字符串只是线索，不能据此断定行为或安全性。\n")
	head := make([]byte, min(size, 64))
	n, e := in.ReadAt(head, 0)
	if e != nil && e != io.EOF {
		return part, e
	}
	head = head[:n]
	fmt.Fprintf(&report, "文件头（最多 32 字节）：%x\n", head[:min(32, len(head))])
	if bytes.HasPrefix(head, []byte("MZ")) {
		aiPEReport(&report, in, size)
	}
	if bytes.HasPrefix(head, []byte("\x7fELF")) && len(head) >= 20 {
		fmt.Fprintf(&report, "格式：ELF，位宽标记 %d，字节序标记 %d\n", head[4], head[5])
	}
	if bytes.HasPrefix(head, []byte("dex\n")) {
		report.WriteString("格式：Android DEX 字节码；以下不是反编译源码。\n")
	}
	ext := strings.ToLower(filepath.Ext(name))
	if bytes.HasPrefix(head, []byte("PK")) || ext == ".apk" || ext == ".aab" || ext == ".jar" {
		if e = aiArchiveBinaryReport(&report, in, size); e != nil {
			return part, e
		}
	} else {
		const span = int64(4 << 20)
		report.WriteString("\n可读字符串采样：最多扫描文件头尾各 4 MiB，每个窗口最多 120 条，每条最多 240 字；不是全部内容。\n")
		for _, offset := range []int64{0, max(span, size-span)} {
			if offset >= size {
				continue
			}
			b := make([]byte, min(span, size-offset))
			n, e := in.ReadAt(b, offset)
			if e != nil && e != io.EOF {
				return part, e
			}
			fmt.Fprintf(&report, "\n采样字节范围：%d–%d\n", offset, offset+int64(n))
			aiBinaryStrings(&report, b[:n], 120)
		}
	}
	if contextual, ok := in.(aiContextFile); ok && contextual.ctx.Err() != nil {
		return part, contextual.ctx.Err()
	}
	part, e = aiStoreText(name, func(s *aiTextSink) error { return s.write(report.String()) })
	if e != nil {
		return part, e
	}
	part.BinaryID = id
	keep = true
	return part, nil
}

func aiPEReport(out *strings.Builder, in io.ReaderAt, size int64) {
	var dos [64]byte
	if _, e := in.ReadAt(dos[:], 0); e != nil {
		out.WriteString("PE 头不完整。\n")
		return
	}
	offset := int64(binary.LittleEndian.Uint32(dos[60:]))
	var h [24]byte
	if offset > size-24 {
		out.WriteString("PE 头偏移超出文件；无法确认结构。\n")
		return
	}
	if _, e := in.ReadAt(h[:], offset); e != nil || string(h[:4]) != "PE\x00\x00" {
		out.WriteString("MZ 文件未包含有效 PE 签名。\n")
		return
	}
	machine := binary.LittleEndian.Uint16(h[4:])
	arch := map[uint16]string{0x14c: "x86", 0x8664: "x64", 0xaa64: "ARM64", 0x1c4: "ARM"}[machine]
	count := int(binary.LittleEndian.Uint16(h[6:]))
	optional := int64(binary.LittleEndian.Uint16(h[20:]))
	fmt.Fprintf(out, "格式：Windows PE，架构：%s (0x%04x)，节数量：%d\n", arch, machine, count)
	start := offset + 24 + optional
	if count > 96 || start > size || int64(count)*40 > size-start {
		out.WriteString("节表超出检查范围或文件不完整，未继续解析。\n")
		return
	}
	for i := 0; i < count; i++ {
		var s [40]byte
		if _, e := in.ReadAt(s[:], start+int64(i)*40); e != nil {
			return
		}
		fmt.Fprintf(out, "节 %q：文件偏移 %d，原始大小 %d 字节\n", strings.TrimRight(string(s[:8]), "\x00"), binary.LittleEndian.Uint32(s[20:]), binary.LittleEndian.Uint32(s[16:]))
	}
}

func aiArchiveBinaryReport(out *strings.Builder, in io.ReaderAt, size int64) error {
	z, e := zip.NewReader(in, size)
	if e != nil {
		fmt.Fprintf(out, "安装包/归档结构无法读取：%v。仅保留基本信息，未解析内部内容。\n", e)
		return nil
	}
	fmt.Fprintf(out, "\nZIP/安装包包含 %d 项；目录展示前 200 项，不展开或执行内部程序。\n", len(z.File))
	for i, f := range z.File {
		if i >= 200 {
			out.WriteString("其余目录项未展示。\n")
			break
		}
		fmt.Fprintf(out, "%q · %d 字节\n", aiBinaryLabel(f.Name), f.UncompressedSize64)
	}
	// Prioritize the manifest; then sample a bounded number of DEX/config entries.
	selected := []*zip.File{}
	for _, f := range z.File {
		if strings.HasSuffix(f.Name, "AndroidManifest.xml") {
			selected = append(selected, f)
			if len(selected) == 2 {
				break
			}
		}
	}
	for _, f := range z.File {
		if len(selected) >= 8 {
			break
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		if !f.FileInfo().IsDir() && !strings.HasSuffix(f.Name, "AndroidManifest.xml") && (ext == ".dex" || ext == ".json" || ext == ".properties" || ext == ".xml" || ext == ".txt") {
			selected = append(selected, f)
		}
	}
	out.WriteString("\n安装包内部字符串：最多 8 项，每项前 1 MiB / 80 条；二进制 Manifest 仅提取字符串，不还原 XML 属性关系。\n")
	for _, f := range selected {
		r, e := f.Open()
		if e != nil {
			fmt.Fprintf(out, "%q 无法读取：%v\n", aiBinaryLabel(f.Name), e)
			continue
		}
		b, e := io.ReadAll(io.LimitReader(r, 1<<20))
		r.Close()
		fmt.Fprintf(out, "\n内部条目 %q，实际采样 %d/%d 字节\n", aiBinaryLabel(f.Name), len(b), f.UncompressedSize64)
		if e != nil {
			fmt.Fprintf(out, "读取异常：%v\n", e)
			continue
		}
		aiBinaryStrings(out, b, 80)
	}
	return nil
}

func aiBinaryLabel(s string) string {
	r := []rune(s)
	if len(r) > 240 {
		return string(r[:240]) + "…（名称已截短）"
	}
	return s
}

func aiBinaryStrings(out *strings.Builder, b []byte, limit int) {
	seen := map[string]bool{}
	count := 0
	emit := func(rs []rune) {
		if len(rs) < 4 || count >= limit {
			return
		}
		s := string(rs)
		if seen[s] {
			return
		}
		seen[s] = true
		count++
		fmt.Fprintf(out, "%q\n", s)
	}
	run := []rune{}
	for i := 0; i < len(b) && count < limit; {
		r, n := utf8.DecodeRune(b[i:])
		i += n
		if r != utf8.RuneError && unicode.IsPrint(r) {
			if len(run) < 240 {
				run = append(run, r)
			}
		} else {
			emit(run)
			run = nil
		}
	}
	emit(run)
	// Windows and Android often embed ASCII names in UTF-16LE. Inspect both alignments.
	for align := 0; align < 2 && count < limit; align++ {
		run = nil
		for i := align; i+1 < len(b) && count < limit; i += 2 {
			if b[i] >= 32 && b[i] <= 126 && b[i+1] == 0 {
				if len(run) < 240 {
					run = append(run, rune(b[i]))
				}
			} else {
				emit(run)
				run = nil
			}
		}
		emit(run)
	}
	if count == 0 {
		out.WriteString("未在采样范围内找到可读字符串。\n")
	}
	if count >= limit {
		out.WriteString("已达到本段字符串展示上限，其余未展示。\n")
	}
}
