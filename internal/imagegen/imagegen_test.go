package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pngBytes returns a real 2x2 PNG: MIME sniffing and header decoding need
// honest bytes, and a hand-written header would pass the tests while failing
// a viewer.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// clearEnv makes the environment rung deterministic: a developer's real
// OPENAI_API_KEY must never decide which provider a test hits.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"OPENAI_API_KEY", "OPENAI_KEY", "GEMINI_API_KEY", "GEMINI_KEY"} {
		t.Setenv(k, "")
	}
}

// keyFor is a credential lookup that answers for the named providers only.
func keyFor(providers ...string) CredentialLookup {
	return func(provider string) (string, string, error) {
		for _, p := range providers {
			if p == provider {
				return "k-" + p, "test", nil
			}
		}
		return "", "", errors.New("no credential for provider")
	}
}

// decodeBody reads the request body a handler received.
func decodeBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("decoding request body: %v", err)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encoding response: %v", err)
	}
}

type openAIReqBody struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	N       int    `json:"n"`
	Size    string `json:"size"`
	Quality string `json:"quality"`
}

type geminiReqBody struct {
	Contents         []struct{ Parts []struct{ Text string } } `json:"contents"`
	GenerationConfig struct {
		ResponseModalities []string `json:"responseModalities"`
		ImageConfig        struct {
			AspectRatio string `json:"aspectRatio"`
		} `json:"imageConfig"`
	} `json:"generationConfig"`
}

type imagenReqBody struct {
	Instances  []struct{ Prompt string } `json:"instances"`
	Parameters struct {
		SampleCount int    `json:"sampleCount"`
		AspectRatio string `json:"aspectRatio"`
	} `json:"parameters"`
}

func TestOpenAIBase64Response(t *testing.T) {
	clearEnv(t)
	want := pngBytes(t)
	var got openAIReqBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/images/generations" {
			t.Errorf("request = %s %s, want POST /images/generations", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer k-openai" {
			t.Errorf("authorization = %q", auth)
		}
		decodeBody(t, r, &got)
		writeJSON(t, w, map[string]any{"data": []map[string]any{{
			"b64_json":       base64.StdEncoding.EncodeToString(want),
			"revised_prompt": "a revised prompt",
		}}})
	}))
	defer srv.Close()

	g := New(Settings{Providers: []Provider{{Name: OpenAI, BaseURL: srv.URL}}}, keyFor(OpenAI))
	res, err := g.Generate(context.Background(), Request{
		Prompt: "a red cube", Size: "1536x1024", Quality: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != DefaultOpenAIModel || got.Prompt != "a red cube" || got.N != 1 {
		t.Fatalf("request body = %+v", got)
	}
	if got.Size != "1536x1024" || got.Quality != "high" {
		t.Fatalf("size/quality not passed through: %+v", got)
	}
	if res.Provider != OpenAI || res.Model != DefaultOpenAIModel || res.CredSource != "test" {
		t.Fatalf("result header = %+v", res)
	}
	if len(res.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(res.Images))
	}
	img := res.Images[0]
	if !bytes.Equal(img.Data, want) {
		t.Fatal("image bytes do not round-trip")
	}
	if img.MIME != "image/png" || img.RevisedPrompt != "a revised prompt" {
		t.Fatalf("image = %+v", img)
	}
}

func TestOpenAIURLResponseDownloads(t *testing.T) {
	clearEnv(t)
	want := pngBytes(t)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/images/generations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"data": []map[string]any{{"url": srv.URL + "/img/one.png"}}})
	})
	mux.HandleFunc("/img/one.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "image/png")
		w.Write(want)
	})

	g := New(Settings{Providers: []Provider{{Name: OpenAI, BaseURL: srv.URL}}}, keyFor(OpenAI))
	res, err := g.Generate(context.Background(), Request{Prompt: "a red cube"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 || !bytes.Equal(res.Images[0].Data, want) {
		t.Fatalf("downloaded image mismatch: %+v", res.Images)
	}
	if res.Images[0].URL != srv.URL+"/img/one.png" || res.Images[0].MIME != "image/png" {
		t.Fatalf("image = %+v", res.Images[0])
	}
}

func TestGeminiGenerateContentInlineImage(t *testing.T) {
	clearEnv(t)
	want := pngBytes(t)
	var got geminiReqBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/models/gemini-2.5-flash-image:generateContent"; r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		if k := r.Header.Get("x-goog-api-key"); k != "k-gemini" {
			t.Errorf("x-goog-api-key = %q", k)
		}
		decodeBody(t, r, &got)
		writeJSON(t, w, map[string]any{"candidates": []map[string]any{{
			"content": map[string]any{"parts": []map[string]any{{
				"inlineData": map[string]any{
					"mimeType": "image/png",
					"data":     base64.StdEncoding.EncodeToString(want),
				},
			}}},
		}}})
	}))
	defer srv.Close()

	g := New(Settings{Providers: []Provider{{Name: Gemini, BaseURL: srv.URL}}}, keyFor(Gemini))
	res, err := g.Generate(context.Background(), Request{Prompt: "a blue sphere", Aspect: "16:9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Contents) != 1 || got.Contents[0].Parts[0].Text != "a blue sphere" {
		t.Fatalf("contents = %+v", got.Contents)
	}
	if len(got.GenerationConfig.ResponseModalities) != 1 || got.GenerationConfig.ResponseModalities[0] != "IMAGE" {
		t.Fatalf("responseModalities = %v", got.GenerationConfig.ResponseModalities)
	}
	if got.GenerationConfig.ImageConfig.AspectRatio != "16:9" {
		t.Fatalf("aspectRatio = %q", got.GenerationConfig.ImageConfig.AspectRatio)
	}
	if res.Provider != Gemini || res.Model != DefaultGeminiModel {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Images) != 1 || !bytes.Equal(res.Images[0].Data, want) {
		t.Fatalf("image mismatch: %+v", res.Images)
	}
}

