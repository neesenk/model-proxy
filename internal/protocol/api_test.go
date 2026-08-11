package protocol

import "testing"

func TestExtractResponseTextPreservesLegacyShapes(t *testing.T) {
	tests := []struct {
		name  string
		proto Protocol
		body  string
		want  string
	}{
		{
			name:  "openai string",
			proto: OpenAI,
			body:  `{"choices":[{"message":{"content":"chat text"}}]}`,
			want:  "chat text",
		},
		{
			name:  "openai content array remains empty",
			proto: OpenAI,
			body:  `{"choices":[{"message":{"content":[{"type":"text","text":"array text"}]}}]}`,
			want:  "",
		},
		{
			name:  "chat shape accepted with anthropic selector",
			proto: Anthropic,
			body:  `{"choices":[{"message":{"content":"chat fallback"}}]}`,
			want:  "chat fallback",
		},
		{
			name:  "anthropic shape accepted with openai selector",
			proto: OpenAI,
			body:  `{"content":[{"type":"text","text":"anthropic fallback"}]}`,
			want:  "anthropic fallback",
		},
		{
			name:  "responses citation",
			proto: Responses,
			body:  `{"output":[{"type":"message","content":[{"type":"output_text","text":"answer","annotations":[{"type":"url_citation","url":"https://example.test","title":"Example"}]}]}]}`,
			want:  "answer\n\nSources: [Example](https://example.test)",
		},
		{
			name:  "responses refusal",
			proto: Responses,
			body:  `{"output":[{"type":"message","content":[{"type":"refusal","refusal":"cannot comply"}]}]}`,
			want:  "cannot comply",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractResponseText([]byte(tc.body), tc.proto); got != tc.want {
				t.Fatalf("ExtractResponseText() = %q, want %q", got, tc.want)
			}
		})
	}
}
