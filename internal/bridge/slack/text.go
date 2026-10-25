package slack

import (
	"strings"
)

// file is an attachment of a message.
type file struct {
	Name       string `json:"name"`
	Title      string `json:"title"`
	Permalink  string `json:"permalink"`
	URLPrivate string `json:"url_private"`
}

// plainText turns Slack's message markup into readable text: mentions
// become @Name, channels #name, links "label (url)", and the escaped &, <
// and > are restored. n names ids that come without a label. Files follow as
// "[file: name] url" lines.
func plainText(text string, files []file, n names) string {
	var b strings.Builder
	for {
		open := strings.IndexByte(text, '<')
		if open < 0 {
			break
		}
		end := strings.IndexByte(text[open:], '>')
		if end < 0 {
			break
		}
		b.WriteString(unescape(text[:open]))
		b.WriteString(token(text[open+1:open+end], n))
		text = text[open+end+1:]
	}
	b.WriteString(unescape(text))
	out := b.String()
	for _, f := range files {
		name := f.Name
		if name == "" {
			name = f.Title
		}
		line := "[file: " + name + "]"
		if u := f.Permalink; u != "" {
			line += " " + u
		} else if u := f.URLPrivate; u != "" {
			line += " " + u
		}
		if out != "" {
			out += "\n"
		}
		out += line
	}
	return out
}

// names resolves the ids in markup that come without a label.
type names struct {
	user    func(id string) string
	channel func(id string) string
}

// token renders what was between < and >.
func token(t string, n names) string {
	ref, label, hasLabel := strings.Cut(t, "|")
	label = unescape(label)
	switch {
	case strings.HasPrefix(ref, "@"):
		if hasLabel && label != "" {
			return "@" + strings.TrimPrefix(label, "@")
		}
		return "@" + n.user(ref[1:])
	case strings.HasPrefix(ref, "#"):
		if hasLabel && label != "" {
			return "#" + label
		}
		return "#" + n.channel(ref[1:])
	case strings.HasPrefix(ref, "!"):
		// Special mentions (<!here>, <!subteam^S1|@team>) and dates
		// (<!date^…|fallback>) carry their readable form as the label.
		if hasLabel && label != "" {
			return label
		}
		name, _, _ := strings.Cut(ref[1:], "^")
		return "@" + name
	}
	url := unescape(ref)
	if !hasLabel || label == "" || label == url || "mailto:"+label == url {
		if strings.HasPrefix(url, "mailto:") {
			return strings.TrimPrefix(url, "mailto:")
		}
		return url
	}
	return label + " (" + url + ")"
}

var unescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

func unescape(s string) string { return unescaper.Replace(s) }

// escaper is what Slack asks clients to escape in text they post, so that
// a literal < is not read as markup.
var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escape(s string) string { return escaper.Replace(s) }