func TestGeminiFileURIDownload(t *testing.T) {
	clearEnv(t)
	want := pngBytes(t)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/models/gemini-2.5-flash-image:generateContent", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"candidates": []map[string]any{{
			"content": map[string]any{"parts": []map[string]any{{
				"fileData": map[string]any{"mimeType": "image/png", "fileUri": srv.URL + "/files/one"},
			}}},
		}}})
	})
	mux.HandleFunc("/files/one", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "image/png")
		w.Write(want)
	})

	g := New(Settings{Providers: []Provider{{Name: Gemini, BaseURL: srv.URL}}}, keyFor(Gemini))
	res, err := g.Generate(context.Background(), Request{Prompt: "a blue sphere"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 || !bytes.Equal(res.Images[0].Data, want) {
		t.Fatalf("downloaded image mismatch: %+v", res.Images)
	}
}

func TestGeminiImagenPredict(t *testing.T) {
	clearEnv(t)
	want := pngBytes(t)
	var got imagenReqBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/models/imagen-3.0-generate-002:predict"; r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		decodeBody(t, r, &got)
		writeJSON(t, w, map[string]any{"predictions": []map[string]any{{
			"bytesBase64Encoded": base64.StdEncoding.EncodeToString(want),
			"mimeType":           "image/png",
		}}})
	}))
	defer srv.Close()

	g := New(Settings{Providers: []Provider{{
		Name: Gemini, Model: "imagen-3.0-generate-002", BaseURL: srv.URL,
	}}}, keyFor(Gemini))
	res, err := g.Generate(context.Background(), Request{Prompt: "a green tree", Aspect: "3:4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Instances) != 1 || got.Instances[0].Prompt != "a green tree" {
		t.Fatalf("instances = %+v", got.Instances)
	}
	if got.Parameters.SampleCount != 1 || got.Parameters.AspectRatio != "3:4" {
		t.Fatalf("parameters = %+v", got.Parameters)
	}
	if res.Model != "imagen-3.0-generate-002" || len(res.Images) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestOversizedResponseIsRejected(t *testing.T) {
	clearEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Well past the cap: half a PNG must never be treated as a result.
		w.Write(bytes.Repeat([]byte("A"), 4096))
	}))
	defer srv.Close()

	g := New(Settings{
		Providers: []Provider{{Name: OpenAI, BaseURL: srv.URL}},
		MaxBytes:  1024,
	}, keyFor(OpenAI))
	_, err := g.Generate(context.Background(), Request{Prompt: "a red cube"})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("oversized response accepted: %v", err)
	}
}

func TestOversizedInlineImageIsRejected(t *testing.T) {
	clearEnv(t)
	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("B"), 4096))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"data": []map[string]any{{"b64_json": big}}})
	}))
	defer srv.Close()

	g := New(Settings{
		Providers: []Provider{{Name: OpenAI, BaseURL: srv.URL}},
		MaxBytes:  1024,
	}, keyFor(OpenAI))
	_, err := g.Generate(context.Background(), Request{Prompt: "a red cube"})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("oversized inline image accepted: %v", err)
	}
}

func TestMissingCredentialNamesTheEnvVar(t *testing.T) {
	clearEnv(t)
	g := New(Settings{}, nil)
	_, err := g.Generate(context.Background(), Request{Prompt: "a red cube"})
	if err == nil {
		t.Fatal("generation without a credential succeeded")
	}
	for _, want := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
}

