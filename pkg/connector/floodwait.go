// COMPANY PATCH — record Telegram FLOOD_WAIT so the admin console can see it.
//
// WHY THIS IS WORTH PERSISTING AND NOT JUST LOGGING
//
// FLOOD_WAIT is the one limiter in this system that cannot be configured away, and
// answering it with harder or parallel retries escalates it to PEER_FLOOD — an
// account-level restriction rather than a request-level one (docs/telegram-rate-limits.md).
// The useful signal is therefore the TREND in frequency and duration, which is
// exactly what a log line cannot give you: the bridge's logs start again at every
// container recreation, and by the time anyone greps them the account is already in
// trouble.
//
// WHY kv_store AND NOT A NEW TABLE
//
// A dedicated table would need a migration in the bridge's upgrade table, which
// collides with upstream numbering on every rebase. kv_store already exists, is
// already the bridge's place for small bridge-scoped values, and one row is enough:
// this is a counter and a high-water mark, not an event log.
//
// The Management API reads it through a VIEW rather than through kv_store itself, so
// a future mautrix version that puts something sensitive in this table does not
// become readable by accident. See scripts/grant-bridge-readonly.sh.
package connector

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-telegram/pkg/gotd/tgerr"
)

// floodWaitStatsKey is the kv_store key holding the counters below. Namespaced with
// "company_" so it is obviously not upstream's.
const floodWaitStatsKey = "company_flood_wait_stats"

// FloodWaitStats is what the admin console shows. Deliberately small: counts and a
// high-water mark, not a per-event history, because the decision it informs ("back
// off or not") does not need one.
type FloodWaitStats struct {
	// Total occurrences since the counters were created. Never reset by the bridge:
	// a counter that resets on restart hides exactly the pattern worth seeing.
	Count int `json:"count"`
	// PEER_FLOOD is not a throttle. It means the account has been restricted, so it
	// is counted separately and should be zero.
	PeerFloodCount int `json:"peerFloodCount"`
	// Longest single wait, in seconds. Rising maximum is what turns a delay into an
	// outage, and it moves before the count does.
	MaxWaitSeconds int `json:"maxWaitSeconds"`
	// Sum, so the console can show a mean without storing samples.
	TotalWaitSeconds int       `json:"totalWaitSeconds"`
	LastAt           time.Time `json:"lastAt,omitempty"`
	FirstAt          time.Time `json:"firstAt,omitempty"`
}

// floodWait wraps tgerr.FloodWait, recording the wait before sleeping through it.
//
// Call sites keep the same shape as upstream's (`retry, err = ...FloodWait(ctx, call)`),
// so this stays a one-word change at each and rebases cleanly.
func (tc *TelegramClient) floodWait(ctx context.Context, err error) (bool, error) {
	if d, ok := tgerr.AsFloodWait(err); ok {
		tc.main.recordFloodWait(ctx, d, tgerr.Is(err, "PEER_FLOOD"))
	}
	return tgerr.FloodWait(ctx, err)
}

// recordFloodWait updates the counters. Failures here are logged and swallowed: a
// telemetry write must never turn a wait that the bridge was handling correctly into
// a failed API call.
func (tc *TelegramConnector) recordFloodWait(ctx context.Context, d time.Duration, peerFlood bool) {
	log := zerolog.Ctx(ctx).With().Str("action", "record flood wait").Logger()
	secs := int(d.Seconds())

	log.Warn().
		Int("wait_seconds", secs).
		Bool("peer_flood", peerFlood).
		Msg("Telegram FLOOD_WAIT")

	db := tc.Bridge.DB.Database
	if db == nil {
		return
	}

	tc.floodWaitMu.Lock()
	defer tc.floodWaitMu.Unlock()

	stats := FloodWaitStats{}
	var raw string
	err := db.QueryRow(ctx, `SELECT value FROM kv_store WHERE bridge_id = $1 AND key = $2`,
		tc.Bridge.ID, floodWaitStatsKey).Scan(&raw)
	if err == nil && raw != "" {
		if jsonErr := json.Unmarshal([]byte(raw), &stats); jsonErr != nil {
			// Corrupt value: start again rather than refuse to count. Losing history
			// is bad; losing the ability to see a flood in progress is worse.
			log.Warn().Err(jsonErr).Msg("Discarding unreadable flood-wait stats")
			stats = FloodWaitStats{}
		}
	}

	now := time.Now().UTC()
	if stats.Count == 0 {
		stats.FirstAt = now
	}
	stats.Count++
	if peerFlood {
		stats.PeerFloodCount++
	}
	stats.TotalWaitSeconds += secs
	if secs > stats.MaxWaitSeconds {
		stats.MaxWaitSeconds = secs
	}
	stats.LastAt = now

	encoded, jsonErr := json.Marshal(stats)
	if jsonErr != nil {
		log.Err(jsonErr).Msg("Failed to encode flood-wait stats")
		return
	}
	if _, execErr := db.Exec(ctx, `
		INSERT INTO kv_store (bridge_id, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (bridge_id, key) DO UPDATE SET value = excluded.value
	`, tc.Bridge.ID, floodWaitStatsKey, string(encoded)); execErr != nil {
		log.Err(execErr).Msg("Failed to persist flood-wait stats")
	}
}
