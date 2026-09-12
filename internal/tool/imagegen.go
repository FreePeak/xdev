package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/imagegen"
	"github.com/FreePeak/xdev/internal/session"
)

// The cloud call lives in that leaf package (internal/config aliases its
// Settings, so it cannot import this one); storage is the session blob
// store, so a generated image becomes a content-addressed session artifact
// exactly like any other attachment.
//
// A provider failure is an ordinary Result with IsError — never a crash and
// never a silent empty success: the error names the missing credential or
// quotes the provider's own message.

// ImageGenToolName is the registered tool name.
const ImageGenToolName = "generate_image"

// ImageGenTool generates one image from a prompt and stores it as a blob.
type ImageGenTool struct {
	Gen   *imagegen.Generator
	Blobs *session.BlobStore
}

// NewImageGenTool builds the tool from the layered imageProviders settings
// block, the credential lookup the harness wires (nil = environment only),
// and the session blob store the image is written into.
func NewImageGenTool(cfg imagegen.Settings, creds imagegen.CredentialLookup, blobs *session.BlobStore) *ImageGenTool {
	return &ImageGenTool{Gen: imagegen.New(cfg, creds), Blobs: blobs}
}

func (t *ImageGenTool) Name() string { return ImageGenToolName }

func (t *ImageGenTool) Description() string {
	return "generate an image from a text prompt through a cloud image provider and save it to disk"
}

