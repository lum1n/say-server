// Package networks registers every network say bridges to. Import it for
// its side effect.
//
// Telegram accounts (gotd/td) add about 10 MB to a binary, so the build tag
// notelegram leaves them out; embedded builds use it (docs/adr/0013-embedding.md).
package networks

import (
	_ "github.com/lum1n/say-v2/internal/bridge/discord"
	_ "github.com/lum1n/say-v2/internal/bridge/email"
	_ "github.com/lum1n/say-v2/internal/bridge/irc"
	_ "github.com/lum1n/say-v2/internal/bridge/matrix"
	_ "github.com/lum1n/say-v2/internal/bridge/say"
	_ "github.com/lum1n/say-v2/internal/bridge/signal"
	_ "github.com/lum1n/say-v2/internal/bridge/slack"
	_ "github.com/lum1n/say-v2/internal/bridge/telegrambot"
	_ "github.com/lum1n/say-v2/internal/bridge/webhook"
)
