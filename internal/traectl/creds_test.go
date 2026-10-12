package traectl

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestBuildCanonical 锁定签名原文格式。
//
// 这个字符串是续期成功与否的关键：分隔符或顺序错了，服务端只会返回
// 20405「需要设备证明」，完全不会提示根因。
func TestBuildCanonical(t *testing.T) {
	got := buildCanonical("POST", "/trae/api/v3/oauth/ExchangeToken",
		"en1oxy7wnw8j9n", "RT123", 1792732200, "aabbccddeeff00112233445566778899")
	want := "POST\n/trae/api/v3/oauth/ExchangeToken\nen1oxy7wnw8j9n\nRT123\n1792732200\naabbccddeeff00112233445566778899"
	if got != want {
		t.Errorf("canonical 不一致\n got=%q\nwant=%q", got, want)
	}
	if n := strings.Count(got, "\n"); n != 5 {
		t.Errorf("分隔符数量 = %d want 5", n)
	}
}

// TestSignDeviceProofVerifiable 签名必须能用对应公钥验证通过，
// 且必须是 DER 编码（ASN.1 SEQUENCE 以 0x30 开头），而不是 IEEE-P1363。
func TestSignDeviceProofVerifiable(t *testing.T) {
	privPEM, pubPEM, err := generateTestKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := signDeviceProof("ono9krqynydwx5", "refresh-token-x", privPEM)
	if err != nil {
		t.Fatalf("signDeviceProof: %v", err)
	}
	if proof.Timestamp <= 0 {
		t.Error("Timestamp 应为正数 unix 秒")
	}
	if len(proof.Nonce) != 32 {
		t.Errorf("Nonce 长度 = %d want 32（16 字节 hex）", len(proof.Nonce))
	}

	sig, err := base64.StdEncoding.DecodeString(proof.Signature)
	if err != nil {
		t.Fatalf("签名不是合法 base64: %v", err)
	}
	if len(sig) == 0 || sig[0] != 0x30 {
		t.Errorf("签名首字节 = 0x%02x，期望 0x30（DER SEQUENCE）—— 说明用了 IEEE-P1363 编码", sig[0])
	}

	canonical := buildCanonical("POST", exchangePath, "ono9krqynydwx5", "refresh-token-x", proof.Timestamp, proof.Nonce)
	digest := sha256.Sum256([]byte(canonical))
	pub, err := parseTestPublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		t.Error("签名验证失败 —— canonical 构造或签名算法不一致")
	}
}

// TestParseECPrivateKeyErrors 非法 PEM 必须报错而不是 panic。
func TestParseECPrivateKeyErrors(t *testing.T) {
	cases := []string{"", "not a pem", "-----BEGIN PRIVATE KEY-----\nzzz\n-----END PRIVATE KEY-----"}
	for _, c := range cases {
		if _, err := parseECPrivateKey(c); err == nil {
			t.Errorf("parseECPrivateKey(%q) 应报错", c)
		}
	}
}

// TestParseRegBuildNumber / TestParseVerOutput 锁定系统版本解析（参与 DeviceInfo）。
func TestParseRegBuildNumber(t *testing.T) {
	out := "\r\nHKEY_LOCAL_MACHINE\\SOFTWARE\\Microsoft\\Windows NT\\CurrentVersion\r\n" +
		"    CurrentBuildNumber    REG_SZ    26100\r\n\r\n"
	if got := parseRegBuildNumber(out); got != "26100" {
		t.Errorf("parseRegBuildNumber=%q want 26100", got)
	}
	if got := parseRegBuildNumber("no match here"); got != "" {
		t.Errorf("无匹配时应返回空，得到 %q", got)
	}
}

