package imagegen

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The two adapters behind generate_image. Each one is a request builder plus
// a decoder for that provider's real response shape; the chain, credentials,
// caps, and download plumbing live in imagegen.go and are shared.

// Provider API roots, used when a Provider entry names no BaseURL. The
// adapters take the root as a parameter rather than reading a package
// variable, so a test server (or a gateway) is just a BaseURL override and
// parallel tests cannot leak into each other.
const (
	openAIBaseURL = "https://api.openai.com/v1"
	geminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"
)

// imagenMarker in a model name selects the Imagen `:predict` endpoint
// instead of generateContent. Imagen takes no responseModalities: its whole
// answer is images, so the model name decides the call rather than a second
// settings key.
const imagenMarker = "imagen"

// openAIRequest is the images/generations body.
type openAIRequest struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	N       int    `json:"n"`
	Size    string `json:"size,omitempty"`
	Quality string `json:"quality,omitempty"`
}

// openAIResponse is the images/generations answer: base64 by default, or a
// URL when the model answers with one.
type openAIResponse struct {
	Data []struct {
		B64JSON       string `json:"b64_json"`
		URL           string `json:"url"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
}

// openai generates through POST /images/generations.
func (g *Generator) openai(ctx context.Context, p Provider, base, key, prompt string, req Request) (Result, error) {
	endpoint := base + "/images/generations"
	model := p.model(DefaultOpenAIModel)
	body := openAIRequest{
		Model:   model,
		Prompt:  prompt,
		N:       1,
		Size:    strings.TrimSpace(req.Size),
		Quality: strings.TrimSpace(req.Quality),
	}
	raw, err := g.post(ctx, endpoint, map[string]string{"authorization": "Bearer " + key}, body)
	if err != nil {
		return Result{}, err
	}
	var parsed openAIResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Result{}, fmt.Errorf("unreadable response: %w", err)
	}
	images := make([]Image, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		img, err := g.decodeImage(ctx, d.B64JSON, d.URL, "image/png", d.RevisedPrompt)
		if err != nil {
			return Result{}, err
		}
		images = append(images, img)
	}
	if len(images) == 0 {
		return Result{}, fmt.Errorf("the response carried no image (model %s)", model)
	}
	return Result{Provider: OpenAI, Model: model, Images: images}, nil
}

// geminiRequest is the generateContent body used for the image-capable Gemini
// models (gemini-2.5-flash-image and friends).
type geminiRequest struct {
	Contents         []geminiContent  `json:"contents"`
	GenerationConfig *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenConfig struct {
	ResponseModalities []string           `json:"responseModalities,omitempty"`
	ImageConfig        *geminiImageConfig `json:"imageConfig,omitempty"`
}

type geminiImageConfig struct {
	AspectRatio string `json:"aspectRatio,omitempty"`
}

// imagenRequest is the Imagen `:predict` body.
type imagenRequest struct {
	Instances  []imagenInstance  `json:"instances"`
	Parameters *imagenParameters `json:"parameters,omitempty"`
}

type imagenInstance struct {
	Prompt string `json:"prompt"`
}

type imagenParameters struct {
	SampleCount int    `json:"sampleCount"`
	AspectRatio string `json:"aspectRatio,omitempty"`
}

// geminiResponse covers both answers: generateContent carries inlineData (or
// a File API fileData URI), Imagen carries predictions.
type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text       string `json:"text"`
				InlineData *struct {
					MIMEType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
				FileData *struct {
					MIMEType string `json:"mimeType"`
					FileURI  string `json:"fileUri"`
				} `json:"fileData"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Predictions []struct {
		BytesBase64Encoded string `json:"bytesBase64Encoded"`
		MIMEType           string `json:"mimeType"`
		URI                string `json:"uri"`
	} `json:"predictions"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// gemini generates through generateContent, or through Imagen's `:predict`
// when the model name says Imagen. The key rides x-goog-api-key (the
// documented header; a query parameter would leak the key into logs).
func (g *Generator) gemini(ctx context.Context, p Provider, base, key, prompt string, req Request) (Result, error) {
	model := p.model(DefaultGeminiModel)
	aspect := strings.TrimSpace(req.Aspect)
	imagen := strings.Contains(strings.ToLower(model), imagenMarker)

	var (
		endpoint string
		body     any
	)
	if imagen {
		endpoint = base + "/models/" + model + ":predict"
		body = imagenRequest{
			Instances:  []imagenInstance{{Prompt: prompt}},
			Parameters: &imagenParameters{SampleCount: 1, AspectRatio: aspect},
		}
	} else {
		endpoint = base + "/models/" + model + ":generateContent"
		cfg := &geminiGenConfig{ResponseModalities: []string{"IMAGE"}}
		if aspect != "" {
			cfg.ImageConfig = &geminiImageConfig{AspectRatio: aspect}
		}
		body = geminiRequest{
			Contents:         []geminiContent{{Parts: []geminiPart{{Text: prompt}}}},
			GenerationConfig: cfg,
		}
	}

	raw, err := g.post(ctx, endpoint, map[string]string{"x-goog-api-key": key}, body)
	if err != nil {
		return Result{}, err
	}
	var parsed geminiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Result{}, fmt.Errorf("unreadable response: %w", err)
	}

	images := make([]Image, 0, 1)
	for _, pred := range parsed.Predictions {
		img, err := g.decodeImage(ctx, pred.BytesBase64Encoded, pred.URI, pred.MIMEType, "")
		if err != nil {
			return Result{}, err
		}
		images = append(images, img)
	}
	var text string
	for _, cand := range parsed.Candidates {
		for _, part := range cand.Content.Parts {
			if strings.TrimSpace(part.Text) != "" {
				text = part.Text
			}
			switch {
			case part.InlineData != nil:
				img, err := g.decodeImage(ctx, part.InlineData.Data, "", part.InlineData.MIMEType, "")
				if err != nil {
					return Result{}, err
				}
				images = append(images, img)
			case part.FileData != nil && strings.TrimSpace(part.FileData.FileURI) != "":
				img, err := g.decodeImage(ctx, "", part.FileData.FileURI, part.FileData.MIMEType, "")
				if err != nil {
					return Result{}, err
				}
				images = append(images, img)
			}
		}
	}
	if len(images) == 0 {
		// The provider answered with prose (a refusal, a safety block, or a
		// text-only model). Say so: "no image" alone is not actionable.
		reason := ""
		if parsed.PromptFeedback != nil && parsed.PromptFeedback.BlockReason != "" {
			reason = " (blocked: " + parsed.PromptFeedback.BlockReason + ")"
		} else if len(parsed.Candidates) > 0 && parsed.Candidates[0].FinishReason != "" {
			reason = " (finishReason: " + parsed.Candidates[0].FinishReason + ")"
		}
		if text != "" {
			return Result{}, fmt.Errorf("the response carried no image%s: %s", reason, truncate(text, maxErrBody))
		}
		return Result{}, fmt.Errorf("the response carried no image%s (model %s)", reason, model)
	}
	return Result{Provider: Gemini, Model: model, Images: images}, nil
}

// truncate caps a provider-authored string for an error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
