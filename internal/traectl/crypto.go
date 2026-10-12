// Package traectl 实现 Trae CN / TRAE SOLO CN 的协议层：
// 凭据存储（tc 信封加解密）、token 续期（ECDSA P-256 DeviceProof）、
// 模型目录读取。
//
// 协议事实全部来自本机客户端逆向 + 实测验证（见仓库根的
// Trae2Api-阶段一验证报告.md），非猜测。
package traectl

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnvelopeType 是 tc 信封的加密类型（由 6 字节头标识）。
type EnvelopeType int

const (
	// EnvAES 头为 ASCII "tc" + 05 10 00 00，盐 = SALT_A ^ SALT_B。
	// 本机 Trae CN 与 TRAE SOLO CN 的主凭据均为此类型。
	EnvAES EnvelopeType = iota
	// EnvAESPrivate 头为 12 39 20 20 02 03（或 12 39 12 20 02 03），
	// 盐 = SALT_C ^ SALT_D。bluechonk 实测第 3 字节有 0x20/0x12 两个变体。
	EnvAESPrivate
)

func (t EnvelopeType) String() string {
	if t == EnvAESPrivate {
		return "AES_PRIVATE"
	}
	return "AES"
}

// 头部字节。EnvAESPrivate 的第二个变体（0x12 39 0x12 0x20 0x02 0x03）
// 与官方 JS 中的字面量不同，但被实测证实存在，故两个都接受。
var (
	headerAES      = []byte{0x74, 0x63, 0x05, 0x10, 0x00, 0x00} // "tc"
	headerAESPrivA = []byte{0x12, 0x39, 0x20, 0x20, 0x02, 0x03}
	headerAESPrivB = []byte{0x12, 0x39, 0x12, 0x20, 0x02, 0x03}
)

const (
	// headerLen 信封头长度。
	headerLen = 6
	// randLen 随机盐长度（KDF 输入）。
	randLen = 32
	// checksumLen 明文前缀的 SHA-512 校验和长度。
	checksumLen = sha512.Size // 64
)

// 四张 64 字节盐表，硬编码于 Trae 客户端 bundle（out/main.js）中。
//
// 安全评估：该加密**不依赖 DPAPI 或任何机器绑定密钥**，唯一秘密就是这四张表。
// 也就是说拿到 storage.json + main.js 即可离线解密 —— 它只提供混淆，不提供
// 真正的机密性。盐表可通过环境变量覆盖（TRAE_SALT_A..D，逗号分隔的 64 个十进制字节），
// 以便上游改表时无需重新编译即可热修。
var (
	defaultSaltA = []byte{
		82, 9, 106, 213, 48, 54, 165, 56, 191, 64, 163, 158, 129, 243, 215, 251,
		124, 227, 57, 130, 155, 47, 255, 135, 52, 142, 67, 68, 196, 222, 233, 203,
		84, 123, 148, 50, 166, 194, 35, 61, 238, 76, 149, 11, 66, 250, 195, 78,
		8, 46, 161, 102, 40, 217, 36, 178, 118, 91, 162, 73, 109, 139, 209, 37,
	}
	defaultSaltB = []byte{
		31, 221, 168, 51, 136, 7, 199, 49, 177, 18, 16, 89, 39, 128, 236, 95,
		96, 81, 127, 169, 25, 181, 74, 13, 45, 229, 122, 159, 147, 201, 156, 239,
		160, 224, 59, 77, 174, 42, 245, 176, 200, 235, 187, 60, 131, 83, 153, 97,
		23, 43, 4, 126, 186, 119, 214, 38, 225, 105, 20, 99, 85, 33, 12, 125,
	}
	defaultSaltC = []byte{
		191, 192, 216, 250, 122, 246, 220, 97, 31, 254, 98, 27, 8, 72, 71, 176,
		135, 99, 96, 18, 127, 101, 203, 104, 211, 102, 191, 125, 37, 72, 150, 156,
		51, 229, 121, 35, 17, 153, 141, 177, 110, 131, 150, 128, 172, 255, 254, 6,
		18, 140, 55, 62, 236, 249, 135, 64, 135, 12, 117, 4, 89, 149, 168, 209,
	}
	defaultSaltD = []byte{
		246, 204, 26, 232, 232, 70, 129, 109, 223, 146, 169, 242, 23, 241, 105, 145,
		50, 196, 165, 42, 254, 120, 3, 54, 244, 207, 209, 85, 53, 6, 138, 106,
		175, 148, 31, 204, 186, 186, 165, 182, 87, 142, 49, 10, 39, 110, 26, 154,
		86, 56, 173, 125, 18, 64, 198, 225, 99, 99, 83, 82, 191, 134, 76, 170,
	}
)

// ErrNotTCEnvelope 表示输入不是 tc 信封（可能是明文 JSON，如 Trae 国际版）。
var ErrNotTCEnvelope = errors.New("traectl: 不是 tc 信封（未知头部）")

// saltFromEnv 读取 TRAE_SALT_<name> 覆盖值；非法或缺失时回落 default。
func saltFromEnv(name string, def []byte) []byte {
	v := strings.TrimSpace(os.Getenv("TRAE_SALT_" + name))
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	if len(parts) != 64 {
		return def
	}
	out := make([]byte, 64)
	for i, p := range parts {
		n := 0
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%d", &n); err != nil || n < 0 || n > 255 {
			return def
		}
		out[i] = byte(n)
	}
	return out
}

