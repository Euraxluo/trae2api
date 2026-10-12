package docparse

// PDF 解析测试。
//
// **测试不依赖任何外部文件**：所有 PDF 都在测试里用代码手写 PDF 语法构造
//（含真实的 xref/trailer），内容流用 compress/zlib 压缩 —— 这样测试自包含、
// 可复现，也不会有"二进制样本进了仓库"的问题。

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 造 PDF

// testPDF 按对象列表拼一个最小但**结构完整**的 PDF（含 xref 表与 trailer）。
// objs[i] 是第 i+1 号对象的内容（不含 "N 0 obj"/"endobj"）。
func testPDF(objs [][]byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n")
	buf.Write([]byte{'%', 0xe2, 0xe3, 0xcf, 0xd3, '\n'}) // 二进制标记
	off := make([]int, len(objs)+1)
	for i, body := range objs {
		off[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n", i+1)
		buf.Write(body)
		buf.WriteString("\nendobj\n")
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objs)+1, xref)
	return buf.Bytes()
}

func pdfFlate(b []byte) []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

// streamObj 造一个流对象。data 必须是**已编码**的数据（/Length 按它算）。
func streamObj(extra, filter string, data []byte) []byte {
	var buf bytes.Buffer
	if filter == "" {
		fmt.Fprintf(&buf, "<< %s /Length %d >>\nstream\n", extra, len(data))
	} else {
		fmt.Fprintf(&buf, "<< %s /Length %d /Filter /%s >>\nstream\n", extra, len(data), filter)
	}
	buf.Write(data)
	buf.WriteString("\nendstream")
	return buf.Bytes()
}

func flateStream(content string) []byte {
	return streamObj("", "FlateDecode", pdfFlate([]byte(content)))
}

// pageObjs 造"1 Catalog / 2 Pages / 3 Page / 4 Contents / 5 Font"这套骨架。
// resources 为 "" 时用页内 /Resources，否则原样放到 /Pages 节点上（测试继承）。
func simplePDFObjs(content string, resources string) [][]byte {
	pageRes := " /Resources << /Font << /F1 5 0 R >> >>"
	pagesRes := ""
	if resources != "" {
		pagesRes = " " + resources
		pageRes = ""
	}
	return [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1" + pagesRes + " >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792]" + pageRes +
			" /Contents 4 0 R >>"),
		flateStream(content),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"),
	}
}

func simplePDF(content string) []byte {
	return testPDF(simplePDFObjs(content, ""))
}

// objStm 造一个 /Type /ObjStm 压缩对象流：把若干小字典打包进一个 FlateDecode 流，
// 文件里**不再有** `N 0 obj` 头（PDF 1.5+ 常见做法）。
func objStm(entries [][2]string) []byte {
	var bodies bytes.Buffer
	offs := make([]int, len(entries))
	for i, e := range entries {
		offs[i] = bodies.Len()
		bodies.WriteString(e[1])
		bodies.WriteString("\n")
	}
	var hdr bytes.Buffer
	for i, e := range entries {
		fmt.Fprintf(&hdr, "%s %d ", e[0], offs[i])
	}
	hdr.WriteString("\n")
	data := append(hdr.Bytes(), bodies.Bytes()...)
	return streamObj(fmt.Sprintf("/Type /ObjStm /N %d /First %d", len(entries), hdr.Len()),
		"FlateDecode", pdfFlate(data))
}

func pdfExpander(t *testing.T, files map[string][]byte) (string, *Expander) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatalf("写测试文件: %v", err)
		}
	}
	return root, New(Options{Enabled: true, AllowedRoots: []string{root}})
}

// ---------------------------------------------------------------- 基础

// dumpFixture 在设置 PDF_FIXTURE_DUMP=<dir> 时把测试用的 PDF 落盘，
// 便于用**另一个独立实现**（脚本或第三方阅读器）校验这些手工构造的 PDF
// 本身是否合法 —— 否则"构造方与解析方共享同一个误解"就测不出来。
// 默认不设置环境变量 → 什么都不做。
func dumpFixture(t *testing.T, name string, data []byte) {
	t.Helper()
	dir := os.Getenv("PDF_FIXTURE_DUMP")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatalf("dump fixture: %v", err)
	}
}

