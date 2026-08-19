// COMPANY PATCH — ADR-0001 tests.
//
// These exist to fail loudly if an upstream rebase reverts the identity boundary.
// The property under test is not "the function returns something", it is "no
// Telegram-derived value survives into the network user ID".
package ids

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestMain(m *testing.M) {
	os.Setenv(identityKeyEnv, "test-key-do-not-use-in-production")
	os.Exit(m.Run())
}

func TestUserIDRoundTrips(t *testing.T) {
	cases := []struct {
		name string
		pt   PeerType
		id   int64
	}{
		{"user", PeerTypeUser, 829183728},
		{"small user", PeerTypeUser, 1},
		{"large user", PeerTypeUser, 7_000_000_000},
		{"channel", PeerTypeChannel, 1234567890},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var uid networkid.UserID
			if tc.pt == PeerTypeChannel {
				uid = MakeChannelUserID(tc.id)
			} else {
				uid = MakeUserID(tc.id)
			}
			gotPT, gotID, err := ParseUserID(uid)
			if err != nil {
				t.Fatalf("ParseUserID: %v", err)
			}
			if gotID != tc.id {
				t.Errorf("id: got %d, want %d", gotID, tc.id)
			}
			if gotPT != tc.pt {
				t.Errorf("peer type: got %q, want %q", gotPT, tc.pt)
			}
		})
	}
}

// The point of the whole patch.
func TestUserIDLeaksNothing(t *testing.T) {
	const tgID = 829183728
	uid := string(MakeUserID(tgID))

	if strings.Contains(uid, strconv.Itoa(tgID)) {
		t.Fatalf("network user ID contains the raw Telegram ID: %q", uid)
	}
	// Also reject the digits appearing in any other base, and any digits at all —
	// the token alphabet is letters and 2-7 only.
	for _, r := range uid {
		if r == '0' || r == '1' || r == '8' || r == '9' {
			t.Errorf("token contains an out-of-alphabet character %q in %q", r, uid)
		}
	}
	if uid == strconv.Itoa(tgID) {
		t.Fatal("token is the raw ID")
	}
}

func TestDeterministic(t *testing.T) {
	a := MakeUserID(829183728)
	b := MakeUserID(829183728)
	if a != b {
		t.Fatalf("not deterministic: %q vs %q — portals and read receipts would break", a, b)
	}
}

func TestDistinctIDsGiveDistinctTokens(t *testing.T) {
	seen := map[networkid.UserID]int64{}
	for id := int64(1); id < 500; id++ {
		tok := MakeUserID(id)
		if prev, dup := seen[tok]; dup {
			t.Fatalf("collision: %d and %d both produce %q", prev, id, tok)
		}
		seen[tok] = id
	}
}

// A user token must not decode as a channel, or the bridge would talk to the wrong
// peer type.
func TestPeerTypesDoNotCollide(t *testing.T) {
	const id = 1234567890
	if MakeUserID(id) == MakeChannelUserID(id) {
		t.Fatal("user and channel tokens for the same ID are identical")
	}
	pt, _, err := ParseUserID(MakeChannelUserID(id))
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}
	if pt != PeerTypeChannel {
		t.Errorf("got peer type %q, want channel", pt)
	}
}

func TestRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "829183728", "not-base32!", "aaaa", strings.Repeat("a", 26)} {
		if _, _, err := ParseUserID(networkid.UserID(bad)); err == nil {
			t.Errorf("accepted invalid token %q", bad)
		}
	}
}

// A token minted under one key must not decode under another, or rotating the key
// would silently map customers onto each other's identities.
func TestWrongKeyIsRejected(t *testing.T) {
	tok := MakeUserID(829183728)

	// Reset the memoised cipher and swap the key.
	identityOnce = sync.Once{}
	identityCipher, identityErr = nil, nil
	os.Setenv(identityKeyEnv, "a-completely-different-key")
	defer func() {
		identityOnce = sync.Once{}
		identityCipher, identityErr = nil, nil
		os.Setenv(identityKeyEnv, "test-key-do-not-use-in-production")
	}()

	if _, _, err := ParseUserID(tok); err == nil {
		t.Error("token decoded under the wrong key")
	}
}

func TestDisplayTokenIsNotPII(t *testing.T) {
	const tgID = 829183728
	got := OpaqueDisplayToken(tgID)
	if got == "" {
		t.Fatal("empty display token")
	}
	if strings.Contains(got, strconv.Itoa(tgID)) {
		t.Errorf("display token contains the Telegram ID: %q", got)
	}
	if got != OpaqueDisplayToken(tgID) {
		t.Error("display token is not stable")
	}
	if OpaqueDisplayToken(tgID) == OpaqueDisplayToken(tgID+1) {
		t.Error("different customers share a display token")
	}
}
