// Package apikeygen 生成与校验「新版」API Key。
//
// 新版格式(全长 53 字符):
//
//	sk-  +  R  +  <43 位 base62 随机>  +  <6 位 base62 校验和>
//	└头┘    └版┘  └──── 256 bit 熵 ────┘  └─ CRC32(版本+随机) ─┘
//
// 设计取舍:
//   - 保留 sk- 头:OpenAI 及绝大多数客户端 SDK 对 "sk-" 开头硬兼容。
//   - 版本位 R(大写字母):旧版是 "sk-"+小写 hex(仅 0-9a-f),新版紧跟大写字母,
//     两者天然互斥 —— IsNewFormat 可零歧义区分新旧,认证链路无需改动。
//   - 仍明文存储:整串作为唯一明文落库,认证 GetByKey 直接整串查询,新旧通吃。
//   - 末尾校验和:CRC32(IEEE) 覆盖「版本位+随机段」,客户端 / secret scanner
//     可在发起请求前本地判断 key 是否完整(防截断/粘贴漏字),不参与服务端认证。
package apikeygen

import (
	"crypto/rand"
	"hash/crc32"
	"math/big"
	"strings"
)

const (
	// DefaultPrefix 是 sk- 头的兜底值,与历史保持一致。
	DefaultPrefix = "sk-"

	// Version 是当前新版的版本位。升版时改为 S/T… 即可与历史 key 共存。
	Version = 'R'

	randomLen   = 43 // base62(32 字节) 的定长编码宽度,约 256 bit 熵
	checksumLen = 6  // base62(CRC32) 的定长编码宽度,uint32 最多 6 位 base62

	randomBytes = 32
)

// base62Alphabet 顺序为 0-9A-Za-z,定长大端编码使用。
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var bigBase = big.NewInt(62)

// Generate 产出一个新版 API Key。prefix 为空时回退到 DefaultPrefix。
// 主体「版本位+随机+校验和」与 prefix 无关;但 Validate / IsNewFormat 以
// DefaultPrefix("sk-") 为锚点,故部署方若自定义了非 sk- 前缀,生成的 key 仍可用
// (认证只整串明文查库),只是无法被本包的新版校验识别 —— 中转站默认 sk-,不受影响。
func Generate(prefix string) (string, error) {
	if prefix == "" {
		prefix = DefaultPrefix
	}

	buf := make([]byte, randomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	random := encodeBase62Fixed(new(big.Int).SetBytes(buf), randomLen)
	body := string(Version) + random
	checksum := encodeChecksum(body)

	return prefix + body + checksum, nil
}

// IsNewFormat 判断 key 是否为新版格式(sk-R… 主体)。
// 不验证校验和,仅看结构特征;旧版 "sk-"+hex 因紧跟字符为小写 hex 而返回 false。
func IsNewFormat(key string) bool {
	body, ok := strings.CutPrefix(key, DefaultPrefix)
	if !ok {
		return false
	}
	return len(body) > 0 && rune(body[0]) == Version
}

// Validate 校验一个新版 key 的结构与校验和是否自洽。
// 返回 true 表示:sk-R 主体、长度正确、且末 6 位校验和与「版本位+随机段」匹配。
// 旧版 key、自定义格式 key 一律返回 false(调用方应据此回退到明文 DB 查询)。
func Validate(key string) bool {
	body, ok := strings.CutPrefix(key, DefaultPrefix)
	if !ok {
		return false
	}
	// body = 版本位(1) + 随机(randomLen) + 校验和(checksumLen)
	if len(body) != 1+randomLen+checksumLen {
		return false
	}
	if rune(body[0]) != Version {
		return false
	}
	signed := body[:1+randomLen]
	got := body[1+randomLen:]
	if !isBase62(signed) || !isBase62(got) {
		return false
	}
	return encodeChecksum(signed) == got
}

// encodeChecksum 计算 body 的 CRC32(IEEE) 并定长 base62 编码为 checksumLen 位。
func encodeChecksum(body string) string {
	sum := crc32.ChecksumIEEE([]byte(body))
	return encodeBase62Fixed(new(big.Int).SetUint64(uint64(sum)), checksumLen)
}

// encodeBase62Fixed 把 n 大端编码为恰好 width 位 base62,高位以 '0' 左填充。
// 前提:n 的实际 base62 位数 <= width(本包用法均满足:CRC32<=6 位、256bit<=43 位)。
func encodeBase62Fixed(n *big.Int, width int) string {
	out := make([]byte, width)
	v := new(big.Int).Set(n)
	mod := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		v.DivMod(v, bigBase, mod)
		out[i] = base62Alphabet[mod.Int64()]
	}
	return string(out)
}

func isBase62(s string) bool {
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(base62Alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}
