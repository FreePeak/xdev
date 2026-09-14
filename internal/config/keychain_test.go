package config

// The Keychain lane (#123 part 2): xdev reads a named item as a credential and
// never writes one. The write path was measured away — see the header of
// keychain.go — so what these tests hold is the read contract and, more
// importantly, that a reference which cannot be satisfied stops the chain
// instead of resolving somebody else's key.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseKeychainRef(t *testing.T) {
	for _, tc := range []struct {
		value     string
		wantRef   KeychainRef
		wantIsRef bool
	}{
		{value: "sk-or-v1plaintext", wantRef: KeychainRef{}},
		{value: "keychain:dev.xdev.credential.openai", wantRef: KeychainRef{Service: "dev.xdev.credential.openai", Account: "dev.xdev.credential.openai"}, wantIsRef: true},
		{value: " keychain:svc/linh ", wantRef: KeychainRef{Service: "svc", Account: "linh"}, wantIsRef: true},
		// A reference with no service is a typo, not a secret named "keychain:".
		{value: "keychain:", wantRef: KeychainRef{}},
		{value: "keychain:/linh", wantRef: KeychainRef{}},
	} {
		got, isRef := parseKeychainRef(tc.value)
		if isRef != tc.wantIsRef || got != tc.wantRef {
			t.Errorf("parseKeychainRef(%q) = %+v,%v want %+v,%v", tc.value, got, isRef, tc.wantRef, tc.wantIsRef)
		}
	}
}

// fakeSecurity answers like /usr/bin/security: secret on stdout, diagnostics on
// stderr, the tool's own exit status — so the parsing and the missing-item
// contract run on every platform (the CI job is Linux, where there is no
// Keychain to test against).
func fakeSecurity(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "security")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKeychainReadShape(t *testing.T) {
	ref := KeychainRef{Service: "dev.xdev.credential.openai", Account: "linh"}
	t.Run("exact value, one terminator trimmed", func(t *testing.T) {
		bin := fakeSecurity(t, `printf '%s\n' sk-item-value-12345`)
		got, err := runKeychainRead(bin, ref)
		if err != nil {
			t.Fatal(err)
		}
		if got != "sk-item-value-12345" {
			t.Fatalf("value = %q", got)
		}
	})
	t.Run("an 8 KiB token survives intact", func(t *testing.T) {
		big := strings.Repeat("B", 8192)
		t.Setenv("FAKE_KC_VALUE", big)
		bin := fakeSecurity(t, `printf '%s\n' "$FAKE_KC_VALUE"`)
		got, err := runKeychainRead(bin, ref)
		if err != nil {
			t.Fatal(err)
		}
		if got != big {
			t.Fatalf("wrote %d bytes, read %d", len(big), len(got))
		}
	})
	t.Run("spaces are not a delimiter", func(t *testing.T) {
		t.Setenv("FAKE_KC_VALUE", "tok with spaces and \"quotes\"")
		bin := fakeSecurity(t, `printf '%s\n' "$FAKE_KC_VALUE"`)
		got, err := runKeychainRead(bin, ref)
		if err != nil {
			t.Fatal(err)
		}
		if got != `tok with spaces and "quotes"` {
			t.Fatalf("value = %q", got)
		}
	})
	t.Run("a missing item says missing", func(t *testing.T) {
		bin := fakeSecurity(t, `echo 'security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.' >&2; exit 44`)
		_, err := runKeychainRead(bin, ref)
		if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), ref.Service) {
			t.Fatalf("err = %v, want it to name the missing item", err)
		}
	})
	t.Run("any other failure names the tool and the reason", func(t *testing.T) {
		bin := fakeSecurity(t, `echo 'unable to open keychain file' >&2; exit 1`)
		_, err := runKeychainRead(bin, ref)
		if err == nil || !strings.Contains(err.Error(), "unable to open keychain file") || !strings.Contains(err.Error(), bin) {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "not found") {
			t.Errorf("a keychain failure must not be dressed up as a missing item: %v", err)
		}
	})
	t.Run("the argv never carries the secret", func(t *testing.T) {
		// A read must pass only the lookup keys. If a value ever rode in argv,
		// any other user could ps it — the exact leak that made the write lane
		// unusable. The fake records its own argv for the assertion.
		argvLog := filepath.Join(t.TempDir(), "argv")
		t.Setenv("FAKE_KC_ARGV", argvLog)
		bin := fakeSecurity(t, `printf '%s\n' "$@" > "$FAKE_KC_ARGV"; printf 'sk\n'`)
		if _, err := runKeychainRead(bin, ref); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(argvLog)
		if err != nil {
			t.Fatal(err)
		}
		want := "find-generic-password\n-s\n" + ref.Service + "\n-a\n" + ref.Account + "\n-w\n"
		if string(raw) != want {
			t.Fatalf("argv = %q, want only the lookup keys %q", raw, want)
		}
	})
}

