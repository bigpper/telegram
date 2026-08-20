// COMPANY PATCH — Matrix -> Telegram pinned message sync.
//
// bridgev2's typed dispatch (portal.handleMatrixEvent) has no case for
// m.room.pinned_events and no generic state-event hook, so the obvious reading is
// that supporting this needs a mautrix-go fork. It does not.
//
// The underlying appservice EventProcessor is a PUBLIC field on the Matrix
// connector, its On() APPENDS handlers rather than replacing them, and
// m.room.pinned_events is absent from the list bridgev2 registers
// (matrix/connector.go:140-157). So a handler registered here is the only one for
// that type and cannot race the framework.
//
// Telegram -> Matrix lives in handletelegram.go; this is the other direction.
package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-telegram/pkg/connector/ids"
	"go.mau.fi/mautrix-telegram/pkg/gotd/tg"
)

// registerPinHandler hooks m.room.pinned_events. Returns false when the Matrix
// connector is not the standard one — in that case pinning simply stays
// Telegram-to-Matrix only rather than failing at runtime.
func (tc *TelegramConnector) registerPinHandler(ctx context.Context) bool {
	conn, ok := tc.Bridge.Matrix.(*matrix.Connector)
	if !ok || conn.EventProcessor == nil {
		return false
	}
	conn.EventProcessor.On(event.StatePinnedEvents, func(ctx context.Context, evt *event.Event) {
		tc.handleMatrixPinEvent(ctx, evt)
	})
	return true
}

func (tc *TelegramConnector) handleMatrixPinEvent(ctx context.Context, evt *event.Event) {
	log := zerolog.Ctx(ctx).With().
		Str("action", "matrix pin sync").
		Str("room_id", evt.RoomID.String()).
		Logger()
	ctx = log.WithContext(ctx)

	// Ignore our own echoes: the bridge writes this state itself when mirroring a
	// Telegram pin, and reacting to that would bounce the change back to Telegram.
	//
	// There is no IsGhostMXID; ParseGhostMXID's second return value is the check.
	if _, isGhost := tc.Bridge.Matrix.ParseGhostMXID(evt.Sender); isGhost {
		return
	}
	if evt.Sender == tc.Bridge.Bot.GetMXID() {
		return
	}

	portal, err := tc.Bridge.GetPortalByMXID(ctx, evt.RoomID)
	if err != nil || portal == nil {
		return // not a portal room
	}

	newPins := pinnedFrom(evt.Content.Parsed)
	oldPins := pinnedFrom(evt.Unsigned.PrevContent.Parsed)

	added := difference(newPins, oldPins)
	removed := difference(oldPins, newPins)
	if len(added) == 0 && len(removed) == 0 {
		return
	}

	login := tc.Bridge.GetCachedUserLoginByID(portal.Receiver)
	if login == nil || login.Client == nil {
		log.Warn().Msg("No login for portal; cannot sync pin")
		return
	}
	client, ok := login.Client.(*TelegramClient)
	if !ok {
		return
	}

	for _, ev := range added {
		client.syncPinToTelegram(ctx, portal, ev, false)
	}
	for _, ev := range removed {
		client.syncPinToTelegram(ctx, portal, ev, true)
	}
}

func pinnedFrom(parsed any) []id.EventID {
	if c, ok := parsed.(*event.PinnedEventsEventContent); ok && c != nil {
		return c.Pinned
	}
	return nil
}

func difference(a, b []id.EventID) []id.EventID {
	inB := make(map[id.EventID]struct{}, len(b))
	for _, x := range b {
		inB[x] = struct{}{}
	}
	var out []id.EventID
	for _, x := range a {
		if _, found := inB[x]; !found {
			out = append(out, x)
		}
	}
	return out
}

// syncPinToTelegram pins or unpins one message on the Telegram side.
//
// Failures are reported into the room rather than swallowed. An agent who pins
// something and is not told it failed will believe the customer sees it — the exact
// false impression this feature exists to remove.
func (tc *TelegramClient) syncPinToTelegram(
	ctx context.Context,
	portal *bridgev2.Portal,
	mxid id.EventID,
	unpin bool,
) {
	log := zerolog.Ctx(ctx)

	dbMsg, err := tc.main.Bridge.DB.Message.GetPartByMXID(ctx, mxid)
	if err != nil || dbMsg == nil {
		log.Debug().Str("event_id", mxid.String()).Msg("Pinned event is not a bridged message")
		return
	}
	_, msgID, err := ids.ParseMessageID(dbMsg.ID)
	if err != nil {
		log.Warn().Err(err).Msg("Could not parse bridged message ID for pin")
		return
	}

	peer, _, err := tc.inputPeerForPortalID(ctx, portal.ID)
	if err != nil {
		log.Warn().Err(err).Msg("Could not resolve peer for pin")
		return
	}

	_, err = tc.client.API().MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
		Peer: peer,
		ID:   msgID,
		// Unpin removes it for everyone, matching what the agent did in Matrix.
		Unpin: unpin,
		// Pinning normally notifies every group member. For a customer service
		// group that is noise on the customer's phone, so pin quietly.
		Silent: true,
	})
	if err != nil {
		verb := "pin"
		if unpin {
			verb = "unpin"
		}
		log.Err(err).Str("event_id", mxid.String()).Msgf("Failed to %s message on Telegram", verb)
		tc.notifyPinFailure(ctx, portal, verb, err)
		return
	}
	log.Info().Str("event_id", mxid.String()).Bool("unpin", unpin).Msg("Synced pin to Telegram")
}