// TestPDFSimpleText 最简单的文本 PDF。
func TestPDFSimpleText(t *testing.T) {
	data := simplePDF("BT /F1 24 Tf 72 700 Td (Hello, PDF!) Tj ET")
	dumpFixture(t, "simple.pdf", data)
	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != "Hello, PDF!" {
		t.Errorf("抽出文本 = %q want %q", got, "Hello, PDF!")
	}
}

// TestPDFLoadThroughExpander .pdf 在 Expander.Load 内部完成分派，
// 产出与其他文本文件一致的 <file> 片段（调用方无需改动）。
func TestPDFLoadThroughExpander(t *testing.T) {
	root, e := pdfExpander(t, map[string][]byte{
		"doc.pdf": simplePDF("BT /F1 24 Tf 72 700 Td (Hello, PDF!) Tj ET"),
	})
	p, n, err := e.Load(filepath.Join(root, "doc.pdf"), 1<<20)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Type != "text" {
		t.Fatalf("类型 = %q want text", p.Type)
	}
	if n <= 0 {
		t.Error("应返回消耗的字节数")
	}
	if !strings.Contains(p.Text, `language="pdf"`) {
		t.Errorf("应标注 language=\"pdf\": %q", p.Text)
	}
	if !strings.HasPrefix(p.Text, "<file ") || !strings.HasSuffix(p.Text, "</file>") {
		t.Errorf("应带 <file> 来源标注: %q", p.Text)
	}
	if !strings.Contains(p.Text, "Hello, PDF!") {
		t.Errorf("缺正文: %q", p.Text)
	}
}

// TestPDFLoadChineseEndToEnd 用户可见的最终形态：中文 PDF 经 Load 后
// 片段里必须出现正确的中文（不是乱码、不是空）。
func TestPDFLoadChineseEndToEnd(t *testing.T) {
	cmap := "1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n" +
		"2 beginbfchar\n<4F60> <4F60>\n<597D> <597D>\nendbfchar\n"
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 4 0 R >> >> " +
			"/Contents 5 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type0 /BaseFont /SimSun /Encoding /Identity-H " +
			"/ToUnicode 6 0 R >>"),
		flateStream("BT /F1 24 Tf 72 700 Td <4F60597D> Tj ET"),
		flateStream(cmap),
	}
	root, e := pdfExpander(t, map[string][]byte{"cn.pdf": testPDF(objs)})
	p, _, err := e.Load(filepath.Join(root, "cn.pdf"), 1<<20)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(p.Text, "你好") {
		t.Errorf("中文正文缺失或乱码: %q", p.Text)
	}
	// 把用户最终看到的内容打进 -v 输出，作为"不是乱码"的直接证据
	t.Logf("Load 产出片段：\n%s", p.Text)
}

// TestPDFMultiLine 用 Td 换行 → 两行。
func TestPDFMultiLine(t *testing.T) {
	data := simplePDF("BT /F1 24 Tf 72 700 Td (Line one) Tj 0 -20 Td (Line two) Tj ET")
	dumpFixture(t, "multiline.pdf", data)
	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != "Line one\nLine two" {
		t.Errorf("抽出文本 = %q want %q", got, "Line one\nLine two")
	}
}

// TestPDFTmNewline Tm 的 y 变化也要断行。
func TestPDFTmNewline(t *testing.T) {
	data := simplePDF("BT /F1 12 Tf 1 0 0 1 72 700 Tm (Alpha) Tj " +
		"1 0 0 1 72 680 Tm (Beta) Tj ET")
	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != "Alpha\nBeta" {
		t.Errorf("抽出文本 = %q want %q", got, "Alpha\nBeta")
	}
}

// TestPDFQuoteOperator ' 操作符换行后输出。
func TestPDFQuoteOperator(t *testing.T) {
	data := simplePDF("BT /F1 12 Tf 14 TL 72 700 Td (One) Tj (Two) ' ET")
	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != "One\nTwo" {
		t.Errorf("抽出文本 = %q want %q", got, "One\nTwo")
	}
}

// TestPDFTJArray TJ 的数组：字符串拼接 + 大负间距当空格。
func TestPDFTJArray(t *testing.T) {
	data := simplePDF("BT /F1 12 Tf 72 700 Td [(Hello) -300 (World)] TJ ET")
	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != "Hello World" {
		t.Errorf("抽出文本 = %q want %q", got, "Hello World")
	}
}

