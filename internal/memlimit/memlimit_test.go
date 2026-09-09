package memlimit

import "testing"

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"100MB", 100 << 20, false},
		{"512KB", 512 << 10, false},
		{"2GB", 2 << 30, false},
		{"4096", 4096, false},
		{"4096b", 4096, false},
		{"", 0, true},
		{"abc", 0, true},
		{"1.5MB", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseBytes(tc.in)
		if tc.wantErr && err == nil {
			t.Errorf("ParseBytes(%q) = %d, want error", tc.in, got)
			continue
		}
		if !tc.wantErr && (err != nil || got != tc.want) {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestApplyDefault(t *testing.T) {
	t.Setenv("XDEV_MEMLIMIT", "")
	if got := Apply(); got != DefaultLimitBytes {
		t.Fatalf("Apply() = %d, want default %d", got, DefaultLimitBytes)
	}
	t.Setenv("XDEV_MEMLIMIT", "64MB")
	if got := Apply(); got != 64<<20 {
		t.Fatalf("Apply() = %d, want 64MB", got)
	}
}
