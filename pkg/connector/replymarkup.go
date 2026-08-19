// COMPANY ADDITION — not upstream. See docs/upstream-mautrix-changes.md.
//
// Render Telegram inline keyboards into the Matrix message body.
//
// Upstream drops `reply_markup` entirely: the protocol types exist in pkg/gotd/tg
// but nothing in pkg/connector reads them. The visible effect is that an agent sees
// a bot's prompt without its options — the customer is looking at three buttons and
// the agent cannot tell they exist, let alone what they say.
//
// THIS IS RENDERING ONLY, AND THAT IS A DELIBERATE LIMIT.
//
// Callback buttons are shown as inert labels. Making them pressable is not a bigger
// version of this file: a press executes an action on Telegram AS THE COMPANY
// ACCOUNT, which is a new capability for agents and has to enter the §12 policy
// model explicitly — with a policy flag and an audit record — rather than arriving
// as a side effect of having drawn the buttons. Nothing here creates that path.
//
// URL buttons DO become real links, because they add no reach: message text can
// already contain links and Element already makes those clickable.
//
// Two button types look like links but are not treated as such:
//
//   - KeyboardButtonURLAuth ("Login URL") — following it authorises the bot to learn
//     the account's Telegram identity. On this deployment that account is the company
//     service account, so it must never be one click away.
//   - KeyboardButtonWebView / SimpleWebView — opens a bot-controlled mini app inside
//     the Telegram session context.
//
// Both are rendered as inert labels carrying their type, so an agent can see the
// button exists and ask an administrator rather than being silently blocked.

package connector

import (
	"fmt"
	"html"
	"strings"

	"go.mau.fi/mautrix-telegram/pkg/gotd/tg"
	"maunium.net/go/mautrix/event"
)

// renderedMarkup is the plain-text and HTML rendering of one inline keyboard.
type renderedMarkup struct {
	text string
	html string
}

// renderInlineKeyboard converts a message's inline keyboard to text and HTML.
//
// Returns ok=false when the message has no inline keyboard, so callers can skip
// touching the content at all. Only ReplyInlineMarkup is handled — that is the
// buttons-under-a-bot-message case. ReplyKeyboardMarkup (a custom keyboard offered
// to the user for their next reply) is a different interaction and is not rendered.
func renderInlineKeyboard(msg *tg.Message) (renderedMarkup, bool) {
	markup, ok := msg.GetReplyMarkup()
	if !ok {
		return renderedMarkup{}, false
	}
	inline, ok := markup.(*tg.ReplyInlineMarkup)
	if !ok || len(inline.Rows) == 0 {
		return renderedMarkup{}, false
	}

	var textRows, htmlRows []string
	for _, row := range inline.Rows {
		var textButtons, htmlButtons []string
		for _, button := range row.Buttons {
			t, h := renderButton(button)
			if t == "" {
				continue
			}
			textButtons = append(textButtons, t)
			htmlButtons = append(htmlButtons, h)
		}
		if len(textButtons) > 0 {
			// Telegram lays buttons out in rows; keeping one row per line preserves
			// the grouping the customer is looking at.
			textRows = append(textRows, strings.Join(textButtons, "  "))
			htmlRows = append(htmlRows, strings.Join(htmlButtons, "  "))
		}
	}
	if len(textRows) == 0 {
		return renderedMarkup{}, false
	}

	return renderedMarkup{
		text: strings.Join(textRows, "\n"),
		html: strings.Join(htmlRows, "<br/>"),
	}, true
}

// renderButton renders a single button. An empty text result means "skip".
func renderButton(button tg.KeyboardButtonClass) (text, htmlOut string) {
	switch b := button.(type) {
	case *tg.KeyboardButtonURL:
		// The one genuinely actionable case. Escaped as an attribute and as text —
		// the label comes from a bot and is not trusted markup.
		return fmt.Sprintf("%s → %s", b.Text, b.URL),
			fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(b.URL), html.EscapeString(b.Text))

	case *tg.KeyboardButtonURLAuth:
		return inertButton(b.Text, "login")
	case *tg.KeyboardButtonWebView:
		return inertButton(b.Text, "web app")
	case *tg.KeyboardButtonSimpleWebView:
		return inertButton(b.Text, "web app")

	case *tg.KeyboardButtonCallback:
		// Deliberately does not carry b.Data. The callback payload is the thing a
		// press would send back to the bot, and putting it in the room would invite
		// exactly the reverse channel this file avoids building.
		return inertButton(b.Text, "")
	case *tg.KeyboardButton:
		return inertButton(b.Text, "")
	case *tg.KeyboardButtonSwitchInline:
		return inertButton(b.Text, "inline query")
	case *tg.KeyboardButtonGame:
		return inertButton(b.Text, "game")
	case *tg.KeyboardButtonBuy:
		return inertButton(b.Text, "payment")

	default:
		// An unknown or future button type. Render the label if the type exposes one
		// rather than dropping it — a button the agent cannot identify is still
		// better than a button they cannot see.
		if labeled, ok := button.(interface{ GetText() string }); ok {
			return inertButton(labeled.GetText(), "unsupported")
		}
		return "", ""
	}
}

// inertButton renders a button an agent cannot press, optionally tagged with why.
func inertButton(label, kind string) (text, htmlOut string) {
	if label == "" {
		return "", ""
	}
	suffix := ""
	if kind != "" {
		suffix = " · " + kind
	}
	return fmt.Sprintf("[%s%s]", label, suffix),
		fmt.Sprintf("<code>[%s%s]</code>", html.EscapeString(label), html.EscapeString(suffix))
}

// appendInlineKeyboard attaches the rendering to an already-converted message part.
//
// Appends to the existing text rather than adding a message part, so one Telegram
// message stays one Matrix message and replies and edits keep pointing at the right
// event. Media parts are the exception: their Body is the filename, so appending
// there would corrupt the displayed name — those get their own trailing notice.
func appendInlineKeyboard(part *event.MessageEventContent, rendered renderedMarkup) {
	existingHTML := part.FormattedBody
	if existingHTML == "" {
		// The part was plain text; promote it so the buttons can carry markup.
		existingHTML = html.EscapeString(part.Body)
		existingHTML = strings.ReplaceAll(existingHTML, "\n", "<br/>")
	}

	part.Body = strings.TrimRight(part.Body, "\n") + "\n\n" + rendered.text
	part.Format = event.FormatHTML
	part.FormattedBody = existingHTML + "<br/><br/>" + rendered.html
}
