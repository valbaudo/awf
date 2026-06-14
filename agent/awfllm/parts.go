package awfllm

import (
	"encoding/base64"

	"github.com/valbaudo/awf/agent"

	"github.com/openai/openai-go/v3"
)

// buildOpenAIParts builds a Chat Completions user content array: the text prompt
// followed by one part per file. PDFs use the `file` content part (file_data data
// URI); images use `image_url` (data URI). Verified: OpenAI file-inputs guide.
func buildOpenAIParts(prompt string, files []agent.InputFile) ([]openai.ChatCompletionContentPartUnionParam, error) {
	parts := []openai.ChatCompletionContentPartUnionParam{openai.TextContentPart(prompt)}
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
	return parts, nil
}
