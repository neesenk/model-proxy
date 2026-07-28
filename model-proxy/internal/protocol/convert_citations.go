package protocol

import (
	"strings"
	"unicode/utf8"
)

// anthropicCitationsToResponses maps the citation forms that have an exact
// Responses annotation equivalent. Output offsets cover the generated
// Anthropic text block carrying the citation; Anthropic citations identify
// their source span, while Responses annotations identify the cited output
// span.
func anthropicCitationsToResponses(raw any, text string) []map[string]any {
	citations, _ := raw.([]any)
	var out []map[string]any
	for _, value := range citations {
		citation := asMap(value)
		switch strOpt(citation["type"]) {
		case "web_search_result_location", "search_result_location":
			url := firstNonEmpty(strOpt(citation["url"]), strOpt(citation["source"]))
			if url == "" {
				continue
			}
			out = append(out, map[string]any{
				"type":        "url_citation",
				"url":         url,
				"title":       strOpt(citation["title"]),
				"start_index": 0,
				"end_index":   utf8.RuneCountInString(text),
			})
		case "content_block_location", "char_location", "page_location":
			if fileID := strOpt(citation["file_id"]); fileID != "" {
				out = append(out, map[string]any{
					"type":     "file_citation",
					"file_id":  fileID,
					"filename": strOpt(citation["document_title"]),
					"index":    intOf(citation["document_index"]),
				})
			}
		}
	}
	return out
}

// anthropicCitationsToChat maps URL-backed Anthropic citations to the Chat
// Completions message.annotations schema. startOffset is the rune offset of
// the Anthropic text block in the concatenated Chat message.
func anthropicCitationsToChat(raw any, text string, startOffset int) []map[string]any {
	citations, _ := raw.([]any)
	var out []map[string]any
	for _, value := range citations {
		citation := asMap(value)
		switch strOpt(citation["type"]) {
		case "web_search_result_location", "search_result_location":
			url := firstNonEmpty(strOpt(citation["url"]), strOpt(citation["source"]))
			if url == "" {
				continue
			}
			out = append(out, map[string]any{
				"type": "url_citation",
				"url_citation": map[string]any{
					"url":         url,
					"title":       strOpt(citation["title"]),
					"start_index": startOffset,
					"end_index":   startOffset + utf8.RuneCountInString(text),
				},
			})
		}
	}
	return out
}

// chatAnnotationsToResponses removes Chat's url_citation wrapper and returns
// the flat annotation objects used by Responses output_text.
func chatAnnotationsToResponses(raw any) []map[string]any {
	annotations, _ := raw.([]any)
	var out []map[string]any
	for _, value := range annotations {
		annotation := asMap(value)
		if strOpt(annotation["type"]) != "url_citation" {
			continue
		}
		citation := asMap(annotation["url_citation"])
		if citation == nil || strOpt(citation["url"]) == "" {
			continue
		}
		item := map[string]any{"type": "url_citation"}
		copyOpt(item, citation, "url", "title", "start_index", "end_index")
		out = append(out, item)
	}
	return out
}

// responsesAnnotationsToChat adds Chat's url_citation wrapper around flat
// Responses annotations. File-only annotations have no Chat equivalent.
func responsesAnnotationsToChat(raw any) []map[string]any {
	annotations, _ := raw.([]any)
	var out []map[string]any
	for _, value := range annotations {
		annotation := asMap(value)
		if strOpt(annotation["type"]) != "url_citation" || strOpt(annotation["url"]) == "" {
			continue
		}
		citation := map[string]any{}
		copyOpt(citation, annotation, "url", "title", "start_index", "end_index")
		out = append(out, map[string]any{"type": "url_citation", "url_citation": citation})
	}
	return out
}

// responsesTextWithCitationLinks preserves citations when converting to
// Anthropic, whose replayable web citation requires an encrypted_index that
// Responses does not provide. Appending de-duplicated Markdown links is a
// valid, user-visible fallback and avoids fabricating an invalid structured
// Anthropic citation.
func responsesTextWithCitationLinks(part map[string]any) string {
	text := firstNonEmpty(strOpt(part["text"]), strOpt(part["refusal"]))
	raw, _ := part["annotations"].([]any)
	annotations := make([]map[string]any, 0, len(raw))
	for _, value := range raw {
		if annotation := asMap(value); annotation != nil {
			annotations = append(annotations, annotation)
		}
	}
	links := responsesCitationLinks(annotations, nil)
	if links == "" {
		return text
	}
	if text == "" {
		return links
	}
	return text + "\n\nSources: " + links
}

// responsesCitationLinks renders de-duplicated Markdown source links for
// url_citation annotations. seen (nil = fresh set) carries the dedup state, so
// streaming converters can pass a per-block/stream set and cite a repeated
// URL only once instead of appending one link per annotation event.
func responsesCitationLinks(annotations []map[string]any, seen map[string]bool) string {
	if seen == nil {
		seen = map[string]bool{}
	}
	var links []string
	for _, annotation := range annotations {
		if strOpt(annotation["type"]) != "url_citation" {
			continue
		}
		url := strOpt(annotation["url"])
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		title := firstNonEmpty(strOpt(annotation["title"]), url)
		links = append(links, "["+title+"]("+url+")")
	}
	return strings.Join(links, " ")
}