// TestPDFHexString 十六进制字符串 <...>。
func TestPDFHexString(t *testing.T) {
	data := simplePDF("BT /F1 12 Tf 72 700 Td <48656C6C6F> Tj ET")
	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != "Hello" {
		t.Errorf("抽出文本 = %q want %q", got, "Hello")
	}
}

// TestPDFLiteralEscapes 字面串的转义：\( \) \\ \101(八进制)。
func TestPDFLiteralEscapes(t *testing.T) {
	data := simplePDF(`BT /F1 12 Tf 72 700 Td (A\(B\)C\\D\101) Tj ET`)
	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != `A(B)C\DA` {
		t.Errorf("抽出文本 = %q want %q", got, `A(B)C\DA`)
	}
}

// TestPDFMultipleContentStreams /Contents 是数组时按顺序拼接。
func TestPDFMultipleContentStreams(t *testing.T) {
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources " +
			"<< /Font << /F1 6 0 R >> >> /Contents [4 0 R 5 0 R] >>"),
		flateStream("BT /F1 12 Tf 72 700 Td (Part A) Tj ET"),
		flateStream("BT /F1 12 Tf 72 660 Td (Part B) Tj ET"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	}
	got, err := ExtractPDFText(testPDF(objs))
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if !strings.Contains(got, "Part A") || !strings.Contains(got, "Part B") {
		t.Errorf("两个内容流都该抽到: %q", got)
	}
}

// ---------------------------------------------------------------- 中文

// TestPDFChineseToUnicode CID 字体 + /ToUnicode CMap（bfchar 与 bfrange）。
func TestPDFChineseToUnicode(t *testing.T) {
	cmap := `/CIDInit /ProcSet findresource begin
12 dict begin
begincmap
/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def
/CMapName /Test-UCS def
/CMapType 2 def
1 begincodespacerange
<0000> <FFFF>
endcodespacerange
2 beginbfchar
<4F60> <4F60>
<597D> <597D>
endbfchar
1 beginbfrange
<4E00> <4E02> <4E00>
endbfrange
endcmap
CMapName currentdict /CMap defineresource pop
end
end
`
	// 对象编号：1 Catalog 2 Pages 3 Page 4 Font(Type0) 5 CIDFont 6 Content 7 CMap
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources " +
			"<< /Font << /F1 4 0 R >> >> /Contents 6 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type0 /BaseFont /SimSun /Encoding /Identity-H " +
			"/DescendantFonts [5 0 R] /ToUnicode 7 0 R >>"),
		[]byte("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /SimSun " +
			"/CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> >>"),
		flateStream("BT /F1 24 Tf 72 700 Td <4F60597D4E004E014E02> Tj ET"),
		flateStream(cmap),
	}
	got, err := ExtractPDFText(testPDF(objs))
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	dumpFixture(t, "chinese.pdf", testPDF(objs))
	want := "你好一丁丂"
	if got != want {
		t.Errorf("抽出文本 = %q want %q", got, want)
	}
}

// TestPDFCIDWithoutToUnicode 没有 /ToUnicode 的 CID 字体 → 必须报错，不能静默给空串。
func TestPDFCIDWithoutToUnicode(t *testing.T) {
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources " +
			"<< /Font << /F1 5 0 R >> >> /Contents 4 0 R >>"),
		flateStream("BT /F1 24 Tf 72 700 Td <4F60597D> Tj ET"),
		[]byte("<< /Type /Font /Subtype /Type0 /BaseFont /SimSun /Encoding /Identity-H >>"),
	}
	_, err := ExtractPDFText(testPDF(objs))
	if err == nil {
		t.Fatal("没有 /ToUnicode 的 CID 字体应报错，而不是返回空串")
	}
	if !strings.Contains(err.Error(), "ToUnicode") {
		t.Errorf("错误信息应说清原因（提到 ToUnicode）: %v", err)
	}
}

// ---------------------------------------------------------------- 多页

// TestPDFMultiPage 多页按阅读顺序拼接；/Resources 放在 /Pages 节点上（继承）。
func TestPDFMultiPage(t *testing.T) {
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 " +
			"/Resources << /Font << /F1 7 0 R >> >> >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 6 0 R >>"),
		flateStream("BT /F1 24 Tf 72 700 Td (Page one) Tj ET"),
		flateStream("BT /F1 24 Tf 72 700 Td (Page two) Tj ET"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	}
	got, err := ExtractPDFText(testPDF(objs))
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	dumpFixture(t, "twopage.pdf", testPDF(objs))
	if !strings.Contains(got, "Page one") || !strings.Contains(got, "Page two") {
		t.Fatalf("两页都应抽到: %q", got)
	}
	if strings.Index(got, "Page one") > strings.Index(got, "Page two") {
		t.Errorf("页序不对（应按 Kids 顺序）: %q", got)
	}
	if got != "Page one\nPage two" {
		t.Errorf("抽出文本 = %q want %q", got, "Page one\nPage two")
	}
}