func TestKeychainReferenceStopsTheChain(t *testing.T) {
	// Every other rung is available and would be used if a reference that
	// cannot be satisfied fell through. It must not: that is a different
	// account, chosen by nobody.
	t.Setenv("ONEGW_API_KEY", "sk-from-the-environment")
	store := CredentialStore{"onegw": {Kind: "api_key", APIKey: "sk-stored-login"}}
	pc := &ProviderConfig{APIKey: "keychain:dev.xdev.credential.onegw"}

	old := keychainLookup
	t.Cleanup(func() { keychainLookup = old })

	t.Run("resolved when the item is there", func(t *testing.T) {
		keychainLookup = func(ref KeychainRef) (string, error) {
			if ref.Service != "dev.xdev.credential.onegw" {
				t.Errorf("looked up %q", ref.Service)
			}
			return "sk-from-the-keychain", nil
		}
		got, err := ResolveCredential(CredentialRequest{Provider: "onegw", ProviderCfg: pc, Store: store})
		if err != nil {
			t.Fatal(err)
		}
		if got.Value != "sk-from-the-keychain" || got.Source != "keychain:dev.xdev.credential.onegw" {
			t.Fatalf("resolved %q from %q", Redact(got.Value), got.Source)
		}
		if got.Header != "Authorization" {
			t.Errorf("header = %q", got.Header)
		}
	})

	for _, name := range []string{"missing", "keychain unavailable", "empty item"} {
		t.Run("no fall-through when "+name, func(t *testing.T) {
			switch name {
			case "missing":
				keychainLookup = func(KeychainRef) (string, error) { return "", os.ErrNotExist }
			case "keychain unavailable":
				keychainLookup = func(KeychainRef) (string, error) {
					return "", os.ErrInvalid // what the production seam returns on a non-mac host
				}
			case "empty item":
				keychainLookup = func(KeychainRef) (string, error) { return "   ", nil }
			}
			got, err := ResolveCredential(CredentialRequest{Provider: "onegw", ProviderCfg: pc, Store: store})
			if err == nil {
				t.Fatalf("fell through to %q (%s)", Redact(got.Value), got.Source)
			}
			if got.Value != "" {
				t.Errorf("a stopped chain must hand back no credential, got %q", Redact(got.Value))
			}
			if !strings.Contains(err.Error(), "keychain:dev.xdev.credential.onegw") {
				t.Errorf("error must name the reference: %v", err)
			}
		})
	}
}

func TestKeychainLookupRefusesWhereNoneCanBeQueried(t *testing.T) {
	// The production seam, unfaked, on a host it cannot serve. Disabling it
	// through the documented escape hatch is the same refusal on a Mac as
	// off-platform, so this asserts identically on the Linux CI job and here.
	t.Setenv(DisableKeychainEnv, "1")
	if KeychainAvailable() {
		t.Fatal("the disable switch must be honoured")
	}
	_, err := keychainLookup(KeychainRef{Service: "svc", Account: "acct"})
	if err == nil || !strings.Contains(err.Error(), DisableKeychainEnv) {
		t.Fatalf("err = %v, want it to name the reason and the switch", err)
	}
}
