package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSettingsImageProvidersKeys pins the imageProviders layer contract (M15
// #69): the block survives KnownFields decoding, the provider list replaces
// wholesale, the timeout and size cap survive a later layer that omits them,
// and ${VAR} references are expanded only at use time.
func TestSettingsImageProvidersKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("IMAGE_TEST_KEY", "sk-from-env")
	cwd := t.TempDir()
	writeFile(t, GlobalSettingsPath(), `
imageProviders:
  timeout: 90s
  maxBytes: 8388608
  providers:
    - name: openai
      model: gpt-image-1
      apiKey: ${IMAGE_TEST_KEY}
    - name: gemini
`)

	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ImageProviders.Timeout != "90s" || s.ImageProviders.MaxBytes != 8388608 {
		t.Fatalf("block = %+v", s.ImageProviders)
	}
	if len(s.ImageProviders.Providers) != 2 {
		t.Fatalf("providers = %+v", s.ImageProviders.Providers)
	}

	// The raw block stays verbatim; the accessor expands for the caller.
	if s.ImageProviders.Providers[0].APIKey != "${IMAGE_TEST_KEY}" {
		t.Fatalf("merge expanded env refs eagerly: %q", s.ImageProviders.Providers[0].APIKey)
	}
	cfg := s.ImageGenConfig()
	if cfg.Providers[0].APIKey != "sk-from-env" {
		t.Fatalf("apiKey = %q, want the expansion of ${IMAGE_TEST_KEY}", cfg.Providers[0].APIKey)
	}
	if cfg.Timeout != "90s" || cfg.MaxBytes != 8388608 {
		t.Fatalf("accessor lost block keys: %+v", cfg)
	}

	// A later layer that names one provider means exactly that chain; keys
	// and caps it never mentions survive. The block carries image-provider
	// credentials, so the layer is the user's file, not a clone's (#114).
	gemini := writeFile(t, filepath.Join(t.TempDir(), "gemini.yml"), `
imageProviders:
  providers:
    - name: gemini
`)
	s, err = LoadSettings(cwd, []string{gemini})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ImageGenConfig().Providers) != 1 || s.ImageGenConfig().Providers[0].Name != "gemini" {
		t.Fatalf("providers after the overlay = %+v", s.ImageGenConfig().Providers)
	}
	if s.ImageProviders.Timeout != "90s" || s.ImageProviders.MaxBytes != 8388608 {
		t.Fatalf("overlay lost untouched keys: %+v", s.ImageProviders)
	}

	// The accessor must not hand out the merged slice: a caller that edits
	// it would corrupt the settings for the rest of the process.
	cfg.Providers[0].Name = "mutated"
	if s.ImageProviders.Providers[0].Name == "mutated" {
		t.Fatal("ImageGenConfig aliased the settings slice")
	}
}

func TestSettingsImageProvidersValidation(t *testing.T) {
	cwd := t.TempDir()
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "bad timeout",
			body: "imageProviders:\n  timeout: ninety seconds\n",
			want: "imageProviders.timeout",
		},
		{
			name: "unknown adapter",
			body: "imageProviders:\n  providers:\n    - name: dall-e\n",
			want: "unknown provider",
		},
		{
			name: "negative cap",
			body: "imageProviders:\n  maxBytes: -1\n",
			want: "maxBytes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			writeFile(t, GlobalSettingsPath(), tc.body)
			_, err := LoadSettings(cwd, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestImageGenConfigNilTolerance: pre-main callers (tests, tooling) hand the
// registry a nil settings pointer, which must still yield a usable zero block.
func TestImageGenConfigNilTolerance(t *testing.T) {
	empty := (*Settings)(nil).ImageGenConfig()
	if len(empty.Providers) != 0 || empty.Timeout != "" || empty.MaxBytes != 0 {
		t.Fatalf("nil settings yielded %+v", empty)
	}
}