// TestPDFMultiPageReverseOrder Kids 顺序与对象号顺序不一致时，按 Kids 走。
func TestPDFMultiPageReverseOrder(t *testing.T) {
	// 对象号顺序是 3(Second) → 4(First)，但 /Kids 是 [4 0 R 3 0 R] → First 在前。
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [4 0 R 3 0 R] /Count 2 /Resources " +
			"<< /Font << /F1 7 0 R >> >> >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /Contents 5 0 R >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /Contents 6 0 R >>"),
		flateStream("BT /F1 12 Tf 72 700 Td (Second) Tj ET"),
		flateStream("BT /F1 12 Tf 72 700 Td (First) Tj ET"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	}
	got, err := ExtractPDFText(testPDF(objs))
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != "First\nSecond" {
		t.Errorf("抽出文本 = %q want %q（应跟随 /Kids 顺序，不是对象号顺序）", got, "First\nSecond")
	}
}

// ---------------------------------------------------------------- 对象流

// TestPDFObjectStream PDF 1.5+ 的 /ObjStm：页面与字体字典都打包在压缩对象流里，
// 文件里没有它们的 `N 0 obj` 头。只做线性扫描的实现会在这里失败。
func TestPDFObjectStream(t *testing.T) {
	// 顶层只有 1 号（ObjStm）和 2 号（内容流）；3-6 号藏在对象流里
	objs := [][]byte{
		objStm([][2]string{
			{"3", "<< /Type /Catalog /Pages 4 0 R >>"},
			{"4", "<< /Type /Pages /Kids [5 0 R] /Count 1 /Resources " +
				"<< /Font << /F1 6 0 R >> >> >>"},
			{"5", "<< /Type /Page /Parent 4 0 R /MediaBox [0 0 612 792] /Contents 2 0 R >>"},
			{"6", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"},
		}),
		flateStream("BT /F1 24 Tf 72 700 Td (From object stream) Tj ET"),
	}
	data := testPDF(objs)
	// trailer 的 /Root 要指向对象流里的 3 号，这里手工改一下
	data = bytes.Replace(data, []byte("/Root 1 0 R"), []byte("/Root 3 0 R"), 1)
	dumpFixture(t, "objstm.pdf", data)

	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("对象流里的页面没被展开: %v", err)
	}
	if got != "From object stream" {
		t.Errorf("抽出文本 = %q want %q", got, "From object stream")
	}
}

// ---------------------------------------------------------------- 报错路径

// TestPDFNoTextErrors 纯图形页（扫描件/矢量图）→ 报错，**不能返回空串**。
func TestPDFNoTextErrors(t *testing.T) {
	data := simplePDF("q 1 0 0 1 0 0 cm 0 0 100 100 re 0.2 0.4 0.6 rg f Q")
	dumpFixture(t, "notext.pdf", data)
	got, err := ExtractPDFText(data)
	if err == nil {
		t.Fatalf("没有文本的 PDF 应报错，实际返回 %q", got)
	}
	if got != "" {
		t.Errorf("报错时不应返回文本: %q", got)
	}
	if !strings.Contains(err.Error(), "扫描件") {
		t.Errorf("错误信息应提示可能是扫描件: %v", err)
	}
}

// TestPDFEncryptedErrors 加密 PDF → 明确报"不支持解密"。
func TestPDFEncryptedErrors(t *testing.T) {
	data := simplePDF("BT /F1 12 Tf 72 700 Td (secret) Tj ET")
	data = bytes.Replace(data, []byte("/Size 6 /Root"),
		[]byte("/Size 6 /Encrypt 99 0 R /Root"), 1)
	if !bytes.Contains(data, []byte("/Encrypt")) {
		t.Fatal("测试构造失败：没插入 /Encrypt")
	}
	_, err := ExtractPDFText(data)
	if err == nil {
		t.Fatal("加密 PDF 应报错")
	}
	if !strings.Contains(err.Error(), "加密") {
		t.Errorf("错误信息应说明是加密: %v", err)
	}
}

