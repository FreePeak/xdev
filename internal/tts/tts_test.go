package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/FreePeak/xdev/internal/tool"
)

// Every non-dry-run test in this file stubs the platform binary (via the
// Speaker.Lookup seam or a fake `say` on PATH) — a test that forgets would
// speak out loud on the developer's machine.

func runes(s string) int { return utf8.RuneCountInString(s) }

func TestSplitSentenceBoundaries(t *testing.T) {
	tests := []struct {
		name string
		text string
		max  int
		want []string
	}{
		{
			name: "packed when it fits",
			text: "One. Two. Three.",
			max:  MaxChunkRunes,
			want: []string{"One. Two. Three."},
		},
		{
			name: "split on sentence ends when max is tight",
			text: "One. Two. Three.",
			max:  8,
			want: []string{"One.", "Two.", "Three."},
		},
		{
			name: "abbreviations do not end a sentence",
			text: "Use e.g. this and i.e. that. Dr. Smith agrees.",
			max:  MaxChunkRunes,
			want: []string{"Use e.g. this and i.e. that. Dr. Smith agrees."},
		},
		{
			name: "initials and list numbers stay inside the sentence",
			text: "J. R. R. Tolkien wrote it. Next.",
			max:  MaxChunkRunes,
			want: []string{"J. R. R. Tolkien wrote it. Next."},
		},
		{
			name: "decimal point is not a boundary",
			text: "Pi is 3.14 exactly. Done.",
			max:  MaxChunkRunes,
			want: []string{"Pi is 3.14 exactly. Done."},
		},
		{
			name: "terminator run is one boundary",
			text: "Wait... What?! Yes!",
			max:  MaxChunkRunes,
			want: []string{"Wait... What?! Yes!"},
		},
		{
			name: "CJK full stops split",
			text: "你好。世界。",
			max:  3,
			want: []string{"你好。", "世界。"},
		},
		{
			name: "empty and blank input produce nothing",
			text: "  \n\t ",
			max:  MaxChunkRunes,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Split(tt.text, tt.max)
			if len(got) != len(tt.want) {
				t.Fatalf("Split(%q, %d) = %q, want %q", tt.text, tt.max, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("chunk %d = %q, want %q (all: %q)", i, got[i], tt.want[i], got)
				}
				if n := runes(got[i]); n > tt.max {
					t.Fatalf("chunk %d is %d runes, over max %d: %q", i, n, tt.max, got[i])
				}
			}
		})
	}
}

func TestSplitBoundsLongSentence(t *testing.T) {
	// One sentence with no terminator at all: must still be broken up, with
	// no word lost and every chunk inside the bound.
	words := make([]string, 60)
	for i := range words {
		words[i] = "alpha"
	}
	text := strings.Join(words, " ")
	const max = 40

	chunks := Split(text, max)
	if len(chunks) < 5 {
		t.Fatalf("got %d chunks for %d runes at max %d", len(chunks), len(text), max)
	}
	var joined []string
	for i, c := range chunks {
		if n := runes(c); n > max {
			t.Fatalf("chunk %d is %d runes, over max %d", i, n, max)
		}
		joined = append(joined, c)
	}
	if got := strings.Join(joined, " "); got != text {
		t.Fatalf("chunks lost or reordered words:\n got %q\nwant %q", got, text)
	}
}

func TestSplitHardCutsUnbrokenToken(t *testing.T) {
	text := strings.Repeat("x", 50)
	chunks := Split(text, 20)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3: %q", len(chunks), chunks)
	}
	for i, c := range chunks {
		if runes(c) > 20 {
			t.Fatalf("chunk %d = %d runes, over max 20", i, runes(c))
		}
	}
	if strings.Join(chunks, "") != text {
		t.Fatalf("hard cut lost characters: %q", chunks)
	}
}

// fakeLookPath reports the named binaries as installed, everything else as
// absent.
func fakeLookPath(installed ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		for _, have := range installed {
			if have == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", fs.ErrNotExist
	}
}

