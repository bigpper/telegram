// COMPANY ADDITION — not upstream. See docs/upstream-mautrix-changes.md.

package connector

import (
	"strings"
	"testing"

	"go.mau.fi/mautrix-telegram/pkg/gotd/tg"
	"maunium.net/go/mautrix/event"
)

func inlineMsg(rows ...[]tg.KeyboardButtonClass) *tg.Message {
	markup := &tg.ReplyInlineMarkup{}
	for _, r := range rows {
		markup.Rows = append(markup.Rows, tg.KeyboardButtonRow{Buttons: r})
	}
	msg := &tg.Message{Message: "Pick one"}
	msg.SetReplyMarkup(markup)
	return msg
}

func TestNoMarkupIsLeftAlone(t *testing.T) {
	if _, ok := renderInlineKeyboard(&tg.Message{Message: "plain"}); ok {
		t.Fatal("a message without reply_markup must not be modified")
	}
}

// A reply keyboard is a different interaction from buttons under a bot message and
// is deliberately not rendered.
func TestReplyKeyboardIsNotRendered(t *testing.T) {
	msg := &tg.Message{Message: "hi"}
	msg.SetReplyMarkup(&tg.ReplyKeyboardMarkup{
		Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButton{Text: "Yes"},
		}}},
	})
	if _, ok := renderInlineKeyboard(msg); ok {
		t.Fatal("ReplyKeyboardMarkup must not be rendered as inline buttons")
	}
}

func TestURLButtonBecomesALink(t *testing.T) {
	r, ok := renderInlineKeyboard(inlineMsg([]tg.KeyboardButtonClass{
		&tg.KeyboardButtonURL{Text: "Open docs", URL: "https://example.com/a?b=1"},
	}))
	if !ok {
		t.Fatal("expected a rendering")
	}
	if !strings.Contains(r.html, `<a href="https://example.com/a?b=1">Open docs</a>`) {
		t.Fatalf("URL button should be a real link, got %q", r.html)
	}
	if !strings.Contains(r.text, "https://example.com/a?b=1") {
		t.Fatalf("plain text should carry the URL, got %q", r.text)
	}
}

// The security-relevant cases: these look like links but must not be clickable.
func TestAuthAndWebViewButtonsAreNotLinks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		button tg.KeyboardButtonClass
		kind   string
	}{
		{"URLAuth", &tg.KeyboardButtonURLAuth{Text: "Log in", URL: "https://evil.example"}, "login"},
		{"WebView", &tg.KeyboardButtonWebView{Text: "Open app", URL: "https://evil.example"}, "web app"},
		{"SimpleWebView", &tg.KeyboardButtonSimpleWebView{Text: "Mini app", URL: "https://evil.example"}, "web app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := renderInlineKeyboard(inlineMsg([]tg.KeyboardButtonClass{tc.button}))
			if !ok {
				t.Fatal("expected a rendering")
			}
			if strings.Contains(r.html, "<a ") || strings.Contains(r.html, "href") {
				t.Fatalf("%s must not render as a link, got %q", tc.name, r.html)
			}
			if strings.Contains(r.html, "evil.example") || strings.Contains(r.text, "evil.example") {
				t.Fatalf("%s must not surface its URL, got %q / %q", tc.name, r.text, r.html)
			}
			if !strings.Contains(r.text, tc.kind) {
				t.Fatalf("%s should be tagged %q, got %q", tc.name, tc.kind, r.text)
			}
		})
	}
}

// The callback payload is what a press would send back to the bot. Keeping it out of
// the room is what stops this rendering from becoming a reverse channel.
func TestCallbackDataIsNeverEmitted(t *testing.T) {
	r, ok := renderInlineKeyboard(inlineMsg([]tg.KeyboardButtonClass{
		&tg.KeyboardButtonCallback{Text: "Balance", Data: []byte("secret_callback_payload")},
	}))
	if !ok {
		t.Fatal("expected a rendering")
	}
	if strings.Contains(r.text, "secret_callback_payload") || strings.Contains(r.html, "secret_callback_payload") {
		t.Fatal("callback data must never reach the room")
	}
	if !strings.Contains(r.text, "[Balance]") {
		t.Fatalf("callback button should show its label, got %q", r.text)
	}
}

func TestLabelsAreEscaped(t *testing.T) {
	r, ok := renderInlineKeyboard(inlineMsg([]tg.KeyboardButtonClass{
		&tg.KeyboardButtonCallback{Text: `<img src=x onerror=alert(1)>`},
	}))
	if !ok {
		t.Fatal("expected a rendering")
	}
	if strings.Contains(r.html, "<img") {
		t.Fatalf("bot-supplied label must be escaped, got %q", r.html)
	}
}

func TestRowLayoutIsPreserved(t *testing.T) {
	r, ok := renderInlineKeyboard(inlineMsg(
		[]tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "A"},
			&tg.KeyboardButtonCallback{Text: "B"},
		},
		[]tg.KeyboardButtonClass{&tg.KeyboardButtonCallback{Text: "C"}},
	))
	if !ok {
		t.Fatal("expected a rendering")
	}
	if lines := strings.Split(r.text, "\n"); len(lines) != 2 {
		t.Fatalf("expected 2 rows, got %d: %q", len(lines), r.text)
	}
	if !strings.Contains(r.html, "<br/>") {
		t.Fatalf("rows should be separated in HTML, got %q", r.html)
	}
}

func TestChineseLabelsSurvive(t *testing.T) {
	r, ok := renderInlineKeyboard(inlineMsg([]tg.KeyboardButtonClass{
		&tg.KeyboardButtonCallback{Text: "查看余额"},
		&tg.KeyboardButtonURL{Text: "帮助", URL: "https://t.me/help"},
	}))
	if !ok {
		t.Fatal("expected a rendering")
	}
	if !strings.Contains(r.text, "查看余额") || !strings.Contains(r.html, "帮助") {
		t.Fatalf("Chinese labels must be preserved, got %q / %q", r.text, r.html)
	}
}

func TestAppendKeepsExistingTextAndPromotesToHTML(t *testing.T) {
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    "Pick one\n",
	}
	appendInlineKeyboard(content, renderedMarkup{text: "[A]", html: "<code>[A]</code>"})

	if !strings.HasPrefix(content.Body, "Pick one") {
		t.Fatalf("original body must be preserved, got %q", content.Body)
	}
	if !strings.Contains(content.Body, "[A]") {
		t.Fatalf("buttons must be appended, got %q", content.Body)
	}
	if content.Format != event.FormatHTML {
		t.Fatal("appending markup must set the HTML format")
	}
	if !strings.Contains(content.FormattedBody, "Pick one") {
		t.Fatalf("plain body must be carried into the HTML, got %q", content.FormattedBody)
	}
}

func TestAppendEscapesPreexistingPlainBody(t *testing.T) {
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    "5 < 6 & 7 > 2",
	}
	appendInlineKeyboard(content, renderedMarkup{text: "[A]", html: "<code>[A]</code>"})
	if strings.Contains(content.FormattedBody, "5 < 6 & 7 > 2") {
		t.Fatalf("plain body must be escaped when promoted to HTML, got %q", content.FormattedBody)
	}
}