// xor64 逐字节异或两张 64 字节盐表。
func xor64(a, b []byte) []byte {
	out := make([]byte, 64)
	for i := 0; i < 64; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// saltFor 返回该信封类型对应的盐。
func saltFor(t EnvelopeType) []byte {
	a := saltFromEnv("A", defaultSaltA)
	b := saltFromEnv("B", defaultSaltB)
	c := saltFromEnv("C", defaultSaltC)
	d := saltFromEnv("D", defaultSaltD)
	if t == EnvAESPrivate {
		return xor64(c, d)
	}
	return xor64(a, b)
}

// detectEnvelope 依据 6 字节头判定信封类型。
func detectEnvelope(header []byte) (EnvelopeType, error) {
	switch {
	case bytes.Equal(header, headerAES):
		return EnvAES, nil
	case bytes.Equal(header, headerAESPrivA), bytes.Equal(header, headerAESPrivB):
		return EnvAESPrivate, nil
	}
	return 0, ErrNotTCEnvelope
}

// deriveKeyIV 派生 AES-128 的 key 与 IV：
//
//	h1 = SHA512(random32)
//	h2 = SHA512(h1 ‖ salt)   // salt 为 64 字节
//	key = h2[0:16], iv = h2[16:32]
func deriveKeyIV(random32, salt []byte) (key, iv []byte) {
	h1 := sha512.Sum512(random32)
	h := sha512.New()
	h.Write(h1[:])
	h.Write(salt)
	h2 := h.Sum(nil)
	return h2[0:16], h2[16:32]
}

// DecryptTC 解开 tc 信封（base64 字符串），返回明文 JSON 字节。
//
// 信封结构：[6B 头][32B 随机盐][N 字节 AES-128-CBC 密文]
// 明文结构：[64B SHA-512 校验和][JSON]
//
// 校验和不匹配时返回错误 —— 这既防篡改，也能在盐表变更时给出明确信号
// （而不是返回一段乱码让上层去 JSON 解析失败）。
func DecryptTC(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("traectl: base64 解码失败: %w", err)
	}
	return DecryptTCBytes(raw)
}

// DecryptTCBytes 同 DecryptTC，但输入是已解码的原始字节。
func DecryptTCBytes(raw []byte) ([]byte, error) {
	if len(raw) < headerLen+randLen+aes.BlockSize {
		return nil, fmt.Errorf("traectl: 信封过短 (%d 字节)", len(raw))
	}
	envType, err := detectEnvelope(raw[:headerLen])
	if err != nil {
		return nil, err
	}
	random32 := raw[headerLen : headerLen+randLen]
	ciphertext := raw[headerLen+randLen:]
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("traectl: 密文长度 %d 不是 AES 块大小的整数倍", len(ciphertext))
	}

	key, iv := deriveKeyIV(random32, saltFor(envType))
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("traectl: 构造 AES: %w", err)
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ciphertext)

	// 必须先去掉 PKCS#7 填充再切分校验和。
	// 对照实现（Node）用 decipher.final() 自动去填充；Go 的 CryptBlocks 不会，
	// 若把填充字节算进明文，SHA-512 必然校验失败。
	plain, err = pkcs7Unpad(plain, aes.BlockSize)
	if err != nil {
		return nil, fmt.Errorf("traectl: 去填充失败: %w", err)
	}

	if len(plain) < checksumLen {
		return nil, fmt.Errorf("traectl: 明文过短 (%d 字节)", len(plain))
	}
	stored := plain[:checksumLen]
	body := plain[checksumLen:]
	calc := sha512.Sum512(body)
	if !bytes.Equal(stored, calc[:]) {
		return nil, fmt.Errorf("traectl: SHA-512 校验失败（盐表可能已变更，可用 TRAE_SALT_A..D 覆盖）")
	}
	return body, nil
}

// EncryptTC 按 tc 信封加密（写回 storage.json 用）。
//
// 每次调用生成新的 32 字节随机盐，故同一明文每次密文都不同 —— 这是格式本身的性质。
func EncryptTC(plain []byte, t EnvelopeType) (string, error) {
	random32 := make([]byte, randLen)
	if _, err := rand.Read(random32); err != nil {
		return "", fmt.Errorf("traectl: 生成随机盐: %w", err)
	}
	sum := sha512.Sum512(plain)
	buf := make([]byte, 0, checksumLen+len(plain))
	buf = append(buf, sum[:]...)
	buf = append(buf, plain...)

	key, iv := deriveKeyIV(random32, saltFor(t))
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("traectl: 构造 AES: %w", err)
	}
	// 明文长度 = 64 + len(plain)；PKCS#7 由 CryptBlocks 手工补齐。
	padded := pkcs7Pad(buf, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)

	header := headerAES
	if t == EnvAESPrivate {
		header = headerAESPrivA
	}
	env := make([]byte, 0, headerLen+randLen+len(out))
	env = append(env, header...)
	env = append(env, random32...)
	env = append(env, out...)
	return base64.StdEncoding.EncodeToString(env), nil
}

// pkcs7Pad 补齐到 blockSize 的整数倍。
func pkcs7Pad(b []byte, blockSize int) []byte {
	n := blockSize - len(b)%blockSize
	out := make([]byte, len(b)+n)
	copy(out, b)
	for i := len(b); i < len(out); i++ {
		out[i] = byte(n)
	}
	return out
}

// pkcs7Unpad 去掉 PKCS#7 填充，并校验填充合法性（防伪造/防错位）。
func pkcs7Unpad(b []byte, blockSize int) ([]byte, error) {
	if len(b) == 0 || len(b)%blockSize != 0 {
		return nil, fmt.Errorf("长度 %d 不是 %d 的整数倍", len(b), blockSize)
	}
	n := int(b[len(b)-1])
	if n == 0 || n > blockSize || n > len(b) {
		return nil, fmt.Errorf("非法填充长度 %d", n)
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, fmt.Errorf("填充字节不一致")
		}
	}
	return b[:len(b)-n], nil
}
