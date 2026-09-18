package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestMouseDebugLine(t *testing.T) {
	tests := []struct {
		name    string
		press   bool
		buttons tcell.ButtonMask
		mod     tcell.ModMask
		x, y    int
		want    string
	}{
		{"left press", true, tcell.Button1, 0, 10, 20, "btn1 press @10,20"},
		{"left release", false, tcell.Button1, 0, 10, 20, "btn1 move @10,20"},
		{"no buttons release", false, 0, 0, 10, 20, "up release @10,20"},
		{"wheel up", false, tcell.WheelUp, 0, 10, 20, "wheel↑ move @10,20"},
		{"wheel down", false, tcell.WheelDown, 0, 10, 20, "wheel↓ move @10,20"},
		{"wheel left", false, tcell.WheelLeft, 0, 10, 20, "wheel← move @10,20"},
		{"wheel right", false, tcell.WheelRight, 0, 10, 20, "wheel→ move @10,20"},
		{"right press", true, tcell.Button2, 0, 10, 20, "btn2 press @10,20"},
		{"middle press", true, tcell.Button3, 0, 10, 20, "btn3 press @10,20"},
		{"shift held", true, tcell.Button1, tcell.ModShift, 10, 20, "btn1+S press @10,20"},
		{"ctrl+alt held", true, tcell.Button1, tcell.ModCtrl | tcell.ModAlt, 10, 20, "btn1+AC press @10,20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tcell.NewEventMouse(tt.x, tt.y, tt.buttons, tt.mod)
			got := mouseDebugLine(m, tt.press)
			if got != tt.want {
				t.Errorf("mouseDebugLine() = %q, want %q", got, tt.want)
			}
		})
	}
}
