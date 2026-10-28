package telegrambot

import (
	"cmp"
	"context"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/lum1n/say-server/internal/bridge"
)

// update is one entry of getUpdates, of the kinds asked for.
type update struct {
	UpdateID          int64           `json:"update_id"`
	Message           *message        `json:"message"`
	EditedMessage     *message        `json:"edited_message"`
	ChannelPost       *message        `json:"channel_post"`
	EditedChannelPost *message        `json:"edited_channel_post"`
	MessageReaction   *reactionUpdate `json:"message_reaction"`
}

// reactionUpdate is a user changing their reactions to a message. Telegram
// sends it only to bots that are admins of the chat.
type reactionUpdate struct {
	Chat        chat           `json:"chat"`
	MessageID   int64          `json:"message_id"`
	User        *user          `json:"user"`
	ActorChat   *chat          `json:"actor_chat"`
	Date        int64          `json:"date"`
	OldReaction []reactionType `json:"old_reaction"`
	NewReaction []reactionType `json:"new_reaction"`
}

// reactionType is an emoji, or a custom or paid reaction, which say leaves
// out.
type reactionType struct {
	Type  string `json:"type"`
	Emoji string `json:"emoji"`
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

	Photo     []file    `json:"photo"`
	Animation *file     `json:"animation"`
	Video     *file     `json:"video"`
	VideoNote *file     `json:"video_note"`
	Voice     *file     `json:"voice"`
	Location  *struct{} `json:"location"`
	Audio     *struct {
		file
		Title string `json:"title"`
	} `json:"audio"`
	Document *file `json:"document"`
	Sticker  *struct {
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

// file is a photo size, or a document, video or other file.
type file struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// maxDownload is the largest file the Bot API lets a bot download.
const maxDownload = 20 << 20

// incoming converts a message; false means it is not shown, such as a
// member joining. Its file, if say takes it, is read with fetch; an edit
// carries none.
func incoming(m *message, edit bool, botID int64, fetch func(ctx context.Context, fileID string) (io.ReadCloser, error)) (bridge.Incoming, bool) {
	text := cmp.Or(m.Text, m.Caption)
	var files []bridge.Media
	if id, f, ok := attachment(m); ok {
		if !edit {
			f.Open = func(ctx context.Context) (io.ReadCloser, error) { return fetch(ctx, id) }
			files = append(files, f)
		}
	} else {
		text = cmp.Or(text, placeholder(m))
	}
	if text == "" && len(files) == 0 || m.MessageID == 0 {
		return bridge.Incoming{}, false
	}
	in := bridge.Incoming{
		Chat:   remoteChat(m.Chat),
		ID:     strconv.FormatInt(m.MessageID, 10),
		Author: author(m, botID),
		Text:   text,
		Media:  files,
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

// attachment is the file of a message that say takes in, and its id; false
// for none, or one too big for a bot to download, which stays a
// placeholder. Stickers stay placeholders too.
func attachment(m *message) (string, bridge.Media, bool) {
	var f file
	mime, name := "", ""
	switch {
	case len(m.Photo) > 0:
		// The sizes of a photo; the largest has it all.
		for _, p := range m.Photo {
			if p.Width*p.Height >= f.Width*f.Height {
				f = p
			}
		}
		mime, name = "image/jpeg", "photo.jpg"
	case m.Animation != nil:
		f, mime, name = *m.Animation, "video/mp4", "animation.mp4"
	case m.Video != nil:
		f, mime, name = *m.Video, "video/mp4", "video.mp4"
	case m.VideoNote != nil:
		f, mime, name = *m.VideoNote, "video/mp4", "video.mp4"
	case m.Voice != nil:
		f, mime, name = *m.Voice, "audio/ogg", "voice.ogg"
	case m.Audio != nil:
		f, name = m.Audio.file, "audio"
	case m.Document != nil:
		f, name = *m.Document, "document"
	default:
		return "", bridge.Media{}, false
	}
	if f.FileID == "" || f.FileSize > maxDownload {
		return "", bridge.Media{}, false
	}
	return f.FileID, bridge.Media{
		Mime: cmp.Or(f.MimeType, mime), Name: cmp.Or(f.FileName, name),
		Size: f.FileSize, Width: f.Width, Height: f.Height,
	}, true
}

// reactions converts a change of someone's reactions into the reactions
// taken back and those added.
func reactions(r *reactionUpdate, botID int64) []bridge.Incoming {
	a := bridge.Author{ID: strconv.FormatInt(r.Chat.ID, 10), Name: chatName(r.Chat)}
	switch {
	case r.ActorChat != nil:
		a = bridge.Author{ID: strconv.FormatInt(r.ActorChat.ID, 10), Name: chatName(*r.ActorChat)}
	case r.User != nil:
		a = author(&message{From: r.User, Chat: r.Chat}, botID)
	}
	emojis := func(ts []reactionType) map[string]bool {
		out := map[string]bool{}
		for _, t := range ts {
			if t.Type == "emoji" && t.Emoji != "" {
				out[t.Emoji] = true
			}
		}
		return out
	}
	old, now := emojis(r.OldReaction), emojis(r.NewReaction)
	var out []bridge.Incoming
	add := func(key string, removed bool) {
		in := bridge.Incoming{
			Chat:     remoteChat(r.Chat),
			Author:   a,
			Reaction: &bridge.IncomingReaction{Target: strconv.FormatInt(r.MessageID, 10), Key: key, Removed: removed},
		}
		if r.Date > 0 {
			in.Time = time.Unix(r.Date, 0)
		}
		out = append(out, in)
	}
	for _, t := range r.OldReaction {
		if old[t.Emoji] && !now[t.Emoji] {
			add(t.Emoji, true)
		}
	}
	for _, t := range r.NewReaction {
		if now[t.Emoji] && !old[t.Emoji] {
			add(t.Emoji, false)
		}
	}
	return out
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

// maxCaption is the most a file's caption may hold, as maxText.
const maxCaption = 1024

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

// single formats a message as one Telegram message, or caption, of at
// most limit, cut short to fit.
func single(sender, text string, limit int) string {
	sender = clip(sender)
	limit -= prefixLen(sender)
	if utf16Len(text) > limit {
		text = strings.TrimRight(split(text, limit-1)[0], " \n") + "…"
	}
	return render(sender, text)
}

// fits reports whether text and the sender's name fit in a caption.
func fits(sender, text string) bool {
	return prefixLen(clip(sender))+utf16Len(text) <= maxCaption
}

// caption formats the caption of a file: the text, cut short to fit, or
// the sender's name alone for a file without text.
func caption(sender, text string) string {
	if text == "" {
		if sender == "" {
			return ""
		}
		return "<b>" + htmlEscaper.Replace(clip(sender)) + "</b>"
	}
	return single(sender, text, maxCaption)
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