func TestForGOOSBackendAndArgv(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		installed []string
		wantBin   string
		wantArgs  []string
	}{
		{
			name:     "darwin defaults",
			goos:     "darwin",
			wantBin:  "say",
			wantArgs: []string{"--", "Hello world"},
		},
		{
			name:     "darwin voice and rate",
			goos:     "darwin",
			wantBin:  "say",
			wantArgs: []string{"-v", "Samantha", "-r", "250", "--", "Hello world"},
		},
		{
			name:      "linux prefers speech-dispatcher",
			goos:      "linux",
			installed: []string{"spd-say", "espeak-ng"},
			wantBin:   "spd-say",
			// relativeRate(250, 100) = 42
			wantArgs: []string{"-w", "-y", "English (US)", "-r", "42", "--", "Hello world"},
		},
		{
			name:      "linux falls back to espeak-ng",
			goos:      "linux",
			installed: []string{"espeak-ng"},
			wantBin:   "espeak-ng",
			wantArgs:  []string{"-v", "en", "-s", "250", "--", "Hello world"},
		},
		{
			name:     "linux with nothing installed still names the preferred binary",
			goos:     "linux",
			wantBin:  "spd-say",
			wantArgs: []string{"-w", "--", "Hello world"},
		},
		{
			name:      "windows prefers powershell",
			goos:      "windows",
			installed: []string{"powershell", "pwsh"},
			wantBin:   "powershell",
		},
		{
			name:      "windows falls back to pwsh",
			goos:      "windows",
			installed: []string{"pwsh"},
			wantBin:   "pwsh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			be, err := ForGOOS(tt.goos, fakeLookPath(tt.installed...))
			if err != nil {
				t.Fatalf("ForGOOS(%q): %v", tt.goos, err)
			}
			if be.Name() != tt.wantBin {
				t.Fatalf("backend = %q, want %q", be.Name(), tt.wantBin)
			}
			if be.Hint() == "" {
				t.Fatal("backend hint is empty: a missing binary error would not be actionable")
			}
			if tt.wantArgs == nil {
				return // Windows argv is asserted separately (encoded command).
			}
			req := Request{Text: "Hello world"}
			if tt.name == "darwin voice and rate" {
				req.Voice, req.Rate = "Samantha", 250
			}
			if strings.Contains(tt.name, "speech-dispatcher") {
				req.Voice, req.Rate = "English (US)", 250
			}
			if strings.Contains(tt.name, "espeak-ng") {
				req.Voice, req.Rate = "en", 250
			}
			got := be.Args(req.Text, req)
			if strings.Join(got, "\x00") != strings.Join(tt.wantArgs, "\x00") {
				t.Fatalf("argv = %q, want %q", got, tt.wantArgs)
			}
		})
	}
}

func TestForGOOSUnsupported(t *testing.T) {
	if _, err := ForGOOS("plan9", fakeLookPath()); err == nil {
		t.Fatal("ForGOOS(plan9) should be an actionable error")
	} else if !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("error should name the OS: %v", err)
	}
}

