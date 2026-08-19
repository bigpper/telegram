// COMPANY PATCH — Matrix -> Telegram inline-keyboard button presses.
//
// This is the return path for the buttons replymarkup.go renders. An agent presses
// a button in Element, the client sends com.company.telegram.button_press into the
// room, and this file turns that into messages.getBotCallbackAnswer on Telegram.
//
// THIS FILE IS THE SECURITY BOUNDARY FOR THE FEATURE. Everything the client sends is
// untrusted: the client says which message and which button, and nothing more is
// believed. Specifically:
//
//   - The callback payload is NOT taken from the event. The client never had it —
//     replymarkup.go deliberately publishes coordinates and labels only. The payload
//     is read here by re-fetching the message from Telegram, so a client cannot
//     invoke a button that was not actually on the message.
//   - The button KIND is re-derived from the re-fetched message. A client claiming
//     "this login button is really a callback button" is contradicted by the source.
//   - The bot must be on the PressableBotIDs allowlist in the bridge config. This is
//     the gate an agent's browser cannot argue with: policy delivered to the client
//     decides what is *drawn*, this decides what is *done*. See ADR-0006 on why a
//     client-side switch is not a boundary.
//
// Registration reuses the mechanism proven by pinsync.go: appservice.EventProcessor
// is a public field and its On() appends, so a custom event type can be handled
// without forking mautrix-go.

package connector

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-telegram/pkg/connector/ids"
	"go.mau.fi/mautrix-telegram/pkg/gotd/tg"
)

// ButtonPressEventType is the custom event the Element module sends on a press.
var ButtonPressEventType = event.Type{Type: "com.company.telegram.button_press", Class: event.MessageEventType}

// buttonPressContent is the client-supplied part of a press. Treated as a claim,
// not as fact — see the file header.
type buttonPressContent struct {
	// The bridged message whose keyboard was pressed.
	RelatesTo struct {
		EventID id.EventID `json:"event_id"`
	} `json:"m.relates_to"`
	Row int `json:"row"`
	Col int `json:"col"`
}

// registerButtonPressHandler hooks the custom press event. Returns false when the
// Matrix connector is not the standard one, in which case buttons simply stay
// unpressable rather than failing at runtime.
func (tc *TelegramConnector) registerButtonPressHandler(ctx context.Context) bool {
	conn, ok := tc.Bridge.Matrix.(*matrix.Connector)
	if !ok || conn.EventProcessor == nil {
		return false
	}
	conn.EventProcessor.On(ButtonPressEventType, func(ctx context.Context, evt *event.Event) {
		tc.handleButtonPress(ctx, evt)
	})
	return true
}

func (tc *TelegramConnector) handleButtonPress(ctx context.Context, evt *event.Event) {
	log := zerolog.Ctx(ctx).With().
		Str("action", "telegram button press").
		Str("room_id", evt.RoomID.String()).
		Str("sender", evt.Sender.String()).
		Logger()
	ctx = log.WithContext(ctx)

	// Ignore anything the bridge itself or a ghost emitted.
	if _, isGhost := tc.Bridge.Matrix.ParseGhostMXID(evt.Sender); isGhost {
		return
	}
	if evt.Sender == tc.Bridge.Bot.GetMXID() {
		return
	}

	var press buttonPressContent
	if raw, ok := evt.Content.Raw["m.relates_to"].(map[string]any); ok {
		if s, ok := raw["event_id"].(string); ok {
			press.RelatesTo.EventID = id.EventID(s)
		}
	}
	press.Row = intFromRaw(evt.Content.Raw["row"])
	press.Col = intFromRaw(evt.Content.Raw["col"])
	if press.RelatesTo.EventID == "" {
		log.Warn().Msg("Button press without a target event")
		return
	}

	portal, err := tc.Bridge.GetPortalByMXID(ctx, evt.RoomID)
	if err != nil || portal == nil {
		return // not a portal room
	}
	login := tc.Bridge.GetCachedUserLoginByID(portal.Receiver)
	if login == nil || login.Client == nil {
		log.Warn().Msg("No login for portal; cannot press button")
		return
	}
	client, ok := login.Client.(*TelegramClient)
	if !ok {
		return
	}
	client.pressButton(ctx, portal, evt, press)
}

