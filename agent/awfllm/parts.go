package awfllm

import (
	"encoding/base64"

	"github.com/valbaudo/awf/agent"

	"github.com/openai/openai-go/v3"
)

// buildOpenAIParts builds a Chat Completions user content array: one part per
// file FIRST, then the text prompt LAST. The order is deliberate — the document
// is the stable, large content and the prompt (which on a gate repair attempt
// carries the varying prior verdict) is the suffix, so the document sits in the
// request's common prefix and OpenAI's automatic prompt caching can reuse it
// across a step's repair attempts (cached input is billed at the cache-read
// rate). PDFs use the `file` content part (file_data data URI); images use
// `image_url` (data URI). Verified: OpenAI file-inputs + prompt-caching guides.
func buildOpenAIParts(prompt string, files []agent.InputFile) ([]openai.ChatCompletionContentPartUnionParam, error) {
	parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(files)+1)
	for _, f := range files {
		// Accept/reject goes through the shared capability table; the ENCODING below
		// (data: URIs) stays OpenAI-specific.
		m, ok := forwardable(providerOpenAI, f.MIME)
		if !ok {
			return nil, unsupportedMIMEErr(f.MIME, "")
		}
		b64 := base64.StdEncoding.EncodeToString(f.Content)
		switch m {
		case modalityImage:
			parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
				URL: "data:" + f.MIME + ";base64," + b64,
			}))
		case modalityDocument:
			parts = append(parts, openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
				FileData: openai.String("data:application/pdf;base64," + b64),
				Filename: openai.String(f.Name),
			}))
		}
	}
	parts = append(parts, openai.TextContentPart(prompt))
	return parts, nil
}
