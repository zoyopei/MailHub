package config

import "testing"

func setBase(t *testing.T) {
	t.Setenv("MAILHUB_API_TOKEN", "tok")
	t.Setenv("MAILHUB_SECRET_KEY", GenSecret())
}

func TestLoadDefaultsAndValidation(t *testing.T) {
	setBase(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.PollIntervalDefault != 60 || c.MaxMessageSize != 50<<20 {
		t.Fatalf("默认值异常: %+v", c)
	}

	t.Setenv("MAILHUB_CONNECT_STAGGER", "-1s")
	if _, err := Load(); err == nil {
		t.Fatal("负 CONNECT_STAGGER 应报错")
	}
	t.Setenv("MAILHUB_CONNECT_STAGGER", "1s")

	t.Setenv("MAILHUB_POLL_INTERVAL_DEFAULT", "3")
	if _, err := Load(); err == nil {
		t.Fatal("POLL_INTERVAL_DEFAULT < 5 应报错")
	}
	t.Setenv("MAILHUB_POLL_INTERVAL_DEFAULT", "60")

	t.Setenv("MAILHUB_SECRET_KEY", "short")
	if _, err := Load(); err == nil {
		t.Fatal("非法 SECRET_KEY 应报错")
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"50MB": 50 << 20, "10MiB": 10 << 20, "4KB": 4 << 10, "1GB": 1 << 30, "102400": 102400}
	for in, want := range cases {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v；期望 %d", in, got, err, want)
		}
	}
	if _, err := parseSize("abcMB"); err == nil {
		t.Error("非法数值应报错")
	}
}
