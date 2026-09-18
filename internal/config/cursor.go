package config

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

// cursorTokenSource is the credential chain's last rung for the Cursor
// subscription: the token Cursor's own IDE stores locally, so
// `xdev connect cursor` works with no key pasted and no env var exported.
//
// Two stores, tried in order:
//  1. the macOS Keychain item "cursor-access-token" (the session JWT
//     Cursor's CLI writes; the same value api.cursor.com/v1 accepts as a
//     bearer), via the existing keychainLookup seam;
//  2. ~/Library/Application Support/Cursor/User/globalStorage/state.vscdb,
//     the SQLite database Cursor's IDE opens, key cursorAuth/accessToken.
//
// Both are read-only: this function never writes, so it cannot corrupt a
// working Cursor install. Absent stores are reported as "" (not an error):
// a machine without Cursor installed is not a failure, it is a fall-through
// to the next credential source.
var cursorTokenSource = func() string {
	if v := cursorTokenKeychain(); v != "" {
		return v
	}
	if v := cursorTokenStateDB(); v != "" {
		return v
	}
	return ""
}

// cursorTokenKeychain reads the "cursor-access-token" Keychain item on macOS.
func cursorTokenKeychain() string {
	if !KeychainAvailable() {
		return ""
	}
	ref := KeychainRef{Service: "cursor-access-token", Account: "cursor-access-token"}
	v, err := keychainLookup(ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// cursorTokenStateDB reads cursorAuth/accessToken from Cursor's IDE state
// database. The path is the standard Cursor (VS Code fork) layout; on other
// platforms or a missing file it returns "".
func cursorTokenStateDB() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	db := filepath.Join(home, "Library", "Application Support", "Cursor",
		"User", "globalStorage", "state.vscdb")
	if _, err := os.Stat(db); err != nil {
		return ""
	}
	conn, err := sql.Open("sqlite", db+"?mode=ro&cache=shared")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		return ""
	}
	var value string
	err = conn.QueryRow(
		`SELECT value FROM ItemTable WHERE key = 'cursorAuth/accessToken' LIMIT 1`,
	).Scan(&value)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// cursorHasLocalToken reports whether a Cursor credential resolves from the
// IDE's own stores. Used by the connect listing to mark a row "ready" with
// zero user action.
func cursorHasLocalToken() bool {
	return cursorTokenSource() != ""
}

// isCursorProvider reports whether the named provider is the Cursor
// subscription (the only provider whose credential can be read from a
// local IDE store).
func isCursorProvider(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "cursor")
}

// cursorLocalTokenSource names the source for the connect listing.
func cursorLocalTokenSource() string {
	if cursorTokenKeychain() != "" {
		return "cursor keychain"
	}
	if cursorTokenStateDB() != "" {
		return "cursor state.db"
	}
	return ""
}
