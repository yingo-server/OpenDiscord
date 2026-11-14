package handler

// helpers_test.go: shared test helper (logger fallback for focused tests).

import "github.com/yingo-server/OpenDiscord/server/internal/pkg/log"

func testLogger() *log.Logger { return log.For("admin.http.test") }
