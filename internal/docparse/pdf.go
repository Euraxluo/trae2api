package docparse

// PDF 文本抽取（零第三方依赖，只用标准库）。
//
// 设计取舍：PDF 是"压缩流 + 对象图"的格式，**不追求写一个完整解析器**，
// 只做一件事 —— 把常见"文本型 PDF"的正文抽出来：
//
//	线性扫描对象（不依赖 xref，对 xref 损坏 / 增量更新更宽容）
//	  → 找页面树 /Type /Pages → /Type /Page
//	  → 解 FlateDecode 内容流
//	  → 抽 Tj / TJ / ' / " 的字符串
//	  → 按 /ToUnicode CMap 或 WinAnsi 解码成 UTF-8
//
// **抽不出文本时明确报错，绝不返回空串** —— 空串会让用户以为"文件是空的"，
// 而真实原因往往是"这是扫描件"或"用了本实现不支持的字体编码"。
//
// 明确不支持（会报错说明，不会静默返回空）：加密 PDF、扫描件/纯图片页、
// LZWDecode / DCTDecode(JPEG) / JPXDecode / CCITTFaxDecode / RunLengthDecode
// 过滤的流、带 /Predictor 的 FlateDecode、没有 /ToUnicode 的 CID 字体。

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// maxDecodedStream 单个流解压后的字节上限，防"解压炸弹"。
// 内容流正常只有几十 KB；超过这个量级说明文件不正常，截断即可（我们只抽文本）。
const maxDecodedStream = 16 << 20

// ExtractPDFText 从 PDF 字节里抽取正文文本。
//
// 返回的文本已 trim，多页之间用 "\n" 连接。
// 抽不到文本时返回**明确的错误**（不是空串 + nil）。
func ExtractPDFText(data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("文件为空")
	}
	// 头检查：允许前面有少量垃圾字节（个别工具会加 BOM 或私有前缀）
	hdr := bytes.Index(data, []byte("%PDF-"))
	if hdr < 0 || hdr > 1024 {
		return "", errors.New("不是 PDF 文件（未找到 %PDF- 头）")
	}

	objs := scanPDFObjects(data)
	if len(objs) == 0 {
		return "", errors.New("未找到任何 PDF 对象（文件可能损坏或被截断）")
	}
	d := &pdfDoc{objs: objs}
	d.expandObjectStreams() // PDF 1.5+ 会把页面/字体塞进 /ObjStm，必须先展开
	trailer := findTrailer(data)

	if hasEncrypt(trailer, objs) {
		return "", errors.New("PDF 已加密（/Encrypt），本实现不支持解密；" +
			"请先解密或另存为未加密 PDF")
	}

	pages := d.collectPages(trailer)
	if len(pages) == 0 {
		return "", errors.New("未找到页面对象（/Type /Page），无法提取文本")
	}

	var parts []string
	var errs []string
	for i, pg := range pages {
		txt, err := d.pageText(pg)
		if err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 页：%v", i+1, err))
			continue
		}
		if txt = strings.TrimSpace(txt); txt != "" {
			parts = append(parts, txt)
		}
	}
	if len(parts) == 0 {
		msg := "未从 PDF 中提取到任何文本：可能是扫描件（页面只有图片），" +
			"或使用了不支持的字体编码（如缺少 /ToUnicode 的 CID 字体）"
		if len(errs) > 0 {
			msg += "；内容流解析失败：" + strings.Join(errs, "；")
		}
		return "", errors.New(msg)
	}
	return strings.Join(parts, "\n"), nil
}

// ---------------------------------------------------------------- 词法/语法

type pdfKind int

const (
	kNull pdfKind = iota
	kBool
	kNum
	kStr
	kName
	kArr
	kDict
	kRef
	kOp
)

// pdfValue 是 PDF 的一个对象值。只实现抽取文本需要的部分。
type pdfValue struct {
	kind pdfKind
	b    bool
	num  float64
	str  []byte
	name string
	arr  []pdfValue
	dict map[string]pdfValue
	ref  int // 间接引用：对象号
	gen  int // 间接引用：代数（本实现不区分代数）
}

type pdfParser struct {
	b []byte
	i int
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isPDFWS(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\f', 0:
		return true
	}
	return false
}

func isDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func isDelimOrWS(c byte) bool { return isDelim(c) || isPDFWS(c) }

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// parseOne 解析单个对象值（忽略尾随内容）。
func parseOne(b []byte) (pdfValue, error) {
	p := &pdfParser{b: b}
	return p.value()
}

func (p *pdfParser) skip() {
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '%' {
			for p.i < len(p.b) && p.b[p.i] != '\n' && p.b[p.i] != '\r' {
				p.i++
			}
			continue
		}
		if isPDFWS(c) {
			p.i++
			continue
		}
		return
	}
}

// word 读取一段"普通字符"（到分隔符/空白为止）。' 和 " 不是分隔符，
// 所以内容流里的 ' / " 操作符会作为普通词被读出来。
func (p *pdfParser) word() string {
	start := p.i
	for p.i < len(p.b) && !isDelimOrWS(p.b[p.i]) {
		p.i++
	}
	return string(p.b[start:p.i])
}

