package traectl

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnvelopeRoundTrip 加解密往返：两种信封类型都要自洽。
func TestEnvelopeRoundTrip(t *testing.T) {
	plain := []byte(`{"token":"x","userId":"916348159159136","note":"往返自检 —— 含中文与 emoji 🚀"}`)
	for _, et := range []EnvelopeType{EnvAES, EnvAESPrivate} {
		enc, err := EncryptTC(plain, et)
		if err != nil {
			t.Fatalf("%v: EncryptTC: %v", et, err)
		}
		got, err := DecryptTC(enc)
		if err != nil {
			t.Fatalf("%v: DecryptTC: %v", et, err)
		}
		if string(got) != string(plain) {
			t.Errorf("%v: 往返不一致\n got=%q\nwant=%q", et, got, plain)
		}
	}
}

// TestEncryptRandomized 同一明文两次加密应产生不同密文（随机盐），但都能解开。
func TestEncryptRandomized(t *testing.T) {
	plain := []byte(`{"a":1}`)
	e1, err := EncryptTC(plain, EnvAES)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := EncryptTC(plain, EnvAES)
	if err != nil {
		t.Fatal(err)
	}
	if e1 == e2 {
		t.Error("两次加密结果相同 —— 随机盐未生效")
	}
	for _, e := range []string{e1, e2} {
		if got, err := DecryptTC(e); err != nil || string(got) != string(plain) {
			t.Errorf("解密失败或内容不符: err=%v got=%q", err, got)
		}
	}
}

// TestDetectEnvelope 头部判定：两个 AES_PRIVATE 变体都必须接受。
func TestDetectEnvelope(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want EnvelopeType
		ok   bool
	}{
		{"AES(tc)", headerAES, EnvAES, true},
		{"AES_PRIVATE 变体1", headerAESPrivA, EnvAESPrivate, true},
		{"AES_PRIVATE 变体2(bluechonk 实测)", headerAESPrivB, EnvAESPrivate, true},
		{"未知头", []byte{0, 1, 2, 3, 4, 5}, 0, false},
		{"全零", make([]byte, 6), 0, false},
	}
	for _, c := range cases {
		got, err := detectEnvelope(c.in)
		if c.ok && err != nil {
			t.Errorf("%s: 期望成功，得到 %v", c.name, err)
			continue
		}
		if !c.ok {
			if err == nil {
				t.Errorf("%s: 期望报错，却返回 %v", c.name, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s: 得到 %v want %v", c.name, got, c.want)
		}
	}
}

// TestDecryptErrors 错误路径：坏 base64、过短、密文非块对齐、校验和不匹配。
func TestDecryptErrors(t *testing.T) {
	if _, err := DecryptTC("!!!not-base64!!!"); err == nil {
		t.Error("坏 base64 应报错")
	}
	if _, err := DecryptTCBytes([]byte{0x74, 0x63}); err == nil {
		t.Error("过短信封应报错")
	}
	// 构造合法头 + 随机盐 + 非块对齐密文
	bad := append(append([]byte{}, headerAES...), make([]byte, randLen)...)
	bad = append(bad, 1, 2, 3)
	if _, err := DecryptTCBytes(bad); err == nil {
		t.Error("密文非 AES 块对齐应报错")
	}
	// 合法加密后篡改最后一个字节 → 校验和必须失败
	enc, err := EncryptTC([]byte(`{"a":1}`), EnvAES)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(enc)
	raw[len(raw)-1] ^= 0xFF
	if _, err := DecryptTCBytes(raw); err == nil {
		t.Error("篡改密文后 SHA-512 校验应失败")
	}
}

// realStorageJSON 返回本机某 Trae 产品的 storage.json 路径；不存在则返回 ""。
func realStorageJSON(product string) string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return ""
	}
	p := filepath.Join(appData, product, "User", "globalStorage", "storage.json")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// TestDecryptRealCredentials 用本机真实凭据做端到端校验（无文件则跳过）。
//
// 这是本包最有价值的测试：它同时验证盐表、KDF、CBC、校验和四段全部正确。
// 断言只看结构与格式，**绝不输出任何 token / 密钥内容**。
func TestDecryptRealCredentials(t *testing.T) {
	products := []string{"Trae CN", "TRAE SOLO CN"}
	tested := 0
	for _, prod := range products {
		p := realStorageJSON(prod)
		if p == "" {
			continue
		}
		t.Run(prod, func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Skipf("读取 %s 失败: %v", p, err)
			}
			var st map[string]any
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatalf("storage.json 不是合法 JSON: %v", err)
			}
			enc, _ := st["iCubeAuthInfo://icube.cloudide"].(string)
			if enc == "" {
				t.Skip("未找到 iCubeAuthInfo://icube.cloudide（可能未登录）")
			}
			// 国际版可能是明文 JSON —— 本项目只处理 CN 的 tc 信封
			if strings.HasPrefix(strings.TrimSpace(enc), "{") {
				t.Skip("凭据为明文 JSON（国际版），非 tc 信封")
			}

			plain, err := DecryptTC(enc)
			if err != nil {
				t.Fatalf("解密失败: %v", err)
			}
			var cred map[string]any
			if err := json.Unmarshal(plain, &cred); err != nil {
				t.Fatalf("解密结果不是合法 JSON: %v", err)
			}

			// 结构断言（不打印值）
			for _, k := range []string{"token", "refreshToken", "userId", "host", "expiredAt"} {
				if v, ok := cred[k]; !ok || v == nil || v == "" {
					t.Errorf("解密结果缺少字段 %q", k)
				}
			}
			if host, _ := cred["host"].(string); !strings.HasPrefix(host, "https://") {
				t.Errorf("host 字段不是 https URL: %q", host)
			}
			if tok, _ := cred["token"].(string); len(tok) < 100 {
				t.Errorf("token 长度异常: %d（期望 >=100）", len(tok))
			}
			if rt, _ := cred["refreshToken"].(string); len(rt) < 20 {
				t.Errorf("refreshToken 长度异常: %d", len(rt))
			}
			// 加解密往返：把解出的明文重新封装后必须还能解回
			re, err := EncryptTC(plain, EnvAES)
			if err != nil {
				t.Fatalf("重新加密失败: %v", err)
			}
			back, err := DecryptTC(re)
			if err != nil || string(back) != string(plain) {
				t.Fatalf("真实凭据重新加密后解不回: err=%v", err)
			}
		})
		tested++
	}
	if tested == 0 {
		t.Skip("本机未找到 Trae CN / TRAE SOLO CN 的 storage.json")
	}
}