func intFromRaw(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// pressButton performs the press, having verified it is permitted.
//
// Failures are reported into the room. An agent who presses a button and is not told
// it failed will believe the bot acted — the same false impression the pin sync
// notice exists to prevent.
func (tc *TelegramClient) pressButton(
	ctx context.Context,
	portal *bridgev2.Portal,
	evt *event.Event,
	press buttonPressContent,
) {
	log := zerolog.Ctx(ctx)

	dbMsg, err := tc.main.Bridge.DB.Message.GetPartByMXID(ctx, press.RelatesTo.EventID)
	if err != nil || dbMsg == nil {
		tc.reportPressFailure(ctx, portal, "that message is not a bridged Telegram message")
		return
	}
	_, msgID, err := ids.ParseMessageID(dbMsg.ID)
	if err != nil {
		tc.reportPressFailure(ctx, portal, "could not identify the Telegram message")
		return
	}
	peer, _, err := tc.inputPeerForPortalID(ctx, portal.ID)
	if err != nil {
		tc.reportPressFailure(ctx, portal, "could not resolve the Telegram chat")
		return
	}

	// Re-fetch the message. This is what makes the callback payload unavailable to
	// the client AND lets the kind and the owning bot be checked against the source
	// rather than against what the press claimed.
	msg, err := tc.fetchMessage(ctx, peer, msgID)
	if err != nil || msg == nil {
		tc.reportPressFailure(ctx, portal, "could not re-read the message from Telegram")
		return
	}

	button, err := buttonAt(msg, press.Row, press.Col)
	if err != nil {
		log.Warn().Err(err).Int("row", press.Row).Int("col", press.Col).
			Msg("Press did not match a button on the message")
		tc.reportPressFailure(ctx, portal, "that button is no longer on the message")
		return
	}
	callback, ok := button.(*tg.KeyboardButtonCallback)
	if !ok {
		// Not a callback button. Refused here regardless of what the client drew:
		// login and web-app buttons authorise a bot against the company identity.
		_, kind := describeButton(button)
		log.Warn().Str("kind", kind).Msg("Press refused: button is not a callback button")
		tc.reportPressFailure(ctx, portal,
			fmt.Sprintf("that is a %q button, which cannot be pressed from here", kind))
		return
	}

	// The allowlist gate. Deliberately last of the checks that read Telegram state,
	// because it needs the message's real sender rather than the room's.
	botID := senderUserID(msg)
	if !tc.main.Config.isBotPressable(botID) {
		log.Warn().Int64("bot_id", botID).Msg("Press refused: bot not in pressable_bot_ids")
		tc.reportPressFailure(ctx, portal,
			"pressing this bot's buttons is not enabled. An integration administrator "+
				"must add it to pressable_bot_ids in the bridge config.")
		return
	}

	answer, err := tc.client.API().MessagesGetBotCallbackAnswer(ctx, &tg.MessagesGetBotCallbackAnswerRequest{
		Peer:  peer,
		MsgID: msgID,
		Data:  callback.Data,
	})
	if err != nil {
		log.Err(err).Msg("Button press failed on Telegram")
		tc.reportPressFailure(ctx, portal, fmt.Sprintf("Telegram refused the press (%s)", err))
		return
	}

	log.Info().
		Int("msg_id", msgID).
		Int64("bot_id", botID).
		Str("sender", evt.Sender.String()).
		Msg("Pressed inline keyboard button")

	tc.reportPressAnswer(ctx, portal, answer)
}

// fetchMessage re-reads one message so its keyboard can be trusted.
func (tc *TelegramClient) fetchMessage(ctx context.Context, peer tg.InputPeerClass, msgID int) (*tg.Message, error) {
	var (
		res tg.MessagesMessagesClass
		err error
	)
	if channel, ok := peer.(*tg.InputPeerChannel); ok {
		res, err = tc.client.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
		})
	} else {
		res, err = tc.client.API().MessagesGetMessages(ctx, []tg.InputMessageClass{
			&tg.InputMessageID{ID: msgID},
		})
	}
	if err != nil {
		return nil, err
	}
	var msgs []tg.MessageClass
	switch r := res.(type) {
	case *tg.MessagesMessages:
		msgs = r.Messages
	case *tg.MessagesMessagesSlice:
		msgs = r.Messages
	case *tg.MessagesChannelMessages:
		msgs = r.Messages
	default:
		return nil, fmt.Errorf("unexpected messages response %T", res)
	}
	for _, m := range msgs {
		if full, ok := m.(*tg.Message); ok && full.ID == msgID {
			return full, nil
		}
	}
	return nil, fmt.Errorf("message %d not found", msgID)
}

