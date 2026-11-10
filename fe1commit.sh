#!/bin/sh
cd /var/minis/workspace/OpenDiscord || exit 1
git add -A
git commit -q -m "feat(1.4): FE-1a edit/forward/mentions/report domain"
git log --oneline -1
