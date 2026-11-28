package i18n

import "testing"

func TestLangDetection(t *testing.T) {
	cases := []struct {
		lang, lcAll, env string
		want             string
	}{
		{"", "zh_CN.UTF-8", "", ZH},
		{"", "C", "", EN},
		{"", "", "en_US.UTF-8", EN},
		{"", "", "zh_HK", ZH},
		{"zh-CN", "C", "", ZH}, // --lang overrides env
		{"en", "zh_CN.UTF-8", "", EN},
		{"", "", "", EN},
	}
	for _, c := range cases {
		t.Setenv("LC_ALL", c.lcAll)
		t.Setenv("LANG", c.env)
		if got := DetectLang(c.lang); got != c.want {
			t.Fatalf("DetectLang(%q, LC_ALL=%q, LANG=%q) = %q, want %q", c.lang, c.lcAll, c.env, got, c.want)
		}
	}
}

func TestMissingKeyFallsBack(t *testing.T) {
	SetLang(ZH)
	if got := T("no.such.key"); got != "no.such.key" {
		t.Fatalf("missing key in zh = %q", got)
	}
	SetLang(ZH)
	if got := T("ui.confirm"); got == "ui.confirm" || got == "" {
		t.Fatalf("zh key missing and did not fall back: %q", got)
	}
	SetLang(EN)
	if got := T("ui.confirm"); got == "ui.confirm" || got == "" {
		t.Fatalf("en key missing: %q", got)
	}
}

func TestSetLangPrefix(t *testing.T) {
	SetLang("zh_TW")
	if Current() != ZH {
		t.Fatalf("zh_TW → %q", Current())
	}
	SetLang("en_GB")
	if Current() != EN {
		t.Fatalf("en_GB → %q", Current())
	}
}
