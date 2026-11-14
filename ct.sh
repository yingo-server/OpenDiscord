#!/bin/sh
cd /var/minis/workspace/OpenDiscord || exit 1
gofmt -w server/internal/data/pipe/msgdomain_test.go
git add -A
git commit -q -m "test(1.4): start fanout workers in TestMsgEditHappyPath"
git log --oneline -1