func TestProviderErrorMessageIsQuoted(t *testing.T) {
	clearEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(t, w, map[string]any{"error": map[string]any{
			"message": "Billing hard limit has been reached",
		}})
	}))
	defer srv.Close()

	g := New(Settings{Providers: []Provider{{Name: OpenAI, BaseURL: srv.URL}}}, keyFor(OpenAI))
	_, err := g.Generate(context.Background(), Request{Prompt: "a red cube"})
	if err == nil || !strings.Contains(err.Error(), "Billing hard limit has been reached") {
		t.Fatalf("provider message lost: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("status lost: %v", err)
	}
}

func TestFailingProviderFallsThroughToTheNext(t *testing.T) {
	clearEnv(t)
	want := pngBytes(t)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := w.Write([]byte(`{"error":{"message":"upstream exploded"}}`)); err != nil {
			t.Error(err)
		}
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"predictions": []map[string]any{{
			"bytesBase64Encoded": base64.StdEncoding.EncodeToString(want),
		}}})
	}))
	defer second.Close()

	g := New(Settings{Providers: []Provider{
		{Name: OpenAI, BaseURL: first.URL},
		{Name: Gemini, BaseURL: second.URL},
	}}, keyFor(OpenAI, Gemini))
	res, err := g.Generate(context.Background(), Request{Prompt: "a red cube"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != Gemini {
		t.Fatalf("provider = %q, want the fallback to answer", res.Provider)
	}

	// Both down: the error keeps both reasons.
	g = New(Settings{Providers: []Provider{
		{Name: OpenAI, BaseURL: first.URL},
		{Name: Gemini, BaseURL: first.URL},
	}}, keyFor(OpenAI, Gemini))
	_, err = g.Generate(context.Background(), Request{Prompt: "a red cube"})
	if err == nil || !strings.Contains(err.Error(), "openai") || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("aggregate error = %v", err)
	}
}

func TestProviderPreferenceIsHonored(t *testing.T) {
	clearEnv(t)
	want := pngBytes(t)
	openaiHits := 0
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHits++
		writeJSON(t, w, map[string]any{"data": []map[string]any{{
			"b64_json": base64.StdEncoding.EncodeToString(want),
		}}})
	}))
	defer openai.Close()
	gemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"predictions": []map[string]any{{
			"bytesBase64Encoded": base64.StdEncoding.EncodeToString(want),
		}}})
	}))
	defer gemini.Close()

	g := New(Settings{Providers: []Provider{
		{Name: OpenAI, BaseURL: openai.URL},
		{Name: Gemini, BaseURL: gemini.URL},
	}}, keyFor(OpenAI, Gemini))
	res, err := g.Generate(context.Background(), Request{Prompt: "a red cube", Provider: "gemini"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != Gemini {
		t.Fatalf("provider = %q, want the requested preference", res.Provider)
	}
	if openaiHits != 0 {
		t.Fatalf("preferred provider was not tried first (%d openai hits)", openaiHits)
	}
}

func TestRefusesNonHTTPImageURL(t *testing.T) {
	clearEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"data": []map[string]any{{"url": "file:///etc/passwd"}}})
	}))
	defer srv.Close()

	g := New(Settings{Providers: []Provider{{Name: OpenAI, BaseURL: srv.URL}}}, keyFor(OpenAI))
	_, err := g.Generate(context.Background(), Request{Prompt: "a red cube"})
	if err == nil || !strings.Contains(err.Error(), "non-http") {
		t.Fatalf("non-http url followed: %v", err)
	}
}

func TestPromptIsRequiredAndCapped(t *testing.T) {
	clearEnv(t)
	g := New(Settings{}, keyFor(OpenAI))
	if _, err := g.Generate(context.Background(), Request{Prompt: "   "}); err == nil {
		t.Fatal("empty prompt accepted")
	}
	if _, err := g.Generate(context.Background(), Request{
		Prompt: strings.Repeat("x", MaxPromptBytes+1),
	}); err == nil {
		t.Fatal("over-long prompt accepted")
	}
}

func TestPerAttemptTimeoutBoundsTheCall(t *testing.T) {
	clearEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	g := New(Settings{
		Providers: []Provider{{Name: OpenAI, BaseURL: srv.URL}},
		Timeout:   "30ms",
	}, keyFor(OpenAI))
	start := time.Now()
	_, err := g.Generate(context.Background(), Request{Prompt: "a red cube"})
	if err == nil {
		t.Fatal("a hanging provider was allowed to answer")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout not enforced: call took %s", elapsed)
	}
}