// decodePowerShell unwraps the -EncodedCommand base64/UTF-16LE payload.
func decodePowerShell(t *testing.T, args []string) string {
	t.Helper()
	if len(args) < 4 || args[len(args)-2] != "-EncodedCommand" {
		t.Fatalf("argv does not use -EncodedCommand: %q", args)
	}
	raw, err := base64.StdEncoding.DecodeString(args[len(args)-1])
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	units := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

func TestWindowsArgvSpeaksTextWithSettings(t *testing.T) {
	be, err := ForGOOS("windows", fakeLookPath("powershell"))
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Text: "It's 3.5 degrees; say \"hi\"", Voice: "Samantha", Rate: 600}
	args := be.Args(req.Text, req)
	script := decodePowerShell(t, args)
	for _, want := range []string{
		"Add-Type -AssemblyName System.Speech",
		"$s.Speak('It''s 3.5 degrees; say \"hi\"')", // single quotes escaped for PS
		"$s.SelectVoice('Samantha')",
		"$s.Rate = 10", // relativeRate(600, 10) clamps to the scale
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestRelativeRateClamps(t *testing.T) {
	for _, tt := range []struct {
		wpm, span, want int
	}{
		{wpm: 175, span: 100, want: 0},
		{wpm: 250, span: 100, want: 42},
		{wpm: 600, span: 10, want: 10},
		{wpm: 1, span: 10, want: -9},
		{wpm: 0, span: 10, want: 0},
	} {
		if got := relativeRate(tt.wpm, tt.span); got != tt.want {
			t.Fatalf("relativeRate(%d, %d) = %d, want %d", tt.wpm, tt.span, got, tt.want)
		}
	}
}

func TestRateOK(t *testing.T) {
	for _, tt := range []struct {
		rate int
		want bool
	}{
		{rate: 0, want: true},
		{rate: 80, want: true},
		{rate: 600, want: true},
		{rate: 79, want: false},
		{rate: 601, want: false},
		{rate: -5, want: false},
	} {
		if got := RateOK(tt.rate); got != tt.want {
			t.Fatalf("RateOK(%d) = %v, want %v", tt.rate, got, tt.want)
		}
	}
}

// stubBinary writes an executable shell script that appends its argv to log.
func stubBinary(t *testing.T, name, log string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub backend is a /bin/sh script")
	}
	path := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub binary was never invoked: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func TestSpeakPlaysEveryChunkSequentially(t *testing.T) {
	log := filepath.Join(t.TempDir(), "say.log")
	bin := stubBinary(t, "say", log)
	// Long enough to need more than one chunk at the shipping chunk size.
	sentence := "Alpha bravo charlie delta echo foxtrot."
	text := strings.TrimSpace(strings.Repeat(sentence+" ", 10))
	chunks := Split(text, MaxChunkRunes)
	if len(chunks) < 2 {
		t.Fatalf("expected a multi-chunk text, got %q", chunks)
	}

	sp := Speaker{backend: sayBackend{}, Lookup: func(string) (string, error) { return bin, nil }}
	rep, err := sp.Speak(context.Background(), Request{Text: text, Rate: 200})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if rep.Backend != "say" || rep.DryRun || rep.Bytes != len(text) || rep.Truncated {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if strings.Join(rep.Chunks, "\x00") != strings.Join(chunks, "\x00") {
		t.Fatalf("chunks = %q, want %q", rep.Chunks, chunks)
	}
	// One invocation per chunk, in order: chunk n+1 starts only after chunk n
	// exits, so playback never overlaps.
	lines := readLines(t, log)
	if len(lines) != len(chunks) {
		t.Fatalf("stub invoked %d times, want %d (%q)", len(lines), len(chunks), chunks)
	}
	for i, line := range lines {
		if !strings.HasSuffix(line, chunks[i]) {
			t.Fatalf("invocation %d = %q, want it to end with chunk %q", i, line, chunks[i])
		}
		if !strings.HasPrefix(line, "-r 200 ") {
			t.Fatalf("rate was not passed through: %q", line)
		}
	}
}

func TestSpeakMissingBinaryIsActionable(t *testing.T) {
	sp := Speaker{backend: sayBackend{}, Lookup: fakeLookPath()}
	_, err := sp.Speak(context.Background(), Request{Text: "hi"})
	if err == nil {
		t.Fatal("missing binary should error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "say") || !strings.Contains(msg, "PATH") {
		t.Fatalf("error not actionable: %q", msg)
	}
}

func TestSpeakDryRunSpawnsNothing(t *testing.T) {
	sp := Speaker{
		backend: sayBackend{},
		Lookup:  func(string) (string, error) { t.Fatal("dry run must not resolve or spawn the backend"); return "", nil },
	}
	rep, err := sp.Speak(context.Background(), Request{Text: "One. Two.", DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !rep.DryRun || len(rep.Chunks) != 1 || rep.Backend != "say" {
		t.Fatalf("report = %+v", rep)
	}
	plan := sp.Plan(Request{Text: "One. Two.", DryRun: true})
	if len(plan) != 1 || !strings.HasPrefix(plan[0], "[1/1] say -- One. Two.") {
		t.Fatalf("plan = %q", plan)
	}
}

func TestSpeakTruncatesAtCap(t *testing.T) {
	text := strings.Repeat("a", MaxTextBytes-1) + ". the tail must be dropped"
	sp := Speaker{backend: sayBackend{}, Lookup: func(string) (string, error) { t.Fatal("dry run spawns nothing"); return "", nil }}
	rep, err := sp.Speak(context.Background(), Request{Text: text, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Truncated || rep.Bytes != MaxTextBytes || rep.Total != len(text) {
		t.Fatalf("report = %+v (text len %d)", rep, len(text))
	}
	if strings.Contains(strings.Join(rep.Chunks, " "), "dropped") {
		t.Fatal("truncated tail leaked into the chunks")
	}
}

func TestSpeakEmptyText(t *testing.T) {
	sp := Speaker{backend: sayBackend{}}
	if _, err := sp.Speak(context.Background(), Request{Text: "   "}); err == nil {
		t.Fatal("blank text should error")
	}
}

func TestSpeakDefaultLookupUsesPATH(t *testing.T) {
	log := filepath.Join(t.TempDir(), "say.log")
	bin := stubBinary(t, "say", log)
	// Lookup nil => the real exec.LookPath, so this covers the shipping path
	// (a real `say` would speak; the stub on PATH keeps it silent).
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	sp := Speaker{backend: sayBackend{}}
	rep, err := sp.Speak(context.Background(), Request{Text: "Path lookup works."})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if rep.Backend != "say" {
		t.Fatalf("backend = %q", rep.Backend)
	}
	if lines := readLines(t, log); len(lines) != 1 || !strings.HasSuffix(lines[0], "Path lookup works.") {
		t.Fatalf("stub log = %q", lines)
	}
}

// --- tool surface ---

func TestToolMeta(t *testing.T) {
	tl := NewTool(Settings{})
	if tl.Name() != ToolName {
		t.Fatalf("Name = %q", tl.Name())
	}
	if tl.Description() == "" {
		t.Fatal("Description is empty")
	}
	var schema struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tl.Parameters(), &schema); err != nil {
		t.Fatalf("Parameters is not valid JSON: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "text" {
		t.Fatalf("required = %q, want [text]", schema.Required)
	}
	for _, key := range []string{"text", "voice", "rate", "dry_run"} {
		if _, ok := schema.Properties[key]; !ok {
			t.Fatalf("schema is missing %q: %s", key, tl.Parameters())
		}
	}
}

func execTool(t *testing.T, tl *Tool, args string) tool.Result {
	t.Helper()
	res, err := tl.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute(%s) returned harness error: %v", args, err)
	}
	return res
}

func TestToolExecuteDryRunReportsChunks(t *testing.T) {
	tl := NewTool(Settings{Rate: 200})
	tl.Backend = sayBackend{} // pin the argv spelling: the host runner may resolve spd-say
	tl.Lookup = func(string) (string, error) { t.Fatal("dry run must not resolve the backend"); return "", nil }
	res := execTool(t, tl, `{"text":"First. Second.","dry_run":true}`)
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Text)
	}
	if !strings.Contains(res.Text, "dry-run: planned 1 chunk(s) via say") {
		t.Fatalf("text = %q", res.Text)
	}
	details, ok := res.Details.(json.RawMessage)
	if !ok {
		t.Fatalf("Details is %T, want json.RawMessage", res.Details)
	}
	var payload struct {
		Chunks []string `json:"chunks"`
		Plan   []string `json:"plan"`
	}
	if err := json.Unmarshal(details, &payload); err != nil {
		t.Fatalf("Details is not valid JSON: %v (%s)", err, details)
	}
	if len(payload.Chunks) != 1 || len(payload.Plan) != 1 {
		t.Fatalf("details = %s", details)
	}
	if !strings.Contains(payload.Plan[0], "-r 200") {
		t.Fatalf("plan did not inherit tts.rate: %q", payload.Plan[0])
	}
}

func TestToolExecuteRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
	}{
		{name: "missing text", args: `{}`, want: "text is required"},
		{name: "blank text", args: `{"text":"  "}`, want: "text is required"},
		{name: "malformed json", args: `{"text":`, want: "malformed arguments"},
		{name: "rate too low", args: `{"text":"hi","rate":10}`, want: "out of range"},
		{name: "rate too high", args: `{"text":"hi","rate":9000}`, want: "out of range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := execTool(t, NewTool(Settings{}), tt.args)
			if !res.IsError || !strings.Contains(res.Text, tt.want) {
				t.Fatalf("result = %+v, want IsError with %q", res, tt.want)
			}
		})
	}
}

func TestToolExecuteWithoutBackendExplains(t *testing.T) {
	tl := NewTool(Settings{})
	tl.Backend = nil // e.g. an unsupported GOOS
	res := execTool(t, tl, `{"text":"hi"}`)
	if !res.IsError || !strings.Contains(res.Text, "no local speech backend") {
		t.Fatalf("result = %+v", res)
	}
}

func TestToolExecuteOverridesSettingsAndSpeaks(t *testing.T) {
	log := filepath.Join(t.TempDir(), "say.log")
	bin := stubBinary(t, "say", log)
	tl := NewTool(Settings{Voice: "Samantha", Rate: 180})
	tl.Backend = sayBackend{} // the stub binary is named `say`, whatever the host resolves
	tl.Lookup = func(string) (string, error) { return bin, nil }

	res := execTool(t, tl, `{"text":"Configured voice."}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "spoke 1 chunk(s) via say") {
		t.Fatalf("text = %q", res.Text)
	}
	if line := readLines(t, log)[0]; !strings.Contains(line, "-v Samantha -r 180 -- Configured voice.") {
		t.Fatalf("settings were not passed through: %q", line)
	}

	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	res = execTool(t, tl, `{"text":"Overridden.","voice":"Alex","rate":300}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if line := readLines(t, log)[0]; !strings.Contains(line, "-v Alex -r 300 -- Overridden.") {
		t.Fatalf("per-call override lost: %q", line)
	}
}

// --- xdev say ---

func TestSayDryRunPrintsPlan(t *testing.T) {
	// Say resolves the backend from the host (ForGOOS), so the argv spelling is
	// the host's own; what this pins is the CLI's plan line around it: one line,
	// `[n/total]` prefix, the configured rate merged in, text after `--`.
	host, err := ForGOOS("", nil)
	if err != nil {
		t.Fatalf("ForGOOS on the host: %v", err)
	}
	want := "[1/1] " + strings.Join(append([]string{host.Name()}, host.Args("One. Two.", Request{Rate: 200})...), " ") + "\n"
	var out, errBuf strings.Builder
	code := Say([]string{"--dry-run", "One.", "Two."}, &out, &errBuf, Settings{Rate: 200})
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errBuf.String())
	}
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
	if !strings.HasSuffix(out.String(), " -- One. Two.\n") {
		t.Fatalf("plan must quote the text after the -- separator: %q", out.String())
	}
}

func TestSayUsageErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "no text", args: nil},
		{name: "blank text", args: []string{"  "}},
		{name: "rate out of range", args: []string{"--rate", "10", "hi"}},
		{name: "unknown flag", args: []string{"--nope", "hi"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errBuf strings.Builder
			if code := Say(tt.args, &out, &errBuf, Settings{}); code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr %q)", code, errBuf.String())
			}
		})
	}
}

