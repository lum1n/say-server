package core

import (
	"encoding/json"
	"strings"
)

// Attachment is the data of an attachment part: a file uploaded to the
// room, named by its media id. URL, on reads, is a signed URL that serves
// the file without a token.
type Attachment struct {
	Media  string `json:"media"`
	Mime   string `json:"mime"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Part is the attachment as a message part.
func (a Attachment) Part() Part {
	data, _ := json.Marshal(a)
	return Part{Kind: PartAttachment, Data: data}
}

// Attachments are the message's attachment parts that name a file.
func (e Event) Attachments() []Attachment {
	var out []Attachment
	for _, p := range e.Parts {
		if p.Kind != PartAttachment {
			continue
		}
		var a Attachment
		if json.Unmarshal(p.Data, &a) == nil && a.Media != "" {
			out = append(out, a)
		}
	}
	return out
}

// Placeholder is a line of text that stands for the file where it can't
// be sent: "[image: name]" or "[file: name]".
func (a Attachment) Placeholder() string {
	kind := "file"
	if strings.HasPrefix(a.Mime, "image/") {
		kind = "image"
	}
	return "[" + kind + ": " + a.Name + "]"
}
