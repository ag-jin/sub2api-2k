package apikeygen

import (
	"math/big"
	"strings"
	"testing"
)

func TestGenerate_FormatAndLength(t *testing.T) {
	key, err := Generate("")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// sk- + R + 43 + 6 = 3 + 50 = 53
	if want := len(DefaultPrefix) + 1 + randomLen + checksumLen; len(key) != want {
		t.Fatalf("len = %d, want %d (%q)", len(key), want, key)
	}
	if !strings.HasPrefix(key, "sk-R") {
		t.Fatalf("expected sk-R prefix, got %q", key)
	}
	if !Validate(key) {
		t.Fatalf("freshly generated key failed Validate: %q", key)
	}
	if !IsNewFormat(key) {
		t.Fatalf("freshly generated key not recognized as new format: %q", key)
	}
}

func TestGenerate_CustomPrefix(t *testing.T) {
	key, err := Generate("mk-")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(key, "mk-R") {
		t.Fatalf("custom prefix not honored: %q", key)
	}
	// 非 sk- 前缀超出新版校验锚点范畴,Validate/IsNewFormat 应为 false。
	if Validate(key) {
		t.Errorf("custom-prefix key should not validate as sk- new format: %q", key)
	}
	if IsNewFormat(key) {
		t.Errorf("custom-prefix key should not be IsNewFormat: %q", key)
	}
}

func TestGenerate_Uniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 2000)
	for i := 0; i < 2000; i++ {
		key, err := Generate("")
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, dup := seen[key]; dup {
			t.Fatalf("duplicate key generated: %q", key)
		}
		seen[key] = struct{}{}
	}
}

func TestValidate_TamperedChecksum(t *testing.T) {
	key, err := Generate("")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// 翻动最后一位校验和字符 -> 必须 Validate 失败。
	b := []byte(key)
	last := b[len(b)-1]
	if last == '0' {
		b[len(b)-1] = '1'
	} else {
		b[len(b)-1] = '0'
	}
	if Validate(string(b)) {
		t.Fatalf("tampered checksum should fail Validate: %q", string(b))
	}
}

func TestValidate_TamperedRandomBody(t *testing.T) {
	key, err := Generate("")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// 翻动随机段中部某字符 -> 校验和不再匹配 -> Validate 失败。
	b := []byte(key)
	idx := len(DefaultPrefix) + 1 + randomLen/2 // 落在随机段内
	if b[idx] == 'a' {
		b[idx] = 'b'
	} else {
		b[idx] = 'a'
	}
	if Validate(string(b)) {
		t.Fatalf("tampered body should fail Validate: %q", string(b))
	}
}

func TestValidate_RejectsLegacyAndJunk(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"legacy hex", "sk-" + strings.Repeat("a", 64)},
		{"empty", ""},
		{"prefix only", "sk-"},
		{"sk-R but too short", "sk-R" + strings.Repeat("A", 10)},
		{"wrong version letter S", "sk-S" + strings.Repeat("A", randomLen+checksumLen)},
		{"no sk prefix", "R" + strings.Repeat("A", randomLen+checksumLen)},
		{"non-base62 in body", "sk-R" + strings.Repeat("-", randomLen) + strings.Repeat("A", checksumLen)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if Validate(c.key) {
				t.Errorf("Validate(%q) = true, want false", c.key)
			}
		})
	}
}

func TestIsNewFormat(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"sk-R8Kq2mXvB9wYpZ3nT7jL5dHfA1cG6sUxQ4eN0Hn4Tz6", true}, // sk-R 主体
		{"sk-" + strings.Repeat("a", 64), false},                 // 旧版 hex(小写)
		{"sk-r" + strings.Repeat("A", 49), false},                // 小写 r != 版本位 R
		{"sk-", false},
		{"", false},
		{"mk-R" + strings.Repeat("A", 49), false}, // 非 sk- 前缀
	}
	for _, c := range cases {
		if got := IsNewFormat(c.key); got != c.want {
			t.Errorf("IsNewFormat(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

func TestEncodeBase62Fixed_Width(t *testing.T) {
	// 0 必须左填充到定长全 '0'。
	if got := encodeBase62Fixed(big.NewInt(0), checksumLen); got != strings.Repeat("0", checksumLen) {
		t.Errorf("encode(0) = %q, want %q", got, strings.Repeat("0", checksumLen))
	}
	// 已知小值:62 -> "10" 末两位,定长左填充。
	if got := encodeBase62Fixed(big.NewInt(62), checksumLen); got != "000010" {
		t.Errorf("encode(62) = %q, want %q", got, "000010")
	}
	if got := encodeBase62Fixed(big.NewInt(61), checksumLen); got != "00000z" {
		t.Errorf("encode(61) = %q, want %q", got, "00000z")
	}
}
