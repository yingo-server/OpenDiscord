//go:build pgdriver

// This file is compiled only into the chatd-pg cold-store variant
// (CI builds the same ./server/cmd/chatd source with -tags pgdriver).
// The blank import registers the "pgx" and "pgx/v5" database/sql drivers in
// THIS process, so assemble.go step 8 finds a driver and opens the cold store.
// The default chatd build deliberately carries no postgres driver (architecture
// red line: driver injection is a deployment-layer decision).
package main

import _ "github.com/jackc/pgx/v5/stdlib"
