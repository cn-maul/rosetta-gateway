package userauth

import "testing"

func TestHashPassword_VerifyRoundTrip(t *testing.T) {
	const pw = "Str0ngPassw0rd!"
	enc, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !VerifyPassword(enc, pw) {
		t.Error("correct password rejected")
	}
	if VerifyPassword(enc, "wrong-password") {
		t.Error("wrong password accepted")
	}
}

// 每次哈希的盐不同 → 同一口令的两个编码串必须不同。
// 若相同，说明盐没生效，一次拖库就能撞出所有相同口令。
func TestHashPassword_SaltIsRandom(t *testing.T) {
	const pw = "Str0ngPassw0rd!"
	a, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	b, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical; salt is not random")
	}
	// 两个编码都要能验过同一口令
	for i, enc := range []string{a, b} {
		if !VerifyPassword(enc, pw) {
			t.Errorf("encoding %d failed to verify", i)
		}
	}
}

// 编码必须自描述 algo 与迭代数，将来调参才能解旧串。
func TestHashPassword_EncodingIsSelfDescribing(t *testing.T) {
	enc, err := HashPassword("Str0ngPassw0rd!")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	parts := splitEncoding(t, enc)
	if parts[0] != algoName {
		t.Errorf("algo = %q, want %q", parts[0], algoName)
	}
	// 第二段必须是可解析的正整数，且等于当前默认迭代数
	iter := parts[1]
	if iter == "" {
		t.Error("iteration count not stored in encoding")
	}
	if iter != "210000" {
		t.Errorf("stored iteration = %q, want the current default 210000", iter)
	}
	if len(parts) != 4 {
		t.Errorf("want 4 segments, got %d: %q", len(parts), enc)
	}
}

func splitEncoding(t *testing.T, enc string) []string {
	t.Helper()
	parts := make([]string, 0, 4)
	cur := ""
	for _, r := range enc {
		if r == '$' {
			parts = append(parts, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(parts, cur)
}

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		pw  string
		ok  bool
		why string
	}{
		{"Str0ngPassw0rd!", true, "强口令应通过"},
		{"abcdefgh", false, "8 位但只有小写 —— 单类字符不达标"},
		{"abcdef1", false, "7 位太短"},
		{"abcdefgh1", true, "小写 + 数字两类"},
		{"Abcdefgh", true, "大小写两类"},
		{"abcdefgh!", true, "小写 + 符号两类"},
		{"", false, "空串"},
		{"中文字符测试abc", true, "含中文 + 拉丁小写，算两类"},
	}
	for _, c := range cases {
		err := ValidatePassword(c.pw)
		if c.ok && err != nil {
			t.Errorf("%q: want pass, got %v (%s)", c.pw, err, c.why)
		}
		if !c.ok && err == nil {
			t.Errorf("%q: want fail, but passed (%s)", c.pw, c.why)
		}
	}
}

// 格式非法的编码必须一律拒绝，且不泄露「哪里错了」。
func TestVerifyPassword_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"not-an-encoding",
		"pbkdf2-sha256$abc$AAAA$AAAA",   // iter 非数字
		"pbkdf2-sha256$210000$!!!$AAAA", // salt 非 base64
		"pbkdf2-sha256$210000$AAAA$!!!", // hash 非 base64
		"bcrypt$210000$AAAA$AAAA",       // algo 不认
		"pbkdf2-sha256$210000$AAAA",     // 段数不足
		"pbkdf2-sha256$0$AAAA$AAAA",     // iter<=0
	}
	for _, enc := range cases {
		if VerifyPassword(enc, "anything") {
			t.Errorf("malformed encoding accepted: %q", enc)
		}
	}
	if VerifyPassword("", "x") || VerifyPassword("x", "") {
		t.Error("empty input must be rejected")
	}
}

// 迭代数随编码保存：低迭代的旧串仍可验证（不强制重算导致全量失效）。
func TestVerifyPassword_HonorsStoredIterations(t *testing.T) {
	// 手工构造一个低迭代编码
	enc, err := hashWithIterations("Str0ngPassw0rd!", 1000)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !VerifyPassword(enc, "Str0ngPassw0rd!") {
		t.Error("encoding with custom iteration count failed to verify")
	}
	if VerifyPassword(enc, "wrong") {
		t.Error("wrong password accepted")
	}
}