func (p *pdfParser) value() (pdfValue, error) {
	p.skip()
	if p.i >= len(p.b) {
		return pdfValue{}, io.ErrUnexpectedEOF
	}
	c := p.b[p.i]
	switch {
	case c == '<':
		if p.i+1 < len(p.b) && p.b[p.i+1] == '<' {
			return p.dict()
		}
		return p.hexString()
	case c == '(':
		return p.literalString()
	case c == '[':
		return p.array()
	case c == '/':
		return p.nameValue()
	case c == ']' || c == '>' || c == ')' || c == '}':
		return pdfValue{}, fmt.Errorf("意外的字符 %q", string(rune(c)))
	}
	if c == '+' || c == '-' || c == '.' || isDigit(c) {
		return p.numberOrRef()
	}
	w := p.word()
	if w == "" { // 防御：不认识的分隔符，前进一格避免死循环
		p.i++
		return pdfValue{kind: kNull}, nil
	}
	switch w {
	case "true":
		return pdfValue{kind: kBool, b: true}, nil
	case "false":
		return pdfValue{kind: kBool}, nil
	case "null":
		return pdfValue{kind: kNull}, nil
	}
	return pdfValue{kind: kOp, name: w}, nil
}

func (p *pdfParser) numberOrRef() (pdfValue, error) {
	tok := p.word()
	n, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		return pdfValue{}, fmt.Errorf("非法数字 %q", tok)
	}
	// 试读 "<gen> R"（间接引用）
	j := p.i
	p.skip()
	if p.i < len(p.b) && isDigit(p.b[p.i]) {
		tok2 := p.word()
		if g, err2 := strconv.Atoi(tok2); err2 == nil {
			p.skip()
			if p.i < len(p.b) && p.b[p.i] == 'R' &&
				(p.i+1 >= len(p.b) || isDelimOrWS(p.b[p.i+1])) {
				p.i++
				if n == math.Trunc(n) && n >= 0 {
					return pdfValue{kind: kRef, ref: int(n), gen: g}, nil
				}
			}
		}
	}
	p.i = j
	return pdfValue{kind: kNum, num: n}, nil
}

func (p *pdfParser) dict() (pdfValue, error) {
	p.i += 2 // <<
	d := map[string]pdfValue{}
	for {
		p.skip()
		if p.i >= len(p.b) {
			return pdfValue{}, io.ErrUnexpectedEOF
		}
		if p.b[p.i] == '>' {
			if p.i+1 < len(p.b) && p.b[p.i+1] == '>' {
				p.i += 2
				return pdfValue{kind: kDict, dict: d}, nil
			}
			p.i++
			continue
		}
		k, err := p.value()
		if err != nil {
			return pdfValue{}, err
		}
		if k.kind != kName {
			continue // 键必须是名字，跳过
		}
		v, err := p.value()
		if err != nil {
			return pdfValue{}, err
		}
		d[k.name] = v
	}
}

func (p *pdfParser) array() (pdfValue, error) {
	p.i++ // [
	var a []pdfValue
	for {
		p.skip()
		if p.i >= len(p.b) || p.b[p.i] == ']' {
			if p.i < len(p.b) {
				p.i++
			}
			return pdfValue{kind: kArr, arr: a}, nil
		}
		v, err := p.value()
		if err != nil {
			return pdfValue{kind: kArr, arr: a}, nil
		}
		a = append(a, v)
	}
}

func (p *pdfParser) nameValue() (pdfValue, error) {
	p.i++ // /
	var sb strings.Builder
	for p.i < len(p.b) && !isDelimOrWS(p.b[p.i]) {
		c := p.b[p.i]
		if c == '#' && p.i+2 < len(p.b) {
			h, l := hexVal(p.b[p.i+1]), hexVal(p.b[p.i+2])
			if h >= 0 && l >= 0 {
				sb.WriteByte(byte(h<<4 | l))
				p.i += 3
				continue
			}
		}
		sb.WriteByte(c)
		p.i++
	}
	return pdfValue{kind: kName, name: sb.String()}, nil
}

func (p *pdfParser) hexString() (pdfValue, error) {
	p.i++ // <
	var out []byte
	var cur byte
	n := 0
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '>' {
			p.i++
			break
		}
		v := hexVal(c)
		if v >= 0 {
			if n%2 == 0 {
				cur = byte(v) << 4
			} else {
				out = append(out, cur|byte(v))
			}
			n++
		}
		p.i++
	}
	if n%2 == 1 {
		out = append(out, cur)
	}
	return pdfValue{kind: kStr, str: out}, nil
}

func (p *pdfParser) literalString() (pdfValue, error) {
	p.i++ // (
	depth := 1
	var out []byte
	for p.i < len(p.b) {
		c := p.b[p.i]
		switch c {
		case '\\':
			p.i++
			if p.i >= len(p.b) {
				break
			}
			e := p.b[p.i]
			switch e {
			case 'n':
				out = append(out, '\n')
				p.i++
			case 'r':
				out = append(out, '\r')
				p.i++
			case 't':
				out = append(out, '\t')
				p.i++
			case 'b':
				out = append(out, '\b')
				p.i++
			case 'f':
				out = append(out, '\f')
				p.i++
			case '(', ')', '\\':
				out = append(out, e)
				p.i++
			case '\r': // 行继续
				p.i++
				if p.i < len(p.b) && p.b[p.i] == '\n' {
					p.i++
				}
			case '\n':
				p.i++
			default:
				if e >= '0' && e <= '7' { // 八进制
					v, k := 0, 0
					for k < 3 && p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '7' {
						v = v*8 + int(p.b[p.i]-'0')
						p.i++
						k++
					}
					out = append(out, byte(v))
				} else {
					out = append(out, e)
					p.i++
				}
			}
		case '(':
			depth++
			out = append(out, c)
			p.i++
		case ')':
			depth--
			if depth == 0 {
				p.i++
				return pdfValue{kind: kStr, str: out}, nil
			}
			out = append(out, c)
			p.i++
		default:
			out = append(out, c)
			p.i++
		}
	}
	return pdfValue{kind: kStr, str: out}, nil
}

