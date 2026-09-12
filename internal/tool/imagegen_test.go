package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/imagegen"
	"github.com/FreePeak/xdev/internal/session"
)

func toolTestPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// clearImageEnv keeps the environment rung of the credential chain out of the
// test's way (a developer's real key must not decide the outcome).
func clearImageEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"OPENAI_API_KEY", "OPENAI_KEY", "GEMINI_API_KEY", "GEMINI_KEY"} {
		t.Setenv(k, "")
	}
}

// openAIImageServer answers one images/generations call with the given bytes
// and records the request body it received.
func openAIImageServer(t *testing.T, data []byte, got *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("authorization = %q", auth)
		}
		if got != nil {
			if err := json.NewDecoder(r.Body).Decode(got); err != nil {
				t.Errorf("decoding request body: %v", err)
			}
		}
		w.Header().Set("content-type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{
			"b64_json": base64.StdEncoding.EncodeToString(data),
		}}}); err != nil {
			t.Errorf("encoding response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// imageGenSettings points the OpenAI adapter at a test server with an inline
// credential, so the call never depends on the environment or on models.yml.
func imageGenSettings(baseURL string) imagegen.Settings {
	return imagegen.Settings{Providers: []imagegen.Provider{{
		Name: imagegen.OpenAI, BaseURL: baseURL, APIKey: "test-key",
	}}}
}

// TestImageGenToolStoresImageInBlobStore is the end-to-end seam test for M15
// #69: the composed prompt reaches the provider, the returned bytes land in
// the session blob store, and the model gets the path plus a summary.
func TestImageGenToolStoresImageInBlobStore(t *testing.T) {
	clearImageEnv(t)
	data := toolTestPNG(t)
	var body map[string]any
	srv := openAIImageServer(t, data, &body)

	blobs := session.NewBlobStore(t.TempDir())
	tool := NewImageGenTool(imageGenSettings(srv.URL), nil, blobs)
	res, err := tool.Execute(context.Background(), json.RawMessage(`{
		"subject": "a red cube",
		"style": "watercolor",
		"action": "floating",
		"image_size": "1024x1024",
		"quality": "high"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool failed: %s", res.Text)
	}
	prompt, _ := body["prompt"].(string)
	for _, want := range []string{"a red cube", "Style: watercolor", "Action: floating"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("composed prompt %q is missing %q", prompt, want)
		}
	}
	if body["size"] != "1024x1024" || body["quality"] != "high" {
		t.Fatalf("size/quality not forwarded: %v", body)
	}
	if body["n"] != float64(1) {
		t.Fatalf("n = %v, want 1", body["n"])
	}

	details, ok := res.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %#v", res.Details)
	}
	if details["provider"] != imagegen.OpenAI {
		t.Fatalf("provider = %v", details["provider"])
	}
	images, ok := details["images"].([]map[string]any)
	if !ok || len(images) != 1 {
		t.Fatalf("details.images = %#v", details["images"])
	}
	img := images[0]
	ref, _ := img["ref"].(string)
	if !strings.HasPrefix(ref, session.BlobRefPrefix) {
		t.Fatalf("blob ref = %q", ref)
	}
	path, _ := img["path"].(string)
	if path == "" || !filepath.IsAbs(path) {
		t.Fatalf("path = %q (want an absolute path)", path)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the returned path: %v", err)
	}
	if !bytes.Equal(onDisk, data) {
		t.Fatal("the file at the returned path is not the generated image")
	}
	if stored, err := blobs.Get(ref); err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("blob store round trip = %d bytes, %v", len(stored), err)
	}
	if img["mime"] != "image/png" || img["bytes"] != len(data) {
		t.Fatalf("image details = %#v", img)
	}
	if img["width"] != 2 || img["height"] != 3 {
		t.Fatalf("dimensions = %vx%v, want 2x3", img["width"], img["height"])
	}
	if !strings.Contains(res.Text, path) {
		t.Fatalf("summary %q does not name the saved path", res.Text)
	}
}

// TestImageGenToolRejectsOversizedResponse: the cap is a settings knob, and a
// response past it fails loudly instead of storing a truncated image.
func TestImageGenToolRejectsOversizedResponse(t *testing.T) {
	clearImageEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(bytes.Repeat([]byte("A"), 4096)); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	cfg := imageGenSettings(srv.URL)
	cfg.MaxBytes = 512
	tool := NewImageGenTool(cfg, nil, session.NewBlobStore(t.TempDir()))
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"subject":"a red cube"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "cap") {
		t.Fatalf("oversized response not rejected: %+v", res)
	}
}

// TestImageGenToolMissingCredentialIsActionable names the environment
// variable rather than reporting a generic failure.
func TestImageGenToolMissingCredentialIsActionable(t *testing.T) {
	clearImageEnv(t)
	tool := NewImageGenTool(imagegen.Settings{}, nil, session.NewBlobStore(t.TempDir()))
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"subject":"a red cube"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "OPENAI_API_KEY") {
		t.Fatalf("missing credential not actionable: %+v", res)
	}
	if !strings.Contains(res.Text, "imageProviders") {
		t.Fatalf("error does not name the config key: %+v", res)
	}
}

func TestImageGenToolRejectsBadArguments(t *testing.T) {
	blobs := session.NewBlobStore(t.TempDir())
	tool := NewImageGenTool(imagegen.Settings{}, nil, blobs)
	if res, err := tool.Execute(context.Background(), json.RawMessage(`{"subject":`)); err != nil || !res.IsError {
		t.Fatalf("malformed args = %+v, %v", res, err)
	}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "subject") {
		t.Fatalf("missing subject = %+v, %v", res, err)
	}
	if res, err := tool.Execute(context.Background(), json.RawMessage(`{"subject":"x"}`)); err != nil || !res.IsError {
		t.Fatalf("unconfigured tool = %+v, %v", res, err)
	}
}

func TestImageGenToolNameAndSchema(t *testing.T) {
	tool := NewImageGenTool(imagegen.Settings{}, nil, nil)
	if tool.Name() != ImageGenToolName {
		t.Fatalf("name = %q", tool.Name())
	}
	var schema struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatalf("schema does not parse: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "subject" {
		t.Fatalf("required = %v", schema.Required)
	}
	for _, k := range []string{"subject", "style", "aspect_ratio", "image_size", "quality", "provider"} {
		if _, ok := schema.Properties[k]; !ok {
			t.Fatalf("schema is missing %q", k)
		}
	}
}