// buttonAt returns the button at the given coordinates of the message's keyboard.
func buttonAt(msg *tg.Message, row, col int) (tg.KeyboardButtonClass, error) {
	markup, ok := msg.GetReplyMarkup()
	if !ok {
		return nil, fmt.Errorf("message has no reply markup")
	}
	inline, ok := markup.(*tg.ReplyInlineMarkup)
	if !ok {
		return nil, fmt.Errorf("reply markup is not an inline keyboard")
	}
	if row < 0 || row >= len(inline.Rows) {
		return nil, fmt.Errorf("row %d out of range", row)
	}
	buttons := inline.Rows[row].Buttons
	if col < 0 || col >= len(buttons) {
		return nil, fmt.Errorf("column %d out of range", col)
	}
	return buttons[col], nil
}

// senderUserID is the Telegram user ID that sent the message — the bot, for a bot
// message. Zero when it cannot be determined, which fails the allowlist closed.
func senderUserID(msg *tg.Message) int64 {
	from, ok := msg.GetFromID()
	if !ok {
		// In a direct chat with a bot the message carries no from_id; the peer is
		// the bot itself.
		if user, ok := msg.PeerID.(*tg.PeerUser); ok {
			return user.UserID
		}
		return 0
	}
	if user, ok := from.(*tg.PeerUser); ok {
		return user.UserID
	}
	return 0
}

// isBotPressable reports whether this bot's buttons may be pressed. An empty
// allowlist means none — the feature is opt-in per bot.
func (c *TelegramConfig) isBotPressable(botID int64) bool {
	if botID == 0 {
		return false
	}
	for _, id := range c.PressableBotIDs {
		if id == botID {
			return true
		}
	}
	return false
}

func (tc *TelegramClient) reportPressFailure(ctx context.Context, portal *bridgev2.Portal, why string) {
	tc.sendRoomNotice(ctx, portal, "⚠️ Button not pressed — "+why)
}

// reportPressAnswer surfaces whatever the bot replied. Telegram answers a callback
// with a toast or an alert that the pressing client is expected to display; there is
// no equivalent in Matrix, so it goes into the room as a notice. Without this the
// agent gets no feedback at all and cannot tell a working button from a dead one.
func (tc *TelegramClient) reportPressAnswer(ctx context.Context, portal *bridgev2.Portal, answer *tg.MessagesBotCallbackAnswer) {
	switch {
	case answer.Message != "":
		prefix := "🔘 Bot replied: "
		if answer.Alert {
			prefix = "🔘 Bot alert: "
		}
		tc.sendRoomNotice(ctx, portal, prefix+answer.Message)
	case answer.URL != "":
		// A URL answer is the bot redirecting the presser. Shown, not followed.
		tc.sendRoomNotice(ctx, portal, "🔘 Bot replied with a link: "+answer.URL)
	default:
		tc.sendRoomNotice(ctx, portal, "🔘 Button pressed. The bot acknowledged without a message.")
	}
}

func (tc *TelegramClient) sendRoomNotice(ctx context.Context, portal *bridgev2.Portal, body string) {
	_, err := tc.main.Bridge.Bot.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{
		Parsed: &event.MessageEventContent{MsgType: event.MsgNotice, Body: body},
	}, nil)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to report button press result into the room")
	}
}
