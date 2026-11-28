package i18n

import (
	"fmt"
	"io"
	"strings"
)

// RequiredFlags lists the required flags of a subcommand when they are
// missing. CLIs stay command-driven (no interactive menu — that was a
// deliberate design rejection): on TTY or not, a missing required parameter
// prints the flag list and exits 64.
//
// Principle (see package doc): flags are the source of truth; there is no
// prompt without an equivalent flag.
func RequiredFlags(w io.Writer, cmd string, flags []string) {
	fmt.Fprintf(w, T("cli.missing_required")+"\n", cmd)
	for _, f := range flags {
		fmt.Fprintf(w, "  --%s\n", f)
	}
	fmt.Fprintln(w, T("cli.rerun_hint"))
}

// FormatRequired is the string variant for tests and error values.
func FormatRequired(cmd string, flags []string) string {
	var b strings.Builder
	RequiredFlags(&b, cmd, flags)
	return b.String()
}
