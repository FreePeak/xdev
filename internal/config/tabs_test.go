package config

import "testing"

// The tab strip paints unless it is explicitly off: unset (the shipped default
// "auto"), "on", and an unknown value all show it, and only "off" hides it.
// A typo must not hide a strip the user did not ask to hide.
func TestTabsModeOnIsOptIn(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"", true},
		{"auto", true},
		{"off", false},
		{"OFF", false},
		{"on", true},
		{"ON", true},
		{"onnn", true},
	}
	for _, c := range cases {
		s := &Settings{}
		s.Tui.Tabs.Mode = c.mode
		if got := s.TabsModeOn(); got != c.want {
			t.Errorf("mode %q -> %v, want %v", c.mode, got, c.want)
		}
	}
	if !(&Settings{}).TabsModeOn() {
		t.Error("a nil-ish default settings must paint the strip")
	}
	var nilSettings *Settings
	if !nilSettings.TabsModeOn() {
		t.Error("nil settings must paint the strip")
	}
}

// The whole Tui.Tabs group had NO merge arm, so a config layer naming it was
// parsed and then thrown away: `tui.tabs.mode: on` (and the `xdev config set
// tui.tabs.mode on` that writes exactly that) never reached TabsModeOn, and
// the strip's visibility was decided by the App's own default. A setting the
// file can spell is a setting the loader has to carry.
func TestTabsSettingsSurviveTheLayerMerge(t *testing.T) {
	layer := &Settings{}
	layer.Tui.Tabs.Mode = "on"
	layer.Tui.Tabs.Scope = "global"
	layer.Tui.Tabs.Indicators = "numbers"

	base := defaultSettings()
	if err := base.merge(layer); err != nil {
		t.Fatal(err)
	}
	if !base.TabsModeOn() {
		t.Error("tui.tabs.mode: on did not survive the merge")
	}
	if base.Tui.Tabs.Scope != "global" || !base.TabsIndicatorsOn() {
		t.Fatalf("tabs group lost fields: scope=%q indicators=%q", base.Tui.Tabs.Scope, base.Tui.Tabs.Indicators)
	}
	// A later layer that omits the group must not blank an earlier one.
	if err := base.merge(&Settings{}); err != nil {
		t.Fatal(err)
	}
	if !base.TabsModeOn() {
		t.Error("an empty layer cleared tui.tabs.mode")
	}
}
