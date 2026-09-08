package slack

import (
	"encoding/json"
	"strings"
)

// flexText is a Block Kit text field.
//
// Slack encodes it either as a bare string or as a {type, text} object,
// depending on which block contains it - a context element uses the object
// form, a rich_text leaf uses the string. Accepting both is not defensive
// programming; it is the actual schema.
type flexText struct{ Text string }

func (f *flexText) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if b[0] == '"' {
		return json.Unmarshal(b, &f.Text)
	}
	var o struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	f.Text = o.Text
	return nil
}

// block is the subset of Slack's Block Kit that carries readable text.
//
// Only the text-bearing shapes are modelled. Block Kit is large and mostly
// layout, and a message's *content* lives in a handful of places - so
// extracting those beats either decoding the whole schema or, worse, rendering
// nothing.
type block struct {
	Type string `json:"type"`
	Text *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"text"`
	Fields []struct {
		Text string `json:"text"`
	} `json:"fields"`
	Elements []blockElement `json:"elements"`
}

// blockElement covers both context elements (flat) and rich_text sections
// (nested one level).
type blockElement struct {
	Type     string   `json:"type"`
	Text     flexText `json:"text"`
	Elements []struct {
		Type string   `json:"type"`
		Text flexText `json:"text"`
		URL  string   `json:"url"`
		Name string   `json:"name"`
	} `json:"elements"`
}

// blocksText flattens blocks into readable lines.
//
// It takes raw JSON and decodes leniently on purpose. Block Kit is
// undocumented at the edges and Slack extends it without notice, so a shape
// surprise must cost this message its pretty rendering - never the whole
// conversation, which is what a hard decode error would do.
func blocksText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err != nil {
		return ""
	}
	return flatten(bs)
}

func flatten(bs []block) string {
	var parts []string
	for _, b := range bs {
		switch b.Type {
		case "divider", "image", "actions":
			// Layout, or an interactive control with no reader value.
			continue
		}
		if b.Text != nil && b.Text.Text != "" {
			parts = append(parts, b.Text.Text)
		}
		for _, f := range b.Fields {
			if f.Text != "" {
				parts = append(parts, f.Text)
			}
		}
		for _, e := range b.Elements {
			if e.Text.Text != "" {
				parts = append(parts, e.Text.Text)
				continue
			}
			var inner []string
			for _, ie := range e.Elements {
				switch {
				case ie.Text.Text != "":
					inner = append(inner, ie.Text.Text)
				case ie.URL != "":
					inner = append(inner, ie.URL)
				case ie.Name != "":
					// An emoji element.
					inner = append(inner, ":"+ie.Name+":")
				}
			}
			if len(inner) > 0 {
				parts = append(parts, strings.Join(inner, ""))
			}
		}
	}
	return strings.Join(parts, "\n")
}

// uselessFallbacks are the placeholder strings Slack and its apps emit when
// there is no plain-text rendering. Showing one is worse than showing nothing,
// because it displaces the real content extracted from blocks.
var uselessFallbacks = map[string]bool{
	"[no preview available]": true,
	"[no text]":              true,
	"":                       true,
}

// bestText picks the most readable rendering of a message.
//
// Order matters: plain text when the sender provided it, then the blocks that
// actually carry the content, then an attachment's own text, and only then a
// fallback - which for a great many app posts is a placeholder.
func bestText(text string, blocks json.RawMessage, atts []rawAttachment) string {
	if text != "" {
		return text
	}
	if s := blocksText(blocks); s != "" {
		return s
	}
	for _, a := range atts {
		if s := blocksText(a.Blocks); s != "" {
			return s
		}
		if a.Text != "" {
			return a.Text
		}
		if a.Title != "" {
			return a.Title
		}
	}
	for _, a := range atts {
		if !uselessFallbacks[strings.TrimSpace(a.Fallback)] {
			return a.Fallback
		}
	}
	return ""
}
