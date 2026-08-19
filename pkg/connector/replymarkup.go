// COMPANY ADDITION — not upstream. See docs/upstream-mautrix-changes.md.
//
// Render Telegram inline keyboards into the Matrix message body.
//
// Upstream drops `reply_markup` entirely: the protocol types exist in pkg/gotd/tg
// but nothing in pkg/connector reads them. The visible effect is that an agent sees
// a bot's prompt without its options — the customer is looking at three buttons and
// the agent cannot tell they exist, let alone what they say.
//
// WHAT IS PRESSABLE, AND WHAT DECIDES IT.
//
// Callback buttons ARE pressable — see buttonpress.go for the return path. A press
// executes an action on Telegram AS THE COMPANY ACCOUNT, so it is gated by a
// server-side allowlist of bots in the bridge config, not by the client. This file
// only decides which buttons are *eligible*; buttonpress.go decides whether a
// particular press is permitted, and re-derives the kind rather than trusting the
// client's claim.
//
// URL buttons become real links, because they add no reach: message text can already
// contain links and Element already makes those clickable.
//
// Two button types look like links but stay INERT and are not pressable at all:
//
//   - KeyboardButtonURLAuth ("Login URL") — following it authorises the bot to learn
//     the account's Telegram identity. On this deployment that account is the company
//     service account, so it must never be one click away.
//   - KeyboardButtonWebView / SimpleWebView — opens a bot-controlled mini app inside
//     the Telegram session context.
//
// Both are rendered as labels carrying their type, so an agent can see the button
// exists and ask an administrator rather than being silently blocked.

package connector

import (
	"fmt"
	"html"
	"strings"

	"go.mau.fi/mautrix-telegram/pkg/gotd/tg"
	"maunium.net/go/mautrix/event"
)

// renderedMarkup is the plain-text and HTML rendering of one inline keyboard,
// plus the machine-readable descriptor the client needs to draw pressable buttons.
type renderedMarkup struct {
	text string
	html string
	// buttons carries COORDINATES AND LABELS ONLY — never KeyboardButtonCallback.Data.
	// The callback payload stays on the Telegram side: when a press arrives, the
	// bridge re-fetches the message and reads the payload then. Putting it in Matrix
	// would publish, to every room member and to anyone reading the room later, the
	// exact bytes needed to drive the bot.
	buttons []buttonDescriptor
}

// buttonDescriptor is one entry of the com.company.telegram.buttons event field.
type buttonDescriptor struct {
	Row   int    `json:"row"`
	Col   int    `json:"col"`
	Label string `json:"label"`
	// callback | url | login | webapp | inline_query | game | payment | unsupported.
	// Only "callback" is pressable; the rest are rendered but inert, either because
	// they are already a link or because pressing them would authorise a bot against
	// the company Telegram identity.
	Kind string `json:"kind"`
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
	var descriptors []buttonDescriptor
	for rowIdx, row := range inline.Rows {
		var textButtons, htmlButtons []string
		for colIdx, button := range row.Buttons {
			t, h := renderButton(button)
			if t == "" {
				continue
			}
			textButtons = append(textButtons, t)
			htmlButtons = append(htmlButtons, h)
			if label, kind := describeButton(button); label != "" {
				descriptors = append(descriptors, buttonDescriptor{
					Row: rowIdx, Col: colIdx, Label: label, Kind: kind,
				})
			}
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
		text:    strings.Join(textRows, "\n"),
		html:    strings.Join(htmlRows, "<br/>"),
		buttons: descriptors,
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

// describeButton returns the label and kind for the client-facing descriptor.
//
// The kind is what decides pressability, and it is decided HERE rather than in the
// client so that a client bug cannot promote an inert button into a pressable one.
// The bridge re-checks the kind again when a press arrives.
func describeButton(button tg.KeyboardButtonClass) (label, kind string) {
	switch b := button.(type) {
	case *tg.KeyboardButtonCallback:
		return b.Text, "callback"
	case *tg.KeyboardButtonURL:
		return b.Text, "url"
	case *tg.KeyboardButtonURLAuth:
		return b.Text, "login"
	case *tg.KeyboardButtonWebView:
		return b.Text, "webapp"
	case *tg.KeyboardButtonSimpleWebView:
		return b.Text, "webapp"
	case *tg.KeyboardButtonSwitchInline:
		return b.Text, "inline_query"
	case *tg.KeyboardButtonGame:
		return b.Text, "game"
	case *tg.KeyboardButtonBuy:
		return b.Text, "payment"
	case *tg.KeyboardButton:
		return b.Text, "unsupported"
	default:
		if labeled, ok := button.(interface{ GetText() string }); ok {
			return labeled.GetText(), "unsupported"
		}
		return "", ""
	}
}
