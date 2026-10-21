package config

import "testing"

// public_origin 是反代部署的同源基准，必须严格到「一个源」：
// 方案、主机、端口之外不允许带路径/查询/用户信息。
func TestPublicOrigin_规范化合法值(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", ""},
		{"https://GW.Example.com", "https://gw.example.com"},
		{"http://127.0.0.1:8666", "http://127.0.0.1:8666"},
		{" https://gw.example.com/ ", "https://gw.example.com"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			c, err := Parse([]byte(`{"listen":"127.0.0.1:8666","public_origin":"` + tc.in + `"}`))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if c.PublicOrigin != tc.want {
				t.Fatalf("PublicOrigin = %q, want %q", c.PublicOrigin, tc.want)
			}
		})
	}
}

func TestPublicOrigin_非法值被拒(t *testing.T) {
	for _, origin := range []string{
		"ftp://gw.example.com",
		"gw.example.com",
		"https:///missing-host",
		"https://user:pass@gw.example.com",
		"https://gw.example.com/admin",
		"https://gw.example.com?x=1",
		"https://gw.example.com#frag",
	} {
		t.Run(origin, func(t *testing.T) {
			if _, err := Parse([]byte(`{"listen":"127.0.0.1:8666","public_origin":"` + origin + `"}`)); err == nil {
				t.Fatalf("public_origin=%q 应被拒绝", origin)
			}
		})
	}
}
