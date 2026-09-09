package tool

import (
	"strings"
	"testing"
)

func TestEnvStripsSensitiveKeys(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/dev",
		"OPENAI_API_KEY=sk-1",
		"ANTHROPIC_API_KEY=sk-2",
		"GEMINI_API_KEY=sk-3",
		"AWS_SECRET_ACCESS_KEY=zz",
		"AZURE_CLIENT_SECRET=zz",
		"GOOGLE_APPLICATION_CREDENTIALS=/x",
		"MY_DB_PASSWORD=hunter2",
		"SSH_AUTH_SOCK=/tmp/sock",
		"GPG_TTY=/dev/ttys0",
		"GNUPGHOME=/home/dev/.gnupg",
		"XDEV_SECRET=1",
		"ONEGW_TOKEN=t",
		"secret_in_middle=sneaky", // case-insensitive match on SECRET
		"HISTFILE=/home/dev/.bash_history",
		"INPUTRC=/etc/inputrc",
		"CDPATH=/wasteland",
		"GLOBIGNORE=*.log",
	}
	env := HardenedEnvFrom(environ)
	joined := strings.Join(env, "\n")
	for _, banned := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
		"AWS_SECRET_ACCESS_KEY", "AZURE_CLIENT_SECRET",
		"GOOGLE_APPLICATION_CREDENTIALS", "MY_DB_PASSWORD", "SSH_AUTH_SOCK",
		"GPG_TTY", "GNUPGHOME", "XDEV_SECRET", "ONEGW_TOKEN",
		"secret_in_middle", "HISTFILE", "INPUTRC", "CDPATH", "GLOBIGNORE",
	} {
		if strings.Contains(joined, banned) {
			t.Errorf("stripped key %q leaked into child env", banned)
		}
	}
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/dev"} {
		if !strings.Contains(joined, want) {
			t.Errorf("benign key %q was dropped", want)
		}
	}
}

func TestEnvAlwaysSetInjected(t *testing.T) {
	env := HardenedEnvFrom(nil)
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"TERM=dumb", "NO_COLOR=1", "GIT_PAGER=cat", "PAGER=cat",
		"DEBIAN_FRONTEND=noninteractive", "LC_ALL=en_US.UTF-8",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("always-set var %q missing", want)
		}
	}
}

func TestEnvAlwaysSetNotDuplicated(t *testing.T) {
	env := HardenedEnvFrom([]string{"TERM=xterm"})
	if strings.Contains(strings.Join(env, "\n"), "TERM=xterm") {
		t.Error("inherited TERM should be overridden by TERM=dumb")
	}
}

func TestEnvHardenedDeterministicOrder(t *testing.T) {
	a := HardenedEnvFrom([]string{"B=2", "A=1"})
	b := HardenedEnvFrom([]string{"A=1", "B=2"})
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Errorf("env not order-independent: %v vs %v", a, b)
	}
}

func TestEnvHardeningMatchesFilteredKeys(t *testing.T) {
	environ := []string{"SECRET=1", "PATH=/bin"}
	hardened := HardenedEnvFrom(environ)
	for _, kv := range hardened {
		if strings.HasPrefix(kv, "SECRET=") {
			t.Errorf("EnvHardening missed %q", kv)
		}
		if strings.HasPrefix(kv, "PATH=") && kv != "PATH=/bin" {
			t.Errorf("PATH mangled: %q", kv)
		}
	}
	if len(hardened) != len(environ)-1+len(envAlwaysSet) {
		t.Fatalf("expected SECRET stripped + always-set appended, got %v", hardened)
	}
}