// ---------------------------------------------------------------- 对象扫描

type pdfObj struct {
	num       int
	gen       int
	dict      map[string]pdfValue
	stream    []byte // 未解码的原始流
	hasStream bool
}

func indexKeyword(data []byte, from int, kw string) int {
	if from < 0 {
		from = 0
	}
	for i := from; i+len(kw) <= len(data); i++ {
		if data[i] != kw[0] || string(data[i:i+len(kw)]) != kw {
			continue
		}
		if i > 0 && !isDelimOrWS(data[i-1]) {
			continue // 例如 "endstream" 里的 "stream"
		}
		if i+len(kw) < len(data) && !isDelimOrWS(data[i+len(kw)]) {
			continue
		}
		return i
	}
	return -1
}

func skipWS(data []byte, i int) int {
	for i < len(data) && isPDFWS(data[i]) {
		i++
	}
	return i
}

func hasKeywordAt(data []byte, i int, kw string) bool {
	return i >= 0 && i+len(kw) <= len(data) && string(data[i:i+len(kw)]) == kw
}

// findObjHeader 找下一个 `N G obj`。返回对象头位置、对象号、代数、body 起点。
func findObjHeader(data []byte, from int) (pos, num, gen, body int) {
	for i := from; i < len(data); i++ {
		if !isDigit(data[i]) {
			continue
		}
		if i > 0 && !isDelimOrWS(data[i-1]) {
			continue
		}
		j := i
		for j < len(data) && isDigit(data[j]) {
			j++
		}
		if j >= len(data) || !isPDFWS(data[j]) {
			continue
		}
		k := skipWS(data, j)
		if k >= len(data) || !isDigit(data[k]) {
			continue
		}
		m := k
		for m < len(data) && isDigit(data[m]) {
			m++
		}
		if m >= len(data) || !isPDFWS(data[m]) {
			continue
		}
		n := skipWS(data, m)
		if !hasKeywordAt(data, n, "obj") {
			continue
		}
		if n+3 < len(data) && !isDelimOrWS(data[n+3]) {
			continue
		}
		nn, _ := strconv.Atoi(string(data[i:j]))
		gg, _ := strconv.Atoi(string(data[k:m]))
		return i, nn, gg, n + 3
	}
	return -1, 0, 0, 0
}

// scanPDFObjects 线性扫描全部对象。
//
// 为什么不用 xref：xref 可能是流式（1.5+）、可能因增量更新而过期、
// 也可能被工具改坏。线性扫描对"能解析出文本"这个目标更稳。
// 代价：需要正确跳过 stream 数据，否则会把流里的字节当成对象头。
func scanPDFObjects(data []byte) map[int]*pdfObj {
	objs := map[int]*pdfObj{}
	i := 0
	for i < len(data) {
		pos, num, gen, body := findObjHeader(data, i)
		if pos < 0 {
			break
		}
		o := &pdfObj{num: num, gen: gen}
		sp := indexKeyword(data, body, "stream")
		ep := indexKeyword(data, body, "endobj")
		next := len(data)

		if sp >= 0 && (ep < 0 || sp < ep) {
			if v, err := parseOne(data[body:sp]); err == nil && v.kind == kDict {
				o.dict = v.dict
			}
			q := sp + len("stream")
			if q < len(data) && data[q] == '\r' {
				q++
			}
			if q < len(data) && data[q] == '\n' {
				q++
			}
			// 优先用 /Length（直接值）精确切；间接 /Length 时回退到找 endstream
			ln := -1
			if o.dict != nil {
				if lv, ok := o.dict["Length"]; ok && lv.kind == kNum {
					ln = int(lv.num)
				}
			}
			end := q
			if ln >= 0 && q+ln <= len(data) {
				r := skipWS(data, q+ln)
				if hasKeywordAt(data, r, "endstream") {
					o.stream = data[q : q+ln]
					end = r + len("endstream")
				}
			}
			if o.stream == nil {
				es := indexKeyword(data, q, "endstream")
				if es < 0 {
					o.stream = data[q:]
					end = len(data)
				} else {
					e := es
					if e > q && data[e-1] == '\n' {
						e--
					}
					if e > q && data[e-1] == '\r' {
						e--
					}
					o.stream = data[q:e]
					end = es + len("endstream")
				}
			}
			o.hasStream = true
			eo := indexKeyword(data, end, "endobj")
			if eo < 0 {
				next = len(data)
			} else {
				next = eo + len("endobj")
			}
		} else {
			end := ep
			if end < 0 {
				end = len(data)
			} else {
				next = end + len("endobj")
			}
			if v, err := parseOne(data[body:end]); err == nil && v.kind == kDict {
				o.dict = v.dict
			}
		}
		objs[num] = o // 增量更新时后定义的覆盖先定义的
		if next <= pos {
			next = pos + 1
		}
		i = next
	}
	return objs
}