func TestSayMissingBinaryExplains(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("missing-binary path asserted on the backends this test can force")
	}
	t.Setenv("PATH", t.TempDir()) // no say/spd-say/espeak-ng anywhere
	var out, errBuf strings.Builder
	code := Say([]string{"hello"}, &out, &errBuf, Settings{})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q)", code, out.String())
	}
	if !strings.Contains(errBuf.String(), "not found on PATH") {
		t.Fatalf("stderr = %q", errBuf.String())
	}
}

func TestSaySpeaksThroughStubbedBinary(t *testing.T) {
	log := filepath.Join(t.TempDir(), "say.log")
	bin := stubBinary(t, "say", log)
	t.Setenv("PATH", filepath.Dir(bin))
	var out, errBuf strings.Builder
	code := Say([]string{"hello", "there"}, &out, &errBuf, Settings{})
	if runtime.GOOS != "darwin" {
		// On other GOOS the stubbed name is not the backend's binary; only the
		// configured-backend cases above are meaningful there.
		t.Skip("stub is named `say`, which is the darwin backend")
	}
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "spoke 1 chunk(s) via say") {
		t.Fatalf("stdout = %q", out.String())
	}
	if lines := readLines(t, log); len(lines) != 1 || !strings.HasSuffix(lines[0], "hello there") {
		t.Fatalf("stub log = %q", lines)
	}
}

func TestTruncateBytesKeepsRunesIntact(t *testing.T) {
	text := strings.Repeat("é", 10) // 2 bytes per rune
	got, cut := truncateBytes(text, 5)
	if !cut {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a rune: %q", got)
	}
	if got != "éé" {
		t.Fatalf("got %q, want 2 runes", got)
	}
}

func TestSpeakerNewOnHost(t *testing.T) {
	sp, err := New()
	if err != nil {
		// Unsupported GOOS is legitimate; the error must still be actionable.
		if !strings.Contains(err.Error(), "no speech backend") {
			t.Fatalf("New() error = %v", err)
		}
		return
	}
	if sp.backend == nil || sp.backend.Name() == "" {
		t.Fatalf("New() returned an empty speaker: %+v", sp)
	}
}