func (t *ImageGenTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "subject": {"type": "string", "description": "main image prompt (required)"},
    "action": {"type": "string", "description": "what the subject is doing"},
    "scene": {"type": "string", "description": "location or environment"},
    "composition": {"type": "string", "description": "camera angle and framing"},
    "lighting": {"type": "string", "description": "lighting setup"},
    "style": {"type": "string", "description": "artistic style"},
    "text": {"type": "string", "description": "short text to render legibly in the image"},
    "aspect_ratio": {"type": "string", "enum": ["1:1", "3:4", "4:3", "9:16", "16:9"], "description": "requested aspect ratio where the provider supports it"},
    "image_size": {"type": "string", "enum": ["1024x1024", "1536x1024", "1024x1536"], "description": "requested output size (OpenAI); other providers use their default"},
    "quality": {"type": "string", "enum": ["low", "medium", "high"], "description": "requested quality where the provider supports it"},
    "provider": {"type": "string", "enum": ["auto", "openai", "gemini"], "description": "prefer one provider for this call; auto uses the configured order"}
  },
  "required": ["subject"]
}`)
}

type imageGenArgs struct {
	Subject     string `json:"subject"`
	Action      string `json:"action"`
	Scene       string `json:"scene"`
	Composition string `json:"composition"`
	Lighting    string `json:"lighting"`
	Style       string `json:"style"`
	Text        string `json:"text"`
	AspectRatio string `json:"aspect_ratio"`
	ImageSize   string `json:"image_size"`
	Quality     string `json:"quality"`
	Provider    string `json:"provider"`
}

func (t *ImageGenTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a imageGenArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Text: ImageGenToolName + ": malformed arguments: " + err.Error(), IsError: true}, nil
	}
	prompt := composeImagePrompt(a)
	if prompt == "" {
		return Result{Text: ImageGenToolName + ": subject is required (the main image prompt)", IsError: true}, nil
	}
	if t.Gen == nil || t.Blobs == nil {
		return Result{Text: ImageGenToolName + ": not configured (no image generator or blob store)", IsError: true}, nil
	}

	start := time.Now()
	res, err := t.Gen.Generate(ctx, imagegen.Request{
		Prompt:   prompt,
		Size:     a.ImageSize,
		Quality:  a.Quality,
		Aspect:   a.AspectRatio,
		Provider: normalizeProviderPref(a.Provider),
	})
	if err != nil {
		return Result{Text: ImageGenToolName + ": " + err.Error(), IsError: true}, nil
	}
	elapsed := time.Since(start).Round(time.Millisecond)

	images := make([]map[string]any, 0, len(res.Images))
	lines := make([]string, 0, len(res.Images))
	for _, img := range res.Images {
		ref, err := t.Blobs.Put(img.Data)
		if err != nil {
			return Result{Text: ImageGenToolName + ": storing the generated image failed: " + err.Error(), IsError: true}, nil
		}
		hexSum, err := session.ParseBlobRef(ref)
		if err != nil {
			return Result{Text: ImageGenToolName + ": " + err.Error(), IsError: true}, nil
		}
		// ponytail: the returned path is the blob store's content-addressed
		// file, so it carries no extension (read sniffs images by extension
		// and will list it as a plain file). Upgrade path: materialize
		// <dataDir>/images/<hex>.png once vision input needs a typed path.
		path := filepath.Join(t.Blobs.Root(), hexSum)
		mime := img.MIME
		if mime == "" {
			mime = http.DetectContentType(img.Data[:min(len(img.Data), 512)])
		}
		w, h := imageDimensions(img.Data)
		entry := map[string]any{
			"path":   path,
			"ref":    ref,
			"mime":   mime,
			"bytes":  len(img.Data),
			"width":  w,
			"height": h,
		}
		if img.URL != "" {
			entry["sourceUrl"] = img.URL
		}
		if img.RevisedPrompt != "" {
			entry["revisedPrompt"] = img.RevisedPrompt
		}
		images = append(images, entry)
		lines = append(lines, fmt.Sprintf("- %s (%s, %s, %d bytes)", path, describeSize(w, h), mime, len(img.Data)))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "generated %d image(s) with %s/%s in %s\n", len(res.Images), res.Provider, res.Model, elapsed)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	if res.CredSource != "" {
		fmt.Fprintf(&b, "credential: %s\n", res.CredSource)
	}
	for _, img := range res.Images {
		if img.RevisedPrompt != "" {
			fmt.Fprintf(&b, "revised prompt: %s\n", truncateText(img.RevisedPrompt, 300))
		}
	}

	return Result{
		Text: strings.TrimRight(b.String(), "\n"),
		Details: map[string]any{
			"provider":         res.Provider,
			"model":            res.Model,
			"credentialSource": res.CredSource,
			"prompt":           prompt,
			"images":           images,
			"elapsedMs":        elapsed.Milliseconds(),
			"totalBytes":       totalImageBytes(res.Images),
		},
	}, nil
}

// composeImagePrompt folds the compositional fields into one prompt. Labels
// keep each field's intent unambiguous; empty fields are omitted, so a bare
// subject stays a bare prompt.
func composeImagePrompt(a imageGenArgs) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(a.Subject))
	add := func(label, v string) {
		if v = strings.TrimSpace(v); v != "" {
			b.WriteString("\n" + label + ": " + v)
		}
	}
	add("Action", a.Action)
	add("Scene", a.Scene)
	add("Composition", a.Composition)
	add("Lighting", a.Lighting)
	add("Style", a.Style)
	add("Text in the image (rendered sharply and spelled correctly)", a.Text)
	return strings.TrimSpace(b.String())
}

// normalizeProviderPref maps the schema's "auto" (and any stray value) to the
// empty preference, which means the configured order.
func normalizeProviderPref(p string) string {
	if p == "auto" {
		return ""
	}
	return p
}

// imageDimensions reads the header of a decoded image; an unreadable format
// reports 0x0 rather than failing the call (the bytes are already stored).
func imageDimensions(data []byte) (int, int) {
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data[:min(len(data), 64<<10)])); err == nil {
		return cfg.Width, cfg.Height
	}
	return 0, 0
}

func describeSize(w, h int) string {
	if w <= 0 || h <= 0 {
		return "unknown size"
	}
	return fmt.Sprintf("%dx%d", w, h)
}

func totalImageBytes(images []imagegen.Image) int {
	n := 0
	for _, img := range images {
		n += len(img.Data)
	}
	return n
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
