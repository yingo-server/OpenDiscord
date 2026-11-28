package main

import (
	"fmt"
	"os"
	"strings"
)

const exitUsage = 64

// main implements the launch contract:
//   - TTY: detect LANG (zh → Chinese UI), start the console; no credentials
//     → guided login wizard first.
//   - non-TTY: print usage and exit 64.
//   - flags: --lang zh-CN|en, --sound.
func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	lang := ""
	sound := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--lang" && i+1 < len(args):
			i++
			lang = args[i]
		case strings.HasPrefix(a, "--lang="):
			lang = strings.TrimPrefix(a, "--lang=")
		case a == "--sound":
			sound = true
		case a == "-h" || a == "--help":
			fmt.Println(localeFor("en").L(kUsage))
			return 0
		default:
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			fmt.Fprintln(os.Stderr, localeFor(detectLang()).L(kUsage))
			return exitUsage
		}
	}
	if lang == "" {
		lang = detectLang()
	}
	t := localeFor(lang)

	// non-TTY: print usage, exit 64
	fi, _ := os.Stdout.Stat()
	if fi == nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Println(t.L(kUsage))
		return exitUsage
	}

	creds, err := loadCredentials()
	if err != nil || creds.Token == "" {
		// guided login wizard before entering the console
		wizOut := os.Stdout
		a := newApp(nil, newUIState(t, lang, ""), os.Stdin, wizOut)
		if !loginWizard(a, "") {
			fmt.Fprintln(os.Stderr, t.L(kLoginFail))
			return 1
		}
		creds, err = loadCredentials()
		if err != nil || creds.Token == "" {
			return 1
		}
	}

	c := newClient(creds.Server, creds.Token, creds.CSRF)
	st := newUIState(t, lang, creds.Server)
	st.sound = sound
	a := newApp(c, st, os.Stdin, os.Stdout)
	stop := make(chan struct{})
	a.refresh()
	a.run(5_000_000_000, stop) // 5s auto refresh, returns when user quits
	close(stop)
	return 0
}
