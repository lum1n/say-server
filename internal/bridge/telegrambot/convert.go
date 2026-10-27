package telegrambot

import (
	"cmp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/lum1n/say-server/internal/bridge"
)

// update is one entry of getUpdates, of the kinds asked for.
type update struct {
	UpdateID          int64    `json:"update_id"`
	Message           *message `json:"message"`
	EditedMessage     *message `json:"edited_message"`
	ChannelPost       *message `json:"channel_post"`
	EditedChannelPost *message `json:"edited_channel_post"`
}

type user struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"` // private, group, supergroup, channel
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type message struct {
	MessageID       int64  `json:"message_id"`
	MessageThreadID int64  `json:"message_thread_id"`
	IsTopicMessage  bool   `json:"is_topic_message"`
	From            *user  `json:"from"`
	SenderChat      *chat  `json:"sender_chat"`
	Chat            chat   `json:"chat"`
	Date            int64  `json:"date"`
	Text            string `json:"text"`
	Caption         string `json:"caption"`
	ReplyToMessage  *struct {
		MessageID int64 `json:"message_id"`
	} `json:"reply_to_message"`

	Photo     []struct{} `json:"photo"`
	Animation *struct{}  `json:"animation"`
	Video     *struct{}  `json:"video"`
	VideoNote *struct{}  `json:"video_note"`
	Voice     *struct{}  `json:"voice"`
	Location  *struct{}  `json:"location"`
	Audio     *struct {
		Title    string `json:"title"`
		FileName string `json:"file_name"`
	} `json:"audio"`
	Document *struct {
		FileName string `json:"file_name"`
	} `json:"document"`
	Sticker *struct {
		Emoji string `json:"emoji"`
	} `json:"sticker"`
	Venue *struct {
		Title string `json:"title"`
	} `json:"venue"`
	Contact *struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	} `json:"contact"`
	Poll *struct {
		Question string `json:"question"`
	} `json:"poll"`
	Dice *struct {
		Emoji string `json:"emoji"`
	} `json:"dice"`
}

// incoming converts a message; false means it is not shown, such as a
// member joining.
func incoming(m *message, edit bool, botID int64) (bridge.Incoming, bool) {
	text := cmp.Or(m.Text, m.Caption, placeholder(m))
	if text == "" || m.MessageID == 0 {
		return bridge.Incoming{}, false
	}
	in := bridge.Incoming{
		Chat:   remoteChat(m.Chat),
		ID:     strconv.FormatInt(m.MessageID, 10),
		Author: author(m, botID),
		Text:   text,
		Edit:   edit,
	}
	if m.Date > 0 {
		in.Time = time.Unix(m.Date, 0)
	}
	// In a forum topic every message answers the topic's first one.
	if r := m.ReplyToMessage; r != nil && !(m.IsTopicMessage && r.MessageID == m.MessageThreadID) {
		in.ReplyTo = strconv.FormatInt(r.MessageID, 10)
	}
	return in, true
}

// author is who wrote a message: the channel or group it was sent as,
// else the user.
func author(m *message, botID int64) bridge.Author {
	if s := m.SenderChat; s != nil {
		return bridge.Author{ID: strconv.FormatInt(s.ID, 10), Name: chatName(*s)}
	}
	if u := m.From; u != nil {
		name := strings.TrimSpace(u.FirstName + " " + u.LastName)
		return bridge.Author{ID: strconv.FormatInt(u.ID, 10), Name: cmp.Or(name, u.Username, strconv.FormatInt(u.ID, 10)), Self: u.ID == botID}
	}
	// Telegram always says who; this is only a fallback.
	return bridge.Author{ID: strconv.FormatInt(m.Chat.ID, 10), Name: chatName(m.Chat)}
}

// topicID is the id a forum topic is linked by, "<chat id>/<topic id>",
// for a message in a topic; "" for others, such as those in General.
func topicID(m *message) string {
	if !m.IsTopicMessage || m.MessageThreadID == 0 {
		return ""
	}
	return strconv.FormatInt(m.Chat.ID, 10) + "/" + strconv.FormatInt(m.MessageThreadID, 10)
}

