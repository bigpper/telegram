// mautrix-telegram - A Matrix-Telegram puppeting bridge.
// Copyright (C) 2024 Sumner Evans
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	"sync"
	"context"
	"github.com/rs/zerolog"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"

	"go.mau.fi/mautrix-telegram/pkg/connector/store"
)

type TelegramConnector struct {
	Bridge *bridgev2.Bridge
	Config TelegramConfig

	// COMPANY PATCH: serialises the read-modify-write of the flood-wait counters in
	// floodwait.go. Two goroutines hitting a wait at once would otherwise lose one of
	// the increments, and undercounting is the wrong direction for this particular
	// number to be wrong in.
	floodWaitMu sync.Mutex
	Store  *store.Container

	useDirectMedia bool
	maxFileSize    int64
}

var _ bridgev2.NetworkConnector = (*TelegramConnector)(nil)
var _ bridgev2.MaxFileSizeingNetwork = (*TelegramConnector)(nil)

func (tc *TelegramConnector) Init(bridge *bridgev2.Bridge) {
	tc.Store = store.NewStore(bridge.DB.Database, dbutil.ZeroLogger(bridge.Log.With().Str("db_section", "telegram").Logger()))
	tc.Bridge = bridge
	tc.Bridge.Commands.(*commands.Processor).AddHandlers(cmdSyncChats, cmdEmojiPack, cmdUpgrade, cmdJoin)
}

func (tc *TelegramConnector) Start(ctx context.Context) error {
	// COMPANY PATCH: Matrix -> Telegram pin sync. See pinsync.go for why this is a
	// handler registration rather than a mautrix-go fork.
	if !tc.registerPinHandler(ctx) {
		zerolog.Ctx(ctx).Warn().Msg("Could not register pin handler; pins will sync Telegram->Matrix only")
	}

	// COMPANY PATCH: install the customer panel widget when a portal room is created.
	// See widgetprovision.go — the bridge does this because it already holds the power
	// level the state event needs, and the Management API deliberately holds none.
	if !tc.registerWidgetProvisioner(ctx) {
		zerolog.Ctx(ctx).Warn().Msg("Could not register widget provisioner; portal rooms will get no customer panel widget")
	}

	// COMPANY PATCH: inline-keyboard button presses. See buttonpress.go — the
	// allowlist there, not this registration, is what decides whether a press is
	// permitted, so registering unconditionally is safe.
	if !tc.registerButtonPressHandler(ctx) {
		zerolog.Ctx(ctx).Warn().Msg("Could not register button press handler; inline keyboard buttons stay inert")
	}
	return tc.Store.Upgrade(ctx)
}

func (tc *TelegramConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) (err error) {
	login.Client, err = NewTelegramClient(ctx, tc, login)
	return
}

func (tc *TelegramConnector) SetMaxFileSize(maxSize int64) {
	tc.maxFileSize = maxSize
}

func (tc *TelegramConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "Telegram",
		NetworkURL:           "https://telegram.org/",
		// COMPANY PATCH: no network icon, rather than one that cannot load.
		//
		// Upstream points this at an image on maunium.net. mautrix writes it into the
		// m.bridge / uk.half-shot.bridge state event of every portal room, so every
		// agent's client requests it in every conversation — and every request fails
		// with 403, forever. Three deliberate decisions guarantee it: the URI is remote
		// media, federation is disabled (ADR-0003), and authenticated media turns the
		// refusal into a 403 rather than a 404. Upstream assumes a federated bridge; this
		// one is the opposite by design.
		//
		// Clearing the appservice bot's own avatar (appservice.bot.avatar: remove) does
		// NOT cover this. That is the bot's profile; this is the protocol icon in room
		// state, and they are written from different places.
		//
		// To have an icon, upload one to THIS homeserver and put its mxc:// here. Any
		// URI on another server has the same problem.
		NetworkIcon:          "",
		NetworkID:            "telegram",
		BeeperBridgeType:     "telegram",
		DefaultPort:          29317,
		DefaultCommandPrefix: "!tg",
	}
}
