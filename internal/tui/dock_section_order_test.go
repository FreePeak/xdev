package tui

import (
	"strings"
	"testing"
)

// TestDockEndsWithMCPThenTrajectory pins the panel's paint order, and it pins
// the ORDER because order is the whole change: the order of the appends in
// dockBuild IS the paint order — there is no sort anywhere in the panel — so a
// move back into collect() is a silent regression a "the section is there" test
// would never catch.
//
// MCP and the trajectory ride last, under SESSION: the panel ends on what this
// session is connected to and how to re-read it, not on a list of running
// children. MCP above TRAJECTORY because the servers are a fact about the
// session and the ledger is a button into it.
func TestDockEndsWithMCPThenTrajectory(t *testing.T) {
	app, _, _ := dockTestApp(t, 200, 40)
	app.SetDockMode(DockShow)
	// The footer is a fold like any other, and it is omitted when it has no
	// rows: with no cwd and no build stamp there is no SESSION section to sit
	// above anything, and the ordering this test pins would be untestable.
	app.SetLocation("/tmp/panel-order")
	app.SetVersion("0.0.0-test")
	app.SetDockOps(DockOps{
		Plan:       func() (string, bool) { return "1. do the thing", true },
		Tasks:      func() string { return "TASKS · 1/2 done\n[x] wire the panel" },
		Agents:     func() string { return "AGENTS · 1 running\nrunning reviewer" },
		Session:    func() (string, string) { return "sidebar order", "sess1234" },
		MCP:        func() string { return "MCP · 2\n○ be-kg\n○ db" },
		Trajectory: func() string { return "TRAJECTORY · 2 records" },
	})
	app.draw()

	app.mu.Lock()
	rows := append([]dockRow(nil), app.dock.lines...)
	app.mu.Unlock()

	at := func(prefix string) int {
		for i, r := range rows {
			if r.head && strings.HasPrefix(r.text, prefix) {
				return i
			}
		}
		return -1
	}
	session, mcp, traj := at("SESSION"), at("MCP"), at("TRAJECTORY")
	if session < 0 || mcp < 0 || traj < 0 {
		t.Fatalf("a section is missing: session=%d mcp=%d traj=%d\n%s", session, mcp, traj, dockLines(rows))
	}
	if !(session < mcp && mcp < traj) {
		t.Fatalf("wanted SESSION < MCP < TRAJECTORY, got %d < %d < %d\n%s", session, mcp, traj, dockLines(rows))
	}
	// And nothing paints after the ledger button: the bottom of the panel is the
	// ledger, which is the row a click opens.
	if tail := rows[traj:]; tail[len(tail)-1].act != dockTrajID {
		t.Fatalf("the ledger button is not the panel's last row\n%s", dockLines(rows))
	}
}