// TestPDFUnsupportedFilterErrors DCTDecode（JPEG）流 → 明确报不支持。
func TestPDFUnsupportedFilterErrors(t *testing.T) {
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 5 0 R >> >> " +
			"/Contents 4 0 R >>"),
		streamObj("", "DCTDecode", []byte{0xff, 0xd8, 0xff, 0xd9}),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	}
	_, err := ExtractPDFText(testPDF(objs))
	if err == nil {
		t.Fatal("不支持的过滤器应报错")
	}
	if !strings.Contains(err.Error(), "DCTDecode") {
		t.Errorf("错误信息应点名过滤器: %v", err)
	}
}

// TestPDFNotAPDFErrors 非 PDF 内容 → 报错。
func TestPDFNotAPDFErrors(t *testing.T) {
	for name, data := range map[string][]byte{
		"空":    {},
		"随机字节": []byte("this is definitely not a pdf file at all"),
		"只有头":  []byte("%PDF-1.7\n"),
		"头被截断": []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\n"),
	} {
		if got, err := ExtractPDFText(data); err == nil {
			t.Errorf("%s：应报错，实际返回 %q", name, got)
		}
	}
}

// TestPDFLoadNoTextReturnsError 走 Load 时同样必须是错误（不是空文本片段）。
func TestPDFLoadNoTextReturnsError(t *testing.T) {
	root, e := pdfExpander(t, map[string][]byte{
		"scan.pdf": simplePDF("q 100 0 0 100 0 0 cm /Im0 Do Q"),
	})
	if _, _, err := e.Load(filepath.Join(root, "scan.pdf"), 1<<20); err == nil {
		t.Fatal("扫描件 PDF 经 Load 也应报错")
	}
}

// ---------------------------------------------------------------- 其他过滤

// TestPDFASCIIHexFilter ASCIIHexDecode 流。
func TestPDFASCIIHexFilter(t *testing.T) {
	content := "BT /F1 12 Tf 72 700 Td (Hex filtered) Tj ET"
	enc := []byte(hex.EncodeToString([]byte(content)) + ">")
	objs := simplePDFObjs("", "")
	objs[3] = streamObj("", "ASCIIHexDecode", enc)
	got, err := ExtractPDFText(testPDF(objs))
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != "Hex filtered" {
		t.Errorf("抽出文本 = %q want %q", got, "Hex filtered")
	}
}

// TestPDFUncompressedStream 没有 /Filter 的明文内容流。
func TestPDFUncompressedStream(t *testing.T) {
	objs := simplePDFObjs("", "")
	objs[3] = streamObj("", "", []byte("BT /F1 12 Tf 72 700 Td (Plain stream) Tj ET"))
	got, err := ExtractPDFText(testPDF(objs))
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != "Plain stream" {
		t.Errorf("抽出文本 = %q want %q", got, "Plain stream")
	}
}

// TestPDFNoXrefStillParses trailer 损坏时回退到线性扫描（对象照常能找到）。
func TestPDFNoXrefStillParses(t *testing.T) {
	data := simplePDF("BT /F1 12 Tf 72 700 Td (No trailer) Tj ET")
	// 砍掉 xref + trailer，只留对象区
	if i := bytes.Index(data, []byte("\nxref\n")); i > 0 {
		data = data[:i]
	}
	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("无 xref/trailer 时应靠线性扫描兜底: %v", err)
	}
	if got != "No trailer" {
		t.Errorf("抽出文本 = %q want %q", got, "No trailer")
	}
}

// TestPDFInlineImageSkipped 内联图片的二进制数据不能被当成文本操作数。
func TestPDFInlineImageSkipped(t *testing.T) {
	content := "BT /F1 12 Tf 72 700 Td (Before) Tj ET\n" +
		"q 100 0 0 100 0 0 cm BI /W 2 /H 2 /BPC 8 /CS /G ID \x00\xff\x10\x20 EI Q\n" +
		"BT /F1 12 Tf 72 600 Td (After) Tj ET"
	data := simplePDF(content)
	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if !strings.Contains(got, "Before") || !strings.Contains(got, "After") {
		t.Errorf("内联图片前后的文本都应抽到: %q", got)
	}
}

