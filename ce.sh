#!/bin/sh
cd /var/minis/workspace/OpenDiscord || exit 1
git add -A
git commit -q -m "fix(1.4): define execChatd handoff helper in chatd-pg"
git log --oneline -1
