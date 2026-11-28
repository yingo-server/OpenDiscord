// Package i18n is a minimal, dependency-free bilingual (zh-CN / en) message
// catalog for the interactive layer of the CLIs.
//
// Design rules:
//   - flags are the SOURCE OF TRUTH; interactive prompts only fill flags in.
//     Every prompt has an equivalent flag, so scripts and CI never need TTY.
//   - Language detection: --lang flag > LC_ALL > LANG; containing "zh" →
//     Chinese, otherwise English. Missing keys fall back to English.
package i18n

import (
	"os"
	"strings"
	"sync/atomic"
)

// Lang codes.
const (
	ZH = "zh-CN"
	EN = "en"
)

var current atomic.Value // string

func init() { current.Store(EN) }

// SetLang overrides the detected language ("zh-CN" or "en").
func SetLang(l string) {
	switch {
	case strings.HasPrefix(l, "zh"):
		current.Store(ZH)
	default:
		current.Store(EN)
	}
}

// DetectLang resolves the language from an explicit --lang value (wins),
// otherwise from LC_ALL / LANG environment variables.
func DetectLang(flagLang string) string {
	if flagLang != "" {
		return normalize(flagLang)
	}
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			return normalize(v)
		}
	}
	return EN
}

func normalize(v string) string {
	if strings.Contains(strings.ToLower(v), "zh") {
		return ZH
	}
	return EN
}

// catalogs maps language → catalog. en.go / zh-CN.go only declare the raw
// maps; this var wires them.
var catalogs = map[string]map[string]string{
	ZH: zhCN,
	EN: en,
}

// Current returns the active language code.
func Current() string { return current.Load().(string) }

// T translates key in the current language; missing keys fall back to
// English, then to the key itself (never panic, never empty).
func T(key string) string {
	lang := Current()
	if s, ok := catalogs[lang][key]; ok {
		return s
	}
	if s, ok := catalogs[EN][key]; ok {
		return s
	}
	return key
}