// TestPDFWinAnsiHighBytes 简单字体没有 /ToUnicode 时按 WinAnsi 解高位字节。
func TestPDFWinAnsiHighBytes(t *testing.T) {
	// 0x93/0x94 是 Windows-1252 的左右双引号
	data := simplePDF("BT /F1 12 Tf 72 700 Td (\x93quoted\x94) Tj ET")
	got, err := ExtractPDFText(data)
	if err != nil {
		t.Fatalf("ExtractPDFText: %v", err)
	}
	if got != "\u201cquoted\u201d" {
		t.Errorf("抽出文本 = %q want %q", got, "\u201cquoted\u201d")
	}
}

// ---------------------------------------------------------------- 不变量

// TestPDFNeverEmptyWithoutError 是**验收标准本身**：
// 要么给出非空文本，要么给出错误；绝不允许"空文本 + nil error"。
//
// 空文本会让用户以为"文件是空的"，而真实原因往往是"解析器不支持这种 PDF"。
func TestPDFNeverEmptyWithoutError(t *testing.T) {
	cases := map[string][]byte{
		"简单文本":   simplePDF("BT /F1 12 Tf 72 700 Td (X) Tj ET"),
		"无文本":    simplePDF("0 0 100 100 re f"),
		"中文":     testPDF(simplePDFObjs("BT /F1 12 Tf 72 700 Td <4F60> Tj ET", "")),
		"空文件":    {},
		"只有头":    []byte("%PDF-1.7\n"),
		"垃圾":     []byte("not a pdf at all"),
		"头后垃圾":   []byte("%PDF-1.7\n" + strings.Repeat("<<<<<<", 200)),
		"超长数字":   []byte("%PDF-1.7\n999999999999999999999999 0 obj\n<< /Type /Catalog >>\nendobj\n"),
		"深嵌套数组":  []byte("%PDF-1.7\n1 0 obj\n" + strings.Repeat("[", 5000) + "\nendobj\n"),
		"未闭合字典":  []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog /Pages\nendobj\n"),
		"未闭合字符串": []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog /X (abc >>\nendobj\n"),
		"截断的流":   []byte("%PDF-1.7\n1 0 obj\n<< /Length 99999 /Filter /FlateDecode >>\nstream\n\x78\x9c\x01\x02\nendstream\nendobj\n"),
		"坏 zlib": testPDF(simplePDFObjs("BT /F1 12 Tf 72 700 Td (X) Tj ET", ""))[:200],
		"对象号悬空": testPDF([][]byte{
			[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
			[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
			[]byte("<< /Type /Page /Parent 2 0 R /Contents 77 0 R >>"),
		}),
		"没有页面对象": testPDF([][]byte{
			[]byte("<< /Type /Catalog >>"),
			[]byte("<< /Type /Font /Subtype /Type1 >>"),
		}),
	}
	for name, data := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic(%v) —— 畸形输入不能让进程崩", name, r)
				}
			}()
			got, err := ExtractPDFText(data)
			if err == nil && strings.TrimSpace(got) == "" {
				t.Errorf("%s: 返回了空文本且没有错误 —— 用户会以为文件是空的", name)
			}
			if err != nil && got != "" {
				t.Errorf("%s: 报错时不应同时返回文本 %q", name, got)
			}
		}()
	}
}

// TestPDFTruncatedEverywhere 任意位置截断都不能 panic / 死循环 / 空文本无错误。
func TestPDFTruncatedEverywhere(t *testing.T) {
	full := simplePDF("BT /F1 24 Tf 72 700 Td (Hello, PDF!) Tj ET")
	for cut := 0; cut < len(full); cut += 7 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("截断到 %d 字节时 panic: %v", cut, r)
				}
			}()
			got, err := ExtractPDFText(full[:cut])
			if err == nil && strings.TrimSpace(got) == "" {
				t.Fatalf("截断到 %d 字节：空文本且无错误", cut)
			}
		}()
	}
}

// TestPDFByteFlipsDoNotPanic 随机翻转字节不能 panic。
func TestPDFByteFlipsDoNotPanic(t *testing.T) {
	full := simplePDF("BT /F1 24 Tf 72 700 Td (Hello, PDF!) Tj ET")
	for i := 0; i < len(full); i += 3 {
		mut := append([]byte(nil), full...)
		mut[i] ^= 0xff
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("翻转第 %d 字节后 panic: %v", i, r)
				}
			}()
			ExtractPDFText(mut)
		}()
	}
}
