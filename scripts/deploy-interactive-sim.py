#!/usr/bin/env python3
"""Drive deploy/install-interactive through a REAL PTY, step by step.

Unlike piping answers, this opens a pseudo-terminal, waits for each prompt to
appear, then sends that answer. It therefore exercises the wizard exactly as
an operator would: prompts are rendered, `read -s` hides the password, and the
final command line is assembled from the answers.

Used by .github/workflows/deploy-sim.yml; also runnable manually:
    python3 scripts/deploy-interactive-sim.py deploy/install-interactive

No third-party deps (stdlib pty/select/os). Transcript is printed to stdout
for the CI log and asserted against expected prompts before exit.
"""
import os
import pty
import select
import sys
import time

SCRIPT = sys.argv[1] if len(sys.argv) > 1 else "deploy/install-interactive"

# (prompt substring to wait for, answer to send, echo-expected?)
# Passwords use read -s: the PTY echoes them back from the terminal driver
# unless ECHO is disabled by the script — we assert on what we see below.
STEPS = [
    ("Language:" if False else "请选择语言", "1\n", True),       # 1 = 简体中文
    ("域名", "https://chat.example.com\n", True),                # ask domain_q
    ("管理员", "root\n", True),                                  # ask user_q
    ("密码", "a-very-strong-password-2026\n", False),            # read -s, no echo
    ("再次输入", "a-very-strong-password-2026\n", False),        # read -s, no echo
    ("冷库", "\n", True),                                        # cold DSN (empty)
    ("TURN", "\n", True),                                        # TURN (empty)
]

pid, fd = pty.fork()
if pid == 0:
    os.environ["TERM"] = "xterm"
    os.execvp("bash", ["bash", SCRIPT, "--interactive-test",
                       "--prefix", os.environ.get("UFOT_PREFIX", "/tmp/ufot-p"),
                       "--etc", os.environ.get("UFOT_ETC", "/tmp/ufot-e"),
                       "--state", os.environ.get("UFOT_STATE", "/tmp/ufot-s"),
                       "--no-start"])

transcript = ""
sent = 0
deadline = time.time() + 30

while time.time() < deadline:
    readable, _, _ = select.select([fd], [], [], 0.5)
    if readable:
        try:
            chunk = os.read(fd, 4096).decode("utf-8", "replace")
        except OSError:
            break
        if not chunk:
            break
        transcript += chunk
        # When the current step's prompt has appeared, answer it.
        if sent < len(STEPS):
            needle, answer, _ = STEPS[sent]
            if needle in transcript:
                os.write(fd, answer.encode())
                sent += 1
    done, status = os.waitpid(pid, os.WNOHANG)
    if done:
        break

print("=== interactive wizard transcript (real PTY) ===")
print(transcript)

if sent != len(STEPS):
    print(f"FAIL: answered {sent}/{len(STEPS)} prompts (transcript above)", file=sys.stderr)
    sys.exit(1)

# Assertions: every prompt type was shown, password not echoed, final line assembled.
required = ["请选择语言", "域名", "管理员", "密码", "再次输入", "冷库", "TURN"]
missing = [r for r in required if r not in transcript]
if missing:
    print(f"FAIL: missing prompts: {missing}", file=sys.stderr)
    sys.exit(1)
if "a-very-strong-password-2026" in transcript:
    print("FAIL: password was echoed to transcript (read -s broken)", file=sys.stderr)
    sys.exit(1)
if "--domain https://chat.example.com" not in transcript:
    print("FAIL: final command line not assembled from answers", file=sys.stderr)
    sys.exit(1)

print("PASS: real interactive flow, password hidden, command assembled")