// splitChat parses a linked chat id into the chat and its topic, 0 when
// it is a whole chat.
func splitChat(id string) (chat string, topic int64) {
	chat, t, ok := strings.Cut(id, "/")
	if !ok {
		return id, 0
	}
	topic, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return id, 0
	}
	return chat, topic
}

func remoteChat(c chat) bridge.RemoteChat {
	return bridge.RemoteChat{ID: strconv.FormatInt(c.ID, 10), Name: chatName(c), Direct: c.Type == "private"}
}

func chatName(c chat) string {
	return cmp.Or(c.Title, strings.TrimSpace(c.FirstName+" "+c.LastName), c.Username, strconv.FormatInt(c.ID, 10))
}

// placeholder stands for media sent without a caption.
func placeholder(m *message) string {
	switch {
	case m.Sticker != nil:
		return strings.TrimSpace("[sticker " + m.Sticker.Emoji + "]")
	case m.Animation != nil:
		return "[GIF]"
	case len(m.Photo) > 0:
		return "[photo]"
	case m.Video != nil:
		return "[video]"
	case m.VideoNote != nil:
		return "[video message]"
	case m.Voice != nil:
		return "[voice message]"
	case m.Audio != nil:
		return named("audio", cmp.Or(m.Audio.Title, m.Audio.FileName))
	case m.Document != nil:
		return named("document", m.Document.FileName)
	case m.Venue != nil:
		return named("venue", m.Venue.Title)
	case m.Location != nil:
		return "[location]"
	case m.Contact != nil:
		return named("contact", strings.TrimSpace(m.Contact.FirstName+" "+m.Contact.LastName))
	case m.Poll != nil:
		return named("poll", m.Poll.Question)
	case m.Dice != nil:
		return strings.TrimSpace("[dice " + m.Dice.Emoji + "]")
	}
	return ""
}

func named(kind, name string) string {
	if name == "" {
		return "[" + kind + "]"
	}
	return "[" + kind + ": " + name + "]"
}

// maxText is the most a message may hold, in UTF-16 units of the text
// as shown (after HTML is parsed).
const maxText = 4096

// maxSender caps a sender's name, in runes.
const maxSender = 100

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

// render formats a relayed message as HTML: the sender in bold.
func render(sender, text string) string {
	if sender == "" {
		return htmlEscaper.Replace(text)
	}
	return "<b>" + htmlEscaper.Replace(sender) + "</b>: " + htmlEscaper.Replace(text)
}

// prefixLen is how much of a message the sender's name takes.
func prefixLen(sender string) int {
	if sender == "" {
		return 0
	}
	return utf16Len(sender + ": ")
}

// parts formats a message as one or more Telegram messages that each fit,
// each with the sender's name.
func parts(sender, text string) []string {
	sender = clip(sender)
	var out []string
	for _, p := range split(text, maxText-prefixLen(sender)) {
		out = append(out, render(sender, p))
	}
	if len(out) == 0 {
		out = append(out, render(sender, text))
	}
	return out
}

// single formats a message as one Telegram message, cut short to fit.
func single(sender, text string) string {
	sender = clip(sender)
	limit := maxText - prefixLen(sender)
	if utf16Len(text) > limit {
		text = strings.TrimRight(split(text, limit-1)[0], " \n") + "…"
	}
	return render(sender, text)
}

func clip(sender string) string {
	if r := []rune(sender); len(r) > maxSender {
		return string(r[:maxSender-1]) + "…"
	}
	return sender
}

// split cuts text into pieces of at most limit UTF-16 units, at a line
// break or a space where one is near the end. Empty pieces are dropped.
func split(text string, limit int) []string {
	var out []string
	add := func(s string) {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	for utf16Len(text) > limit {
		cut, n := len(text), 0
		for i, r := range text {
			w := max(utf16.RuneLen(r), 1)
			if n+w > limit {
				cut = i
				break
			}
			n += w
		}
		head := text[:cut]
		if j := strings.LastIndexByte(head, '\n'); j > len(head)/2 {
			cut = j + 1
		} else if j := strings.LastIndexByte(head, ' '); j > len(head)/2 {
			cut = j + 1
		}
		if cut == 0 {
			// limit is below one character.
			break
		}
		add(strings.TrimRight(text[:cut], " \n"))
		text = text[cut:]
	}
	add(text)
	return out
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += max(utf16.RuneLen(r), 1)
	}
	return n
}
