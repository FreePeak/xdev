package tool

import (
	"errors"
	"strings"
	"testing"
)

// The write-device seam behind write xd://resolve|reject: dispatch, the
// unknown-device error (never a silently created file), and the
// unregistered-scheme fallthrough to the filesystem.
func TestWriteURIDispatchesRegisteredDevice(t *testing.T) {
	var got []string
	RegisterWriteDevice("xdtest", "resolve", func(uri, content string) (string, error) {
		got = append(got, uri+"|"+content)
		return "resolved: " + content, nil
	})

	text, handled, err := WriteURI("xdtest://resolve", "ship it")
	if !handled || err != nil {
		t.Fatalf("dispatch: handled=%v err=%v", handled, err)
	}
	if text != "resolved: ship it" {
		t.Fatalf("device text = %q", text)
	}
	if len(got) != 1 || got[0] != "xdtest://resolve|ship it" {
		t.Fatalf("device saw %v", got)
	}

	// A registered scheme with an unregistered device is an error, so a
	// typo cannot fall through to a literal file.
	if _, handled, err := WriteURI("xdtest://nope", "x"); !handled || err == nil {
		t.Fatalf("unknown device: handled=%v err=%v, want handled+error", handled, err)
	}

	// Unregistered schemes stay filesystem paths.
	if _, handled, _ := WriteURI("noscheme://resolve", "x"); handled {
		t.Fatal("unregistered scheme must not be handled by the device seam")
	}
	if _, handled, _ := WriteURI("/tmp/plain.txt", "x"); handled {
		t.Fatal("plain paths must not be handled by the device seam")
	}
}

// A device error reaches the caller instead of being swallowed.
func TestWriteURISurfacesDeviceError(t *testing.T) {
	RegisterWriteDevice("xderr", "reject", func(string, string) (string, error) {
		return "", errors.New("no pending proposal to reject")
	})
	text, handled, err := WriteURI("xderr://reject", "nope")
	if !handled || err == nil || !strings.Contains(err.Error(), "no pending") {
		t.Fatalf("text=%q handled=%v err=%v", text, handled, err)
	}
}
