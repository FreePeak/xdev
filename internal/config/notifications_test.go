package config

import (
	"strings"
	"testing"
)

// The notification axes ship with a default per axis, not with one switch for
// the whole group: a failure nobody sees is the case the group exists for, while
// an alert for the expected outcome is noise. Reading an unset axis must give
// that shipped default, so only an explicit `false` silences errors.
func TestNotificationAxisDefaults(t *testing.T) {
	s := &Settings{}
	if !s.NotificationsErrorsOn() {
		t.Error("errors must default ON: a failure nobody sees is the whole point")
	}
	if s.NotificationsAgentOn() {
		t.Error("agent (success) must default OFF: success is the expected outcome")
	}
	if !s.NotificationsSubagentsOn() {
		t.Error("subagents must default ON: a swallowed subagent failure hides the cause")
	}
	if s.NotificationsSoundOn() {
		t.Error("sound must default OFF: a bell interrupts another window")
	}
	if got := s.NotificationsCommand(); got != "" {
		t.Errorf("command must default empty, got %q", got)
	}

	var nilSettings *Settings
	if !nilSettings.NotificationsErrorsOn() || nilSettings.NotificationsAgentOn() ||
		!nilSettings.NotificationsSubagentsOn() || nilSettings.NotificationsSoundOn() {
		t.Error("a nil Settings must answer with the shipped defaults")
	}
	if got := nilSettings.NotificationsCommand(); got != "" {
		t.Errorf("nil Settings command = %q, want empty", got)
	}
}

// An explicit false is the only way to turn an axis off, and it must survive:
// this is the half of "unset != false" that makes the *bool worth having.
func TestNotificationAxisExplicitOff(t *testing.T) {
	off := false
	s := &Settings{}
	s.Notifications.Errors = &off
	s.Notifications.Subagents = &off
	if s.NotificationsErrorsOn() {
		t.Error("an explicit notifications.errors: false must silence failures")
	}
	if s.NotificationsSubagentsOn() {
		t.Error("an explicit notifications.subagents: false must silence subagent failures")
	}
}

// F2: a partial overlay changes only the keys it names. A layer that sets one
// axis must leave every other axis on its default, and a later layer that omits
// the group entirely must not blank what an earlier one set.
func TestNotificationSettingsSurviveTheLayerMerge(t *testing.T) {
	layer := &Settings{}
	on := true
	layer.Notifications.Agent = &on
	layer.Notifications.Command = "/bin/true"

	base := defaultSettings()
	if err := base.merge(layer); err != nil {
		t.Fatal(err)
	}
	if !base.NotificationsAgentOn() {
		t.Error("notifications.agent: true did not survive the merge")
	}
	if got := base.NotificationsCommand(); got != "/bin/true" {
		t.Errorf("notifications.command lost: %q", got)
	}
	// The axes the layer did not name keep their shipped defaults.
	if !base.NotificationsErrorsOn() {
		t.Error("naming notifications.agent changed notifications.errors")
	}
	if !base.NotificationsSubagentsOn() {
		t.Error("naming notifications.agent changed notifications.subagents")
	}
	if base.NotificationsSoundOn() {
		t.Error("naming notifications.agent turned sound on")
	}

	// A later layer omitting the group must not blank an earlier one.
	if err := base.merge(&Settings{}); err != nil {
		t.Fatal(err)
	}
	if !base.NotificationsAgentOn() || base.NotificationsCommand() != "/bin/true" {
		t.Error("an empty layer cleared the notifications group")
	}
}

// An axis turned off produces no sink work: the command is what makes the
// notification system spawn a process, so an unset command must stay unset and
// a command of only whitespace must not become a command.
func TestNotificationCommandOffMeansNoProcess(t *testing.T) {
	s := &Settings{}
	if s.NotificationsCommand() != "" {
		t.Fatal("an unset command must read as no command")
	}
	s.Notifications.Command = "   "
	if got := s.NotificationsCommand(); got != "" {
		t.Fatalf("whitespace command = %q, want empty: it would spawn an empty shell", got)
	}
}

// `xdev config set notifications.<axis>` must walk a real path, and a typo
// must still be rejected before it is written to a file that later fails to
// load (the whole reason settingsKeyOK exists).
func TestNotificationKeysAreKnown(t *testing.T) {
	known := []string{
		"notifications.errors",
		"notifications.agent",
		"notifications.subagents",
		"notifications.sound",
		"notifications.command",
	}
	for _, k := range known {
		if !settingsKeyOK(k) {
			t.Errorf("%s should walk a real path in Settings", k)
		}
	}
	if settingsKeyOK("notifications.errros") {
		t.Error("a typo below notifications must be rejected")
	}
	if settingsKeyOK("notification.errors") {
		t.Error("a typo in the group name must be rejected")
	}
}

// The rendered config listing has to show the effective value, not the raw
// pointer, or the printed config disagrees with what the session will do.
func TestNotificationAxesAppearInTheListing(t *testing.T) {
	out := strings.Join(List(&Settings{}, ""), "\n")
	for _, key := range []string{
		"notifications.errors true",
		"notifications.agent false",
		"notifications.subagents true",
		"notifications.sound false",
	} {
		if !strings.Contains(out, key) {
			t.Errorf("listing is missing %q", key)
		}
	}
}
