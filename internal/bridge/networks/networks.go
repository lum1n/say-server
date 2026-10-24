// Package networks registers every network say bridges to. Import it for
// its side effect.
package networks

import (
	_ "github.com/vegard/say-v2/internal/bridge/email"
	_ "github.com/vegard/say-v2/internal/bridge/irc"
	_ "github.com/vegard/say-v2/internal/bridge/matrix"
	_ "github.com/vegard/say-v2/internal/bridge/signal"
	_ "github.com/vegard/say-v2/internal/bridge/slack"
	_ "github.com/vegard/say-v2/internal/bridge/telegram"
)