// findTrailer 找最后一个含内容的 trailer 字典。
func findTrailer(data []byte) map[string]pdfValue {
	var best map[string]pdfValue
	for i := 0; ; {
		p := indexKeyword(data, i, "trailer")
		if p < 0 {
			break
		}
		pp := &pdfParser{b: data, i: p + len("trailer")}
		if v, err := pp.value(); err == nil && v.kind == kDict {
			best = v.dict
		}
		i = p + len("trailer")
	}
	return best
}

func hasEncrypt(trailer map[string]pdfValue, objs map[int]*pdfObj) bool {
	if trailer != nil {
		if _, ok := trailer["Encrypt"]; ok {
			return true
		}
	}
	for _, o := range objs {
		if o.dict == nil {
			continue
		}
		if _, ok := o.dict["Encrypt"]; ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 文档视图

type pdfDoc struct {
	objs     map[int]*pdfObj
	expanded map[int]bool // 已展开过的 /ObjStm，防重复
}

func numOf(v pdfValue) (float64, bool) {
	if v.kind == kNum {
		return v.num, true
	}
	return 0, false
}

// expandObjectStreams 把 /Type /ObjStm 里打包的对象展开进对象表。
//
// 为什么必须做：PDF 1.5+ 的常见生成器（qpdf、Acrobat 的"压缩对象流"）会把
// 页面、字体、Catalog 这些**小字典**打包进一个压缩流，文件里根本没有
// `N 0 obj` 头 —— 只做线性扫描的话，这类文件会以"未找到页面对象"告终。
func (d *pdfDoc) expandObjectStreams() {
	if d.expanded == nil {
		d.expanded = map[int]bool{}
	}
	for pass := 0; pass < 4; pass++ {
		added := false
		for _, n := range d.sortedObjNums() {
			o := d.objs[n]
			if o == nil || !o.hasStream || o.dict == nil || d.expanded[n] {
				continue
			}
			if nameOf(o.dict["Type"]) != "ObjStm" {
				continue
			}
			d.expanded[n] = true
			if d.expandOneObjStm(o) {
				added = true
			}
		}
		if !added {
			return
		}
	}
}

func (d *pdfDoc) expandOneObjStm(o *pdfObj) bool {
	raw, err := decodeStream(o.dict, o.stream)
	if err != nil {
		return false
	}
	cnt, ok1 := numOf(o.dict["N"])
	first, ok2 := numOf(o.dict["First"])
	if !ok1 || !ok2 || cnt <= 0 || cnt > 100000 || first < 0 || int(first) > len(raw) {
		return false
	}
	base := int(first)
	// 流开头是 /N 组 "(对象号) (相对偏移)" 的纯文本对
	type ent struct{ num, off int }
	var ents []ent
	p := &pdfParser{b: raw}
	for i := 0; i < int(cnt); i++ {
		p.skip()
		a := p.word()
		p.skip()
		b := p.word()
		an, err1 := strconv.Atoi(a)
		bn, err2 := strconv.Atoi(b)
		if a == "" || b == "" || err1 != nil || err2 != nil {
			break
		}
		ents = append(ents, ent{an, bn})
	}
	if len(ents) == 0 {
		return false
	}
	added := false
	for _, e := range ents {
		start := base + e.off
		if start < 0 || start >= len(raw) {
			continue
		}
		if _, exists := d.objs[e.num]; exists {
			continue // 顶层已定义的同号对象优先
		}
		// 只解析一个值：ObjStm 里的对象不能是流，字典/数组/标量都够用了
		sub := &pdfParser{b: raw, i: start}
		v, err := sub.value()
		if err != nil || v.kind != kDict {
			continue
		}
		d.objs[e.num] = &pdfObj{num: e.num, dict: v.dict}
		added = true
	}
	return added
}

func (d *pdfDoc) objOf(v pdfValue) *pdfObj {
	for i := 0; i < 32 && v.kind == kRef; i++ {
		o := d.objs[v.ref]
		if o == nil {
			return nil
		}
		return o
	}
	return nil
}

func (d *pdfDoc) dictOf(v pdfValue) map[string]pdfValue {
	if v.kind == kDict {
		return v.dict
	}
	if o := d.objOf(v); o != nil {
		return o.dict
	}
	return nil
}

func (d *pdfDoc) sortedObjNums() []int {
	nums := make([]int, 0, len(d.objs))
	for n := range d.objs {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	return nums
}

func nameOf(v pdfValue) string {
	if v.kind == kName {
		return v.name
	}
	return ""
}

func filterNames(v pdfValue) []string {
	switch v.kind {
	case kName:
		return []string{v.name}
	case kArr:
		var out []string
		for _, e := range v.arr {
			if e.kind == kName {
				out = append(out, e.name)
			}
		}
		return out
	}
	return nil
}

// ---------------------------------------------------------------- 流解码

func decodeStream(dict map[string]pdfValue, raw []byte) ([]byte, error) {
	filters := filterNames(dict["Filter"])
	if len(filters) == 0 {
		return raw, nil
	}
	if err := checkPredictor(dict["DecodeParms"]); err != nil {
		return nil, err
	}
	out := raw
	for _, f := range filters {
		var err error
		switch f {
		case "FlateDecode", "Fl":
			out, err = flateDecode(out)
		case "ASCIIHexDecode", "AHx":
			out, err = asciiHexDecode(out)
		default:
			return nil, fmt.Errorf("不支持的流过滤器 /%s（本实现只支持 FlateDecode 与 "+
				"ASCIIHexDecode；LZWDecode、DCTDecode(JPEG)、JPXDecode、"+
				"CCITTFaxDecode、RunLengthDecode 均未实现）", f)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func checkPredictor(v pdfValue) error {
	check := func(d map[string]pdfValue) error {
		if d == nil {
			return nil
		}
		if p, ok := d["Predictor"]; ok && p.kind == kNum && p.num > 1 {
			return errors.New("不支持带 /Predictor 的 FlateDecode 流（PNG/TIFF 预测器未实现）")
		}
		return nil
	}
	switch v.kind {
	case kDict:
		return check(v.dict)
	case kArr:
		for _, e := range v.arr {
			if err := check(e.dict); err != nil {
				return err
			}
		}
	}
	return nil
}

func flateDecode(raw []byte) ([]byte, error) {
	if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
		out, rerr := io.ReadAll(io.LimitReader(zr, maxDecodedStream))
		zr.Close()
		if rerr == nil {
			return out, nil
		}
	}
	// 回退：个别 PDF 写的是裸 deflate（没有 zlib 头）
	fr := flate.NewReader(bytes.NewReader(raw))
	out, err := io.ReadAll(io.LimitReader(fr, maxDecodedStream))
	fr.Close()
	if err != nil {
		return nil, fmt.Errorf("FlateDecode 解压失败: %w", err)
	}
	return out, nil
}

func asciiHexDecode(raw []byte) ([]byte, error) {
	var out []byte
	var cur byte
	n := 0
	for _, c := range raw {
		if c == '>' {
			break
		}
		v := hexVal(c)
		if v < 0 {
			continue
		}
		if n%2 == 0 {
			cur = byte(v) << 4
		} else {
			out = append(out, cur|byte(v))
		}
		n++
	}
	if n%2 == 1 {
		out = append(out, cur)
	}
	return out, nil
}

// ---------------------------------------------------------------- 页面

type pdfPage struct {
	obj       *pdfObj
	resources map[string]pdfValue
	contents  pdfValue
}

func (d *pdfDoc) collectPages(trailer map[string]pdfValue) []*pdfPage {
	var out []*pdfPage
	if trailer != nil {
		if root, ok := trailer["Root"]; ok {
			if rd := d.dictOf(root); rd != nil {
				if pages, ok := rd["Pages"]; ok {
					d.walkPageTree(pages, nil, &out, 0)
				}
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	nums := d.sortedObjNums()
	// 回退 1：任何 /Type /Catalog 对象（trailer 丢失/损坏时）
	for _, n := range nums {
		o := d.objs[n]
		if o.dict == nil || nameOf(o.dict["Type"]) != "Catalog" {
			continue
		}
		if pages, ok := o.dict["Pages"]; ok {
			d.walkPageTree(pages, nil, &out, 0)
			if len(out) > 0 {
				return out
			}
		}
	}
	// 回退 2：任何 /Type /Pages 对象
	for _, n := range nums {
		o := d.objs[n]
		if o.dict == nil || nameOf(o.dict["Type"]) != "Pages" {
			continue
		}
		d.walkPageTree(pdfValue{kind: kDict, dict: o.dict}, nil, &out, 0)
		if len(out) > 0 {
			return out
		}
	}
	// 回退 3：按对象号顺序收集所有 /Type /Page（顺序可能不等于阅读顺序）
	for _, n := range nums {
		o := d.objs[n]
		if o.dict == nil || nameOf(o.dict["Type"]) != "Page" {
			continue
		}
		out = append(out, &pdfPage{
			obj:       o,
			resources: d.dictOf(o.dict["Resources"]),
			contents:  o.dict["Contents"],
		})
	}
	return out
}

func (d *pdfDoc) walkPageTree(v pdfValue, inherited map[string]pdfValue, out *[]*pdfPage, depth int) {
	if depth > 64 || len(*out) > 20000 {
		return
	}
	node := d.dictOf(v)
	if node == nil {
		return
	}
	res := inherited
	if r, ok := node["Resources"]; ok {
		if rd := d.dictOf(r); rd != nil {
			res = rd // 子节点的 /Resources 覆盖继承来的
		}
	}
	switch nameOf(node["Type"]) {
	case "Page":
		*out = append(*out, &pdfPage{obj: d.objOf(v), resources: res, contents: node["Contents"]})
	case "Pages":
		if kids, ok := node["Kids"]; ok && kids.kind == kArr {
			for _, k := range kids.arr {
				d.walkPageTree(k, res, out, depth+1)
			}
		}
	default:
		if kids, ok := node["Kids"]; ok && kids.kind == kArr {
			for _, k := range kids.arr {
				d.walkPageTree(k, res, out, depth+1)
			}
		}
	}
}

func (d *pdfDoc) streamOf(v pdfValue) ([]byte, error) {
	o := d.objOf(v)
	if o == nil {
		return nil, errors.New("引用的对象不存在")
	}
	if !o.hasStream {
		return nil, fmt.Errorf("对象 %d 不是流", o.num)
	}
	return decodeStream(o.dict, o.stream)
}

// contentBytes 取页面的内容流（可能是单个流，也可能是流数组）。
func (d *pdfDoc) contentBytes(v pdfValue) ([]byte, error) {
	switch v.kind {
	case kNull:
		return nil, nil
	case kArr:
		var buf bytes.Buffer
		for _, e := range v.arr {
			b, err := d.contentBytes(e)
			if err != nil {
				return nil, err
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		return buf.Bytes(), nil
	case kRef:
		return d.streamOf(v)
	}
	return nil, fmt.Errorf("无法识别的 /Contents（kind=%d）", v.kind)
}

func (d *pdfDoc) pageText(pg *pdfPage) (string, error) {
	content, err := d.contentBytes(pg.contents)
	if err != nil {
		return "", err
	}
	if len(content) == 0 {
		return "", nil
	}
	ex := &textExtractor{fonts: d.pageFonts(pg.resources)}
	ex.run(contentTokens(content))
	txt := ex.text()
	if txt == "" && ex.unmapped > 0 {
		return "", fmt.Errorf("页面上有 %d 个码位未能映射成 Unicode"+
			"（字体缺少 /ToUnicode，或使用了本实现不支持的编码）", ex.unmapped)
	}
	return txt, nil
}

// ---------------------------------------------------------------- 字体

type pdfFont struct {
	codeBytes int
	toUnicode map[uint32]string
	cid       bool
}

// decode 把内容流里的字节串解码成 UTF-8。第二个返回值是"未能映射的码位数"，
// 用于最后判断"是不是整篇都没解出来"。
func (f *pdfFont) decode(b []byte) (string, int) {
	if f == nil {
		return latin1String(b), 0
	}
	width := f.codeBytes
	if width <= 0 {
		width = 1
	}
	if f.toUnicode != nil {
		var sb strings.Builder
		miss := 0
		for i := 0; i < len(b); {
			w := width
			if i+w > len(b) {
				w = len(b) - i
			}
			code := beU32(b[i : i+w])
			if s, ok := f.toUnicode[code]; ok {
				sb.WriteString(s)
			} else if f.cid {
				miss++ // CID 码位没有映射：跳过，最后统计
			} else {
				sb.WriteString(latin1String(b[i : i+w]))
			}
			i += w
		}
		return sb.String(), miss
	}
	if f.cid {
		return "", len(b) // 没有 /ToUnicode 的 CID 字体：无法可靠解码
	}
	return latin1String(b), 0
}

func (d *pdfDoc) pageFonts(res map[string]pdfValue) map[string]*pdfFont {
	out := map[string]*pdfFont{}
	if res == nil {
		return out
	}
	fd := d.dictOf(res["Font"])
	if fd == nil {
		return out
	}
	for name, fv := range fd {
		fo := d.dictOf(fv)
		if fo == nil {
			continue
		}
		f := &pdfFont{codeBytes: 1}
		if nameOf(fo["Subtype"]) == "Type0" {
			f.cid = true
			f.codeBytes = 2
		}
		if tu, ok := fo["ToUnicode"]; ok {
			if raw, err := d.streamOf(tu); err == nil {
				cb, m := parseToUnicodeCMap(raw)
				if len(m) > 0 {
					f.toUnicode = m
					if cb > 0 {
						f.codeBytes = cb
					}
				}
			}
		}
		out[name] = f
	}
	return out
}

// Windows-1252 里 0x80-0x9F 的补充字符（PDF 简单字体默认编码基本等于它）。
var win1252 = map[byte]rune{
	0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„', 0x85: '…', 0x86: '†', 0x87: '‡',
	0x88: 'ˆ', 0x89: '‰', 0x8a: 'Š', 0x8b: '‹', 0x8c: 'Œ', 0x8e: 'Ž', 0x91: '‘',
	0x92: '’', 0x93: '“', 0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—', 0x98: '˜',
	0x99: '™', 0x9a: 'š', 0x9b: '›', 0x9c: 'œ', 0x9e: 'ž', 0x9f: 'Ÿ',
}

func latin1String(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if r, ok := win1252[c]; ok {
			sb.WriteRune(r)
			continue
		}
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

func beU32(b []byte) uint32 {
	var v uint32
	for i := 0; i < len(b) && i < 4; i++ {
		v = v<<8 | uint32(b[i])
	}
	return v
}

func utf16beString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) == 1 {
		return string(rune(b[0]))
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(u))
}

func addToUTF16(b []byte, delta uint32) []byte {
	out := append([]byte(nil), b...)
	if len(out) < 2 {
		return out
	}
	v := uint32(out[len(out)-2])<<8 | uint32(out[len(out)-1])
	v += delta
	out[len(out)-2] = byte(v >> 8)
	out[len(out)-1] = byte(v)
	return out
}

// ---------------------------------------------------------------- ToUnicode

type cmapTok struct {
	kind int // 0=词 1=十六进制串 2=十六进制数组
	word string
	hex  []byte
	arr  [][]byte
}

func cmapTokens(b []byte) []cmapTok {
	var toks []cmapTok
	i := 0
	for i < len(b) {
		c := b[i]
		if isPDFWS(c) {
			i++
			continue
		}
		if c == '%' {
			for i < len(b) && b[i] != '\n' && b[i] != '\r' {
				i++
			}
			continue
		}
		switch c {
		case '<':
			if i+1 < len(b) && b[i+1] == '<' { // << /Registry ... >> 之类的字典
				i = skipDictBytes(b, i)
				continue
			}
			j := i + 1
			var out []byte
			var cur byte
			n := 0
			for j < len(b) && b[j] != '>' {
				if v := hexVal(b[j]); v >= 0 {
					if n%2 == 0 {
						cur = byte(v) << 4
					} else {
						out = append(out, cur|byte(v))
					}
					n++
				}
				j++
			}
			i = j + 1
			toks = append(toks, cmapTok{kind: 1, hex: out})
		case '[':
			i++
			var list [][]byte
			for i < len(b) && b[i] != ']' {
				if b[i] == '<' {
					j := i + 1
					var out []byte
					var cur byte
					n := 0
					for j < len(b) && b[j] != '>' {
						if v := hexVal(b[j]); v >= 0 {
							if n%2 == 0 {
								cur = byte(v) << 4
							} else {
								out = append(out, cur|byte(v))
							}
							n++
						}
						j++
					}
					i = j + 1
					list = append(list, out)
					continue
				}
				i++
			}
			if i < len(b) {
				i++
			}
			toks = append(toks, cmapTok{kind: 2, arr: list})
		case ']', '>', '{', '}':
			i++
		default:
			start := i
			for i < len(b) && !isDelimOrWS(b[i]) {
				i++
			}
			if w := string(b[start:i]); w != "" {
				toks = append(toks, cmapTok{kind: 0, word: w})
			} else {
				i++
			}
		}
	}
	return toks
}

func skipDictBytes(b []byte, i int) int {
	depth := 0
	for i < len(b) {
		if i+1 < len(b) && b[i] == '<' && b[i+1] == '<' {
			depth++
			i += 2
			continue
		}
		if i+1 < len(b) && b[i] == '>' && b[i+1] == '>' {
			depth--
			i += 2
			if depth <= 0 {
				return i
			}
			continue
		}
		i++
	}
	return i
}

// parseToUnicodeCMap 解析 CMap，返回码位宽度与 码位→Unicode 映射。
//
// 覆盖 begincodespacerange / beginbfchar / beginbfrange（含数组目标）。
// 不覆盖 usecmap 继承与 CIDRange 以外的 PostScript 逻辑。
func parseToUnicodeCMap(b []byte) (int, map[uint32]string) {
	m := map[uint32]string{}
	codeBytes := 1
	mode := ""
	var lastHex []byte
	var group []cmapTok

	for _, tk := range cmapTokens(b) {
		if tk.kind == 0 {
			switch tk.word {
			case "begincodespacerange":
				mode = "csr"
			case "endcodespacerange":
				mode = ""
			case "beginbfchar":
				mode, lastHex = "bfchar", nil
			case "endbfchar":
				mode = ""
			case "beginbfrange":
				mode, group = "bfrange", nil
			case "endbfrange":
				mode = ""
			}
			continue
		}
		switch mode {
		case "csr":
			if tk.kind == 1 {
				if lastHex == nil {
					lastHex = tk.hex
				} else {
					if n := len(lastHex); n >= 1 && n <= 4 {
						codeBytes = n
					}
					lastHex = nil
				}
			}
		case "bfchar":
			if tk.kind == 1 {
				if lastHex == nil {
					lastHex = tk.hex
				} else {
					m[beU32(lastHex)] = utf16beString(tk.hex)
					lastHex = nil
				}
			}
		case "bfrange":
			group = append(group, tk)
			if len(group) == 3 {
				if group[0].kind == 1 && group[1].kind == 1 {
					lo, hi := beU32(group[0].hex), beU32(group[1].hex)
					if group[2].kind == 1 {
						for c := lo; c <= hi && c-lo < 65536; c++ {
							m[c] = utf16beString(addToUTF16(group[2].hex, c-lo))
						}
					} else {
						for k, dst := range group[2].arr {
							if lo+uint32(k) > hi {
								break
							}
							m[lo+uint32(k)] = utf16beString(dst)
						}
					}
				}
				group = nil
			}
		}
	}
	return codeBytes, m
}

// ---------------------------------------------------------------- 内容流

type cToken struct {
	kind pdfKind
	str  []byte
	num  float64
	name string
	arr  []cToken
}

func contentTokens(b []byte) []cToken {
	toks, _ := scanContentTokens(b, 0, false)
	return toks
}

func scanContentTokens(b []byte, i int, inArray bool) ([]cToken, int) {
	var out []cToken
	for i < len(b) {
		c := b[i]
		if isPDFWS(c) {
			i++
			continue
		}
		if c == '%' {
			for i < len(b) && b[i] != '\n' && b[i] != '\r' {
				i++
			}
			continue
		}
		switch c {
		case '(':
			p := &pdfParser{b: b, i: i}
			v, _ := p.literalString()
			i = p.i
			out = append(out, cToken{kind: kStr, str: v.str})
		case '<':
			if i+1 < len(b) && b[i+1] == '<' {
				i = skipDictBytes(b, i)
				continue
			}
			p := &pdfParser{b: b, i: i}
			v, _ := p.hexString()
			i = p.i
			out = append(out, cToken{kind: kStr, str: v.str})
		case '[':
			sub, ni := scanContentTokens(b, i+1, true)
			i = ni
			out = append(out, cToken{kind: kArr, arr: sub})
		case ']':
			if inArray {
				return out, i + 1
			}
			i++
		case '>':
			if i+1 < len(b) && b[i+1] == '>' {
				i += 2
				continue
			}
			i++
		case '/':
			p := &pdfParser{b: b, i: i}
			v, _ := p.nameValue()
			i = p.i
			out = append(out, cToken{kind: kName, name: v.name})
		case '{', '}':
			i++
		default:
			if c == '+' || c == '-' || c == '.' || isDigit(c) {
				p := &pdfParser{b: b, i: i}
				v, err := p.numberOrRef()
				if err != nil {
					i++
					continue
				}
				i = p.i
				out = append(out, cToken{kind: kNum, num: v.num})
				continue
			}
			start := i
			for i < len(b) && !isDelimOrWS(b[i]) {
				i++
			}
			w := string(b[start:i])
			if w == "" {
				i++
				continue
			}
			if w == "BI" { // 内联图片：数据里可能有任意字节，整体跳过
				i = skipInlineImage(b, i)
				continue
			}
			out = append(out, cToken{kind: kOp, name: w})
		}
	}
	return out, i
}

// skipInlineImage 从 "BI" 之后跳到 "EI"（内联图片的二进制数据可能含 ()<> 等字符）。
func skipInlineImage(b []byte, i int) int {
	for j := i; j+1 < len(b); j++ {
		if b[j] == 'E' && b[j+1] == 'I' && j > 0 && isPDFWS(b[j-1]) &&
			(j+2 >= len(b) || isDelimOrWS(b[j+2])) {
			return j + 2
		}
	}
	return len(b)
}

type textExtractor struct {
	fonts     map[string]*pdfFont
	cur       *pdfFont
	sb        strings.Builder
	last      byte
	pendingNL bool
	haveY     bool
	y         float64
	leading   float64
	unmapped  int
}

func (ex *textExtractor) run(toks []cToken) {
	var ops []cToken
	for _, tk := range toks {
		if tk.kind != kOp {
			ops = append(ops, tk)
			continue
		}
		ex.op(tk.name, ops)
		ops = ops[:0]
	}
}

func lastStr(ops []cToken) []byte {
	for i := len(ops) - 1; i >= 0; i-- {
		if ops[i].kind == kStr {
			return ops[i].str
		}
	}
	return nil
}

func (ex *textExtractor) op(name string, ops []cToken) {
	switch name {
	case "Tj":
		if s := lastStr(ops); s != nil {
			ex.show(s)
		}
	case "TJ":
		if len(ops) > 0 && ops[len(ops)-1].kind == kArr {
			for _, e := range ops[len(ops)-1].arr {
				switch e.kind {
				case kStr:
					ex.show(e.str)
				case kNum:
					// TJ 里的负数表示字间距拉开；足够大就当词间空格
					if e.num <= -200 {
						ex.space()
					}
				}
			}
		}
	case "'", "\"":
		ex.newline()
		if s := lastStr(ops); s != nil {
			ex.show(s)
		}
	case "Tf":
		for i := len(ops) - 1; i >= 0; i-- {
			if ops[i].kind == kName {
				ex.cur = ex.fonts[ops[i].name]
				break
			}
		}
	case "Td", "TD":
		if len(ops) >= 2 {
			ty := ops[len(ops)-1].num
			if ty != 0 {
				ex.newline()
			}
			ex.y += ty
			ex.haveY = true
			if name == "TD" {
				ex.leading = -ty
			}
		}
	case "T*":
		ex.newline()
	case "Tm":
		if len(ops) >= 6 {
			y := ops[len(ops)-1].num
			if ex.haveY && math.Abs(y-ex.y) > 0.5 {
				ex.newline()
			}
			ex.y = y
			ex.haveY = true
		}
	case "TL":
		if len(ops) >= 1 {
			ex.leading = ops[len(ops)-1].num
		}
	}
}

func (ex *textExtractor) show(b []byte) {
	var s string
	miss := 0
	if ex.cur != nil {
		s, miss = ex.cur.decode(b)
	} else {
		s = latin1String(b)
	}
	ex.unmapped += miss
	if s == "" {
		return
	}
	if ex.pendingNL && ex.sb.Len() > 0 {
		ex.sb.WriteByte('\n')
		ex.last = '\n'
	}
	ex.pendingNL = false
	ex.sb.WriteString(s)
	ex.last = s[len(s)-1]
}

func (ex *textExtractor) newline() {
	if ex.sb.Len() > 0 {
		ex.pendingNL = true
	}
}

func (ex *textExtractor) space() {
	if ex.sb.Len() > 0 && ex.last != ' ' && ex.last != '\n' {
		ex.sb.WriteByte(' ')
		ex.last = ' '
	}
}

func (ex *textExtractor) text() string {
	return strings.TrimSpace(ex.sb.String())
}
