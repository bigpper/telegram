// COMPANY PATCH — install the customer panel widget when a portal room is created.
//
// WHY THE BRIDGE DOES THIS AND NOT THE MANAGEMENT API
//
// Writing im.vector.modular.widgets is a state event, which needs power level 50 in
// the portal room. The bridge bot creates these rooms and already holds that level;
// the Management API holds none, and giving it the appservice token so it could would
// hand it the ability to impersonate every ghost on the homeserver (ADR-0005, §25).
// The isolation is the point, so the work moves to the side that already has the
// power rather than the power moving to the side that wants to do the work.
//
// WHY AT CREATION AND NOT ON A TIMER
//
// A periodic sweep leaves every new conversation without a panel for up to one
// interval, and an agent cannot tell "not provisioned yet" from "this customer has no
// page". The room is created here; the widget belongs in the same operation.
//
// WHAT THE URL IS
//
// One fixed URL for every room, carrying $matrix_room_id — Element substitutes that,
// not this code. The per-room mapping lives in the management database and is
// resolved by the host system's own page via /api/widget/verify. That is why this
// needs no channel to the Management API: there is nothing per-room to ask it.
//
// The URL becomes room state, readable by every member, so it must carry no secret.
//
// WHEN company_widget_url IS EMPTY, NOTHING IS INSTALLED. That is the supported
// state, not a degraded one: Element renders the customer panel's empty state itself
// (CustomerPanelEmptyCard), so agents still get the panel and a clear message. A
// deployment with no host system should not have an iframe pointing at nothing.
package connector

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"
)

// widgetStateType is Element's widget state event. The im.vector.* name is the one
// Element actually reads; the MSC that would standardise it is not implemented.
var widgetStateType = event.Type{Type: "im.vector.modular.widgets", Class: event.StateEventType}

// companyWidgetStateKey identifies our widget among any others in the room. A stable
// key means re-running this replaces our widget rather than accumulating copies.
const companyWidgetStateKey = "company-customer-panel"

// registerWidgetProvisioner hooks room creation. Returns false when the Matrix
// connector is not the type we expect, so the caller can log and continue without
// this rather than failing to start.
func (tc *TelegramConnector) registerWidgetProvisioner(ctx context.Context) bool {
	if strings.TrimSpace(tc.Config.CompanyWidgetURL) == "" {
		zerolog.Ctx(ctx).Info().
			Msg("company_widget_url is empty; portal rooms get no widget and Element shows its own empty state")
		return true
	}
	conn, ok := tc.Bridge.Matrix.(*matrix.Connector)
	if !ok || conn.EventProcessor == nil {
		return false
	}
	conn.EventProcessor.On(event.StateCreate, func(ctx context.Context, evt *event.Event) {
		tc.handleRoomCreated(ctx, evt)
	})
	return true
}

func (tc *TelegramConnector) handleRoomCreated(ctx context.Context, evt *event.Event) {
	log := zerolog.Ctx(ctx).With().Str("action", "provision widget").Stringer("room_id", evt.RoomID).Logger()

	// Only portal rooms. The bridge bot is also in its own management room and in the
	// personal filtering space, and neither of those is a customer conversation.
	portal, err := tc.Bridge.GetPortalByMXID(ctx, evt.RoomID)
	if err != nil {
		log.Err(err).Msg("Failed to look up portal")
		return
	}
	if portal == nil {
		return
	}

	// Idempotent: an m.room.create can be redelivered, and a portal can be recreated.
	if stateReader, canReadState := tc.Bridge.Matrix.(bridgev2.MatrixConnectorWithArbitraryRoomState); canReadState {
		if existing, err := stateReader.GetStateEvent(ctx, evt.RoomID, widgetStateType, companyWidgetStateKey); err == nil && existing != nil {
			if url, _ := existing.Content.Raw["url"].(string); url == tc.Config.CompanyWidgetURL {
				return
			}
		}
	}

	content := map[string]any{
		"type":    "customer",
		"url":     tc.Config.CompanyWidgetURL,
		"name":    tc.Config.CompanyWidgetName,
		"creatorUserId": tc.Bridge.Bot.GetMXID().String(),
		// Element substitutes these from the viewing session; they are not filled in
		// here and must not be, since this event is shared by every member.
		"data": map[string]any{},
	}

	if _, err := tc.Bridge.Bot.SendState(
		ctx, evt.RoomID, widgetStateType, companyWidgetStateKey,
		&event.Content{Raw: content}, time.Time{},
	); err != nil {
		log.Err(err).Msg("Failed to install customer panel widget")
		return
	}
	log.Info().Msg("Installed customer panel widget")
}