// notifyPinFailure tells the agent, in the room, that the pin did not reach
// Telegram. Usually this is the company account lacking pin rights in that group.
func (tc *TelegramClient) notifyPinFailure(
	ctx context.Context,
	portal *bridgev2.Portal,
	verb string,
	cause error,
) {
	msg := fmt.Sprintf(
		"⚠️ Could not %s this message on Telegram — the customer does not see the change. "+
			"This usually means the company account lacks pin permission in that group. (%s)",
		verb, cause,
	)
	_, err := tc.main.Bridge.Bot.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{
		Parsed: &event.MessageEventContent{MsgType: event.MsgNotice, Body: msg},
	}, nil)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to report pin failure into the room")
	}
}

var _ = appservice.EventProcessor{}

// importPinnedMessages brings across pins that already existed on Telegram when the
// portal was bridged.
//
// The incremental path above only sees pin SERVICE MESSAGES, which Telegram emits
// when a pin happens. Anything pinned before the room was bridged never produced one
// we saw, so those pins were invisible — and since Telegram allows many pins per
// chat, a busy group could arrive with several and show none of them.
//
// Runs at most once per portal, guarded by PortalMetadata.PinsImported. The guard is
// a stored flag rather than "does this room have pins yet", because most rooms have
// no pins and that test would re-query Telegram on every sync — a rate-limit surface
// for no benefit (see docs/telegram-rate-limits.md).
func (tc *TelegramClient) importPinnedMessages(ctx context.Context, portal *bridgev2.Portal) {
	log := zerolog.Ctx(ctx).With().Str("action", "import telegram pins").Logger()

	meta, ok := portal.Metadata.(*PortalMetadata)
	if !ok || meta.PinsImported || portal.MXID == "" {
		return
	}

	peer, _, err := tc.inputPeerForPortalID(ctx, portal.ID)
	if err != nil {
		log.Debug().Err(err).Msg("Could not resolve peer; leaving pins for a later sync")
		return
	}

	// The full pinned list is only reachable through search; chat info carries just
	// pinned_msg_id, which is the single most recent pin and is what made this look
	// like "Telegram only gives us one".
	res, err := tc.client.API().MessagesSearch(ctx, &tg.MessagesSearchRequest{
		Peer:   peer,
		Filter: &tg.InputMessagesFilterPinned{},
		Limit:  100,
	})
	if err != nil {
		log.Warn().Err(err).Msg("Could not fetch pinned messages")
		return
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
		log.Warn().Msgf("Unexpected search response %T", res)
		return
	}

	portalKey := portal.PortalKey
	var pinned []id.EventID
	for _, m := range msgs {
		full, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		dbMsg, err := tc.main.Bridge.DB.Message.GetFirstPartByID(
			ctx, tc.loginID, ids.MakeMessageID(portalKey, full.ID))
		if err != nil || dbMsg == nil {
			// Predates backfill. Skipping is right: a pin pointing at a message the
			// agent cannot open is worse than no pin.
			continue
		}
		pinned = append(pinned, dbMsg.MXID)
	}

	// Mark as done even when nothing was found, so a chat with no pins is not
	// re-queried forever.
	meta.PinsImported = true
	if err := portal.Save(ctx); err != nil {
		log.Warn().Err(err).Msg("Could not persist pins-imported flag")
	}
	if len(pinned) == 0 {
		return
	}

	// Merge rather than overwrite: a pin may already have arrived through the
	// incremental path while this was running.
	stateReader, canReadState := tc.main.Bridge.Matrix.(bridgev2.MatrixConnectorWithArbitraryRoomState)
	if !canReadState {
		log.Warn().Msg("Matrix connector cannot read arbitrary room state; skipping pin import")
		return
	}
	var current event.PinnedEventsEventContent
	if existing, err := stateReader.GetStateEvent(ctx, portal.MXID, event.StatePinnedEvents, ""); err == nil && existing != nil {
		if parsed, ok := existing.Content.Parsed.(*event.PinnedEventsEventContent); ok && parsed != nil {
			current = *parsed
		}
	}
	seen := make(map[id.EventID]struct{}, len(current.Pinned))
	for _, ev := range current.Pinned {
		seen[ev] = struct{}{}
	}
	added := 0
	for _, ev := range pinned {
		if _, dup := seen[ev]; dup {
			continue
		}
		current.Pinned = append(current.Pinned, ev)
		seen[ev] = struct{}{}
		added++
	}
	if added == 0 {
		return
	}

	if _, err := tc.main.Bridge.Bot.SendState(
		ctx, portal.MXID, event.StatePinnedEvents, "",
		&event.Content{Parsed: &current}, time.Time{},
	); err != nil {
		log.Err(err).Msg("Failed to write imported pins")
		return
	}
	log.Info().Int("imported", added).Int("total", len(current.Pinned)).Msg("Imported Telegram pins")
}
