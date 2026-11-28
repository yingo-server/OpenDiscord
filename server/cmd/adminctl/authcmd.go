package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/pkg/i18n"
)

func cmdLogin(g globals, args []string, in io.Reader, out, errw io.Writer) error {
	fs := newFlagSet("login", errw)
	username := fs.String("username", "", "admin username")
	passwordStdin := fs.Bool("password-stdin", false, "read password from stdin")
	totpFlag := fs.String("totp", "", "TOTP code")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *passwordStdin && *username == "" {
		return &usageError{msg: "--password-stdin requires --username"}
	}
	reader := bufio.NewReader(in)
	user := strings.TrimSpace(*username)
	if user == "" {
		// TTY → prompt; non-TTY → list the required flag and exit 64.
		// Flags are the source of truth: --username is the equivalent of
		// this prompt; interaction only fills flags in.
		if !i18n.StdinIsTTY() {
			i18n.RequiredFlags(errw, "login", []string{"username"})
			return &usageError{msg: "username is required"}
		}
		fmt.Fprint(errw, i18n.T("ui.admin_user"))
		line, err := readLine(reader)
		if err != nil {
			return err
		}
		user = strings.TrimSpace(line)
	}
	if user == "" {
		return &usageError{msg: "username is required"}
	}
	var password string
	if *passwordStdin {
		line, err := readLine(reader)
		if err != nil {
			return err
		}
		password = line
	} else {
		line, err := readSecret(in, errw, reader, "Password: ")
		if err != nil {
			return err
		}
		password = line
	}
	if password == "" {
		return &usageError{msg: "password is required"}
	}
	code := strings.TrimSpace(*totpFlag)
	if code == "" {
		fmt.Fprint(errw, "TOTP: ")
		line, err := readLine(reader)
		if err != nil {
			return err
		}
		code = strings.TrimSpace(line)
	}
	if code == "" {
		return &usageError{msg: "TOTP code is required"}
	}

	c, err := newClient(globals{Server: g.Server})
	if err != nil {
		return err
	}
	raw, err := c.do("POST", "/api/v1/login", map[string]string{
		"username": user, "password": password, "totp": code,
	}, false)
	if err != nil {
		return err
	}
	var lr struct {
		Token     string `json:"token"`
		CSRF      string `json:"csrf_token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &lr); err != nil {
		return fmt.Errorf("decode login response: %w", err)
	}
	if lr.Token == "" {
		return errors.New("login response did not contain a token")
	}
	if !g.NoSave {
		if err := saveCredentials(credentials{
			Server: g.Server, Token: lr.Token, CSRF: lr.CSRF,
			Username: user, ExpiresAt: lr.ExpiresAt,
		}); err != nil {
			fmt.Fprintf(errw, "adminctl: warning: could not save credentials: %v\n", err)
		}
	}
	if g.Output == "json" {
		return render(out, raw, "json")
	}
	// Requirement: login prints the token (stdout is script-consumable).
	fmt.Fprintln(out, lr.Token)
	return nil
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) && line == "" {
		return "", io.EOF
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readSecret disables terminal echo with stty when available. It is a
// best-effort UX measure; the password never touches disk or logs.
func readSecret(in io.Reader, errw io.Writer, r *bufio.Reader, prompt string) (string, error) {
	f, isFile := in.(*os.File)
	if isFile {
		stty, err := exec.LookPath("stty")
		if err == nil {
			off := exec.Command(stty, "-echo")
			off.Stdin = f
			if off.Run() == nil {
				defer func() {
					on := exec.Command(stty, "echo")
					on.Stdin = f
					_ = on.Run()
				}()
				fmt.Fprint(errw, prompt)
				line, rerr := readLine(r)
				fmt.Fprintln(errw)
				return line, rerr
			}
		}
	}
	fmt.Fprint(errw, prompt)
	return readLine(r)
}

func cmdCompletion(args []string, out io.Writer) error {
	if len(args) != 1 || args[0] != "bash" {
		return &usageError{msg: "completion requires exactly: bash"}
	}
	_, err := fmt.Fprint(out, `# bash completion for adminctl
_adminctl() {
    local cur prev
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    local cmds="login whoami users kick reports messages broadcast flags audit archive completion help"
    case "${prev}" in
        users)   COMPREPLY=( $(compgen -W "list show ban unban" -- "$cur") ); return ;;
        reports) COMPREPLY=( $(compgen -W "list show handle" -- "$cur") ); return ;;
        messages) COMPREPLY=( $(compgen -W "search recall" -- "$cur") ); return ;;
        broadcast) COMPREPLY=( $(compgen -W "preview send" -- "$cur") ); return ;;
        flags)   COMPREPLY=( $(compgen -W "list set" -- "$cur") ); return ;;
        archive) COMPREPLY=( $(compgen -W "status run" -- "$cur") ); return ;;
        completion) COMPREPLY=( $(compgen -W "bash" -- "$cur") ); return ;;
        --output|-o) COMPREPLY=( $(compgen -W "json table" -- "$cur") ); return ;;
        --server|--token|--csrf-token|--idempotency-key) return ;;
    esac
    if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "--server --token --csrf-token --output --idempotency-key --no-save" -- "$cur") )
    else
        COMPREPLY=( $(compgen -W "$cmds" -- "$cur") )
    fi
}
complete -F _adminctl adminctl
`)
	return err
}
