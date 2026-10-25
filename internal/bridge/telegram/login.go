package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/lum1n/say-v2/internal/bridge"
)

// authClient is the part of gotd's auth.Client a login uses.
type authClient interface {
	Status(ctx context.Context) (*auth.Status, error)
	SendCode(ctx context.Context, phone string, opt auth.SendCodeOptions) (tg.AuthSentCodeClass, error)
	SignIn(ctx context.Context, phone, code, codeHash string) (*tg.AuthAuthorization, error)
	Password(ctx context.Context, password string) (*tg.AuthAuthorization, error)
}

// tries is how often the owner may mistype a code or password before the
// login starts over.
const tries = 3

// login signs the session in to the account on phone, unless it is
// already: Telegram sends a code, which the owner types in say, then the
// password if two-step verification is on. Accounts are not signed up
// here; that takes a Telegram app.
func login(ctx context.Context, ac authClient, phone string, h bridge.Host) error {
	status, err := ac.Status(ctx)
	if err != nil {
		return fmt.Errorf("telegram: auth status: %w", err)
	}
	if status.Authorized {
		return nil
	}
	sent, err := ac.SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return fmt.Errorf("telegram: send code: %w", err)
	}
	code, ok := sent.(*tg.AuthSentCode)
	if !ok {
		// Signed in without a code.
		return nil
	}
	text := "Telegram sent a code to " + phone + codeVia(code.Type) + ". Type it here."
	for try := 1; ; try++ {
		answer, err := h.Ask(ctx, bridge.Prompt{Kind: "code", Text: text})
		if err != nil {
			return err
		}
		_, err = ac.SignIn(ctx, phone, strings.Join(strings.Fields(answer), ""), code.PhoneCodeHash)
		var signUp *auth.SignUpRequired
		switch {
		case err == nil:
			return nil
		case errors.Is(err, auth.ErrPasswordAuthNeeded):
			return password(ctx, ac, h)
		case errors.As(err, &signUp):
			return errors.New("telegram: this phone number has no Telegram account; sign up in a Telegram app first")
		case tgerr.Is(err, "PHONE_CODE_INVALID", "PHONE_CODE_EMPTY") && try < tries:
			text = "That code was wrong. Type the code Telegram sent to " + phone + "."
		default:
			return fmt.Errorf("telegram: sign in: %w", err)
		}
	}
}

// password finishes a login with the two-step verification password.
func password(ctx context.Context, ac authClient, h bridge.Host) error {
	text := "Your Telegram account has two-step verification. Type its password."
	for try := 1; ; try++ {
		pw, err := h.Ask(ctx, bridge.Prompt{Kind: "password", Text: text})
		if err != nil {
			return err
		}
		_, err = ac.Password(ctx, pw)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, auth.ErrPasswordInvalid) && try < tries:
			text = "That password was wrong. Type your Telegram two-step verification password."
		default:
			return fmt.Errorf("telegram: password: %w", err)
		}
	}
}

// codeVia says how a login code was sent.
func codeVia(t tg.AuthSentCodeTypeClass) string {
	switch t.(type) {
	case *tg.AuthSentCodeTypeApp:
		return " in your Telegram app"
	case *tg.AuthSentCodeTypeSMS:
		return " by SMS"
	case *tg.AuthSentCodeTypeCall, *tg.AuthSentCodeTypeFlashCall, *tg.AuthSentCodeTypeMissedCall:
		return " by a phone call"
	case *tg.AuthSentCodeTypeEmailCode:
		return " by email"
	}
	return ""
}