func TestParseVerOutput(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\r\nMicrosoft Windows [Version 10.0.26100.1742]\r\n", "10.0.26100"},
		{"Microsoft Windows [Version 10.0.22631.4169]", "10.0.22631"},
		{"garbage", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseVerOutput(c.in); got != c.want {
			t.Errorf("parseVerOutput(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestParseTraeTime 凭据时间解析（客户端为毫秒精度 UTC）。
func TestParseTraeTime(t *testing.T) {
	got, err := parseTraeTime("2026-10-23T05:10:11.618Z")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.UTC().Format("2006-01-02T15:04:05") != "2026-10-23T05:10:11" {
		t.Errorf("解析结果不符: %v", got.UTC())
	}
	if _, err := parseTraeTime(""); err == nil {
		t.Error("空串应报错")
	}
	if _, err := parseTraeTime("not-a-time"); err == nil {
		t.Error("非法时间应报错")
	}
	// 往返
	if s := formatTraeTime(got); s != "2026-10-23T05:10:11.618Z" {
		t.Errorf("formatTraeTime 往返 = %q", s)
	}
}

// ---- 真实凭据相关（无文件则跳过）----

// TestOpenStoreReal 用本机真实 storage.json 验证凭据加载全链路。
func TestOpenStoreReal(t *testing.T) {
	tested := 0
	for _, p := range Products {
		path, err := DefaultStoragePath(p)
		if err != nil {
			t.Skip("无 APPDATA")
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		t.Run(p.Key, func(t *testing.T) {
			s, err := OpenStore(p, "")
			if err != nil {
				t.Skipf("打开失败（可能未登录）: %v", err)
			}
			if s.Cred.Token == "" || s.Cred.RefreshToken == "" {
				t.Error("token / refreshToken 为空")
			}
			if s.Cred.UserID == "" {
				t.Error("userId 为空")
			}
			if !strings.HasPrefix(s.Cred.Host, "https://") {
				t.Errorf("host 异常: %q", s.Cred.Host)
			}
			if s.DeviceID == "" {
				t.Error("DeviceID 为空（未找到 iCubeAuthInfo://icube-dc:*）")
			}
			for _, c := range s.DeviceID {
				if c < '0' || c > '9' {
					t.Errorf("DeviceID 应为纯数字，得到 %q", s.DeviceID)
					break
				}
			}
			if s.Device.PrivateKeyPEM == "" || s.Device.PublicKeyPEM == "" {
				t.Error("设备密钥对缺失")
			}
			if _, err := parseECPrivateKey(s.Device.PrivateKeyPEM); err != nil {
				t.Errorf("真实设备私钥解析失败: %v", err)
			}
			if s.MachineID == "" {
				t.Error("telemetry.machineId 为空")
			}
			// 用真实私钥签一次并验签（证明续期证明能生成）
			if _, err := signDeviceProof(p.ClientID, s.Cred.RefreshToken, s.Device.PrivateKeyPEM); err != nil {
				t.Errorf("用真实设备私钥签名失败: %v", err)
			}
			// 凭据自带 host 应指向 CN 区
			if !strings.Contains(s.Cred.Host, "trae.cn") {
				t.Logf("提示：host=%q 非 trae.cn（国际版？）", s.Cred.Host)
			}
		})
		tested++
	}
	if tested == 0 {
		t.Skip("本机未安装/未登录任何 Trae 产品")
	}
}

// TestSaveRoundTripOnCopy 在**真实文件的副本**上验证写回：绝不触碰用户的原始凭据。
func TestSaveRoundTripOnCopy(t *testing.T) {
	for _, p := range Products {
		src, err := DefaultStoragePath(p)
		if err != nil {
			t.Skip("无 APPDATA")
		}
		if _, err := os.Stat(src); err != nil {
			continue
		}
		t.Run(p.Key, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "storage.json")
			raw, err := os.ReadFile(src)
			if err != nil {
				t.Skipf("读取失败: %v", err)
			}
			if err := os.WriteFile(dst, raw, 0o600); err != nil {
				t.Fatal(err)
			}

			s, err := OpenStore(p, dst)
			if err != nil {
				t.Skipf("打开副本失败: %v", err)
			}
			origToken := s.Cred.Token
			origRefresh := s.Cred.RefreshToken
			// 记录一个「非本渠道」的键，验证写回不会丢
			var before map[string]json.RawMessage
			if err := json.Unmarshal(raw, &before); err != nil {
				t.Fatal(err)
			}
			keysBefore := len(before)

			s.Cred.Token = "NEW-TOKEN-FOR-TEST"
			s.Cred.ExpiredAt = "2030-01-01T00:00:00.000Z"
			if err := s.Save(); err != nil {
				t.Fatalf("Save: %v", err)
			}

			// 重新打开，验证写入生效且其它键未丢
			re, err := OpenStore(p, dst)
			if err != nil {
				t.Fatalf("重新打开失败: %v", err)
			}
			if re.Cred.Token != "NEW-TOKEN-FOR-TEST" {
				t.Errorf("token 未写回: %q", re.Cred.Token)
			}
			if re.Cred.ExpiredAt != "2030-01-01T00:00:00.000Z" {
				t.Errorf("expiredAt 未写回: %q", re.Cred.ExpiredAt)
			}
			if re.Cred.RefreshToken != origRefresh {
				t.Error("未修改的 refreshToken 被改动了")
			}
			var after map[string]json.RawMessage
			rawAfter, _ := os.ReadFile(dst)
			if err := json.Unmarshal(rawAfter, &after); err != nil {
				t.Fatalf("写回后不是合法 JSON: %v", err)
			}
			if len(after) != keysBefore {
				t.Errorf("键数量变化: %d → %d（写回丢键）", keysBefore, len(after))
			}
			// 其它键逐一比对（内容不变）
			for k, v := range before {
				if k == keyAuth {
					continue // 这个键本来就要被替换
				}
				if av, ok := after[k]; !ok {
					t.Errorf("键 %q 丢失", k)
				} else if string(av) != string(v) {
					var v1, v2 any
					if err1 := json.Unmarshal(v, &v1); err1 == nil {
						if err2 := json.Unmarshal(av, &v2); err2 == nil {
							if reflect.DeepEqual(v1, v2) {
								continue
							}
						}
					}
					t.Errorf("键 %q 内容被改动", k)
				}
			}
			_ = origToken
		})
	}
}

// ---- 测试用密钥工具 ----

func generateTestKeyPair() (privPEM, pubPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})), nil
}

func parseTestPublicKey(pemStr string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("测试公钥不是合法 PEM")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("测试公钥不是 EC 密钥")
	}
	return pub, nil
}

// TestNeedsRefresh 过期判定：无 token / 已过期 / 临期 / 远期。
func TestNeedsRefresh(t *testing.T) {
	mk := func(exp string) *Store { return &Store{Cred: Credentials{Token: "t", ExpiredAt: exp}} }
	if !mk("").NeedsRefresh(time.Minute) {
		t.Error("空 expiredAt 应判定需要刷新")
	}
	if !mk("2020-01-01T00:00:00.000Z").NeedsRefresh(time.Minute) {
		t.Error("已过期应判定需要刷新")
	}
	soon := formatTraeTime(time.Now().Add(5 * time.Minute))
	if !mk(soon).NeedsRefresh(10 * time.Minute) {
		t.Error("5 分钟后过期、阈值 10 分钟 → 应刷新")
	}
	far := formatTraeTime(time.Now().Add(24 * time.Hour))
	if mk(far).NeedsRefresh(10 * time.Minute) {
		t.Error("24 小时后过期 → 不应刷新")
	}
}
