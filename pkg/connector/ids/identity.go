// COMPANY PATCH — ADR-0001, identity boundary.
//
// Upstream derives every ghost's Matrix ID from the raw Telegram user ID:
//
//	MakeUserID(829183728) -> "829183728"
//	                      -> @telegram_829183728:server
//
// Those Matrix IDs are the state_key of m.room.member events, so Matrix delivers
// them to every joined client. Any agent holding their own access token can read
// every customer's Telegram ID straight out of /joined_members or /sync, whatever
// the UI chooses to display. Masking in the client cannot fix that; the identifier
// has already crossed the boundary.
//
// This file replaces the identifier at the point it is minted, so nothing
// downstream of the bridge ever holds a Telegram-derived value:
//
//	MakeUserID(829183728) -> "k7f3d9e1x2b4m8qpa5..."
//	                      -> @telegram_k7f3d9e1x2b4m8qpa5...:server
//
// The token is a single AES-256 block: deterministic (so the bridge database,
// room membership and read receipts keep working), and reversible only with
// MAUTRIX_IDENTITY_KEY, which never leaves this process.
//
// Security note: this is keyed, so publishing the mechanism costs nothing. The
// guarantee rests entirely on the key. See docs/adr/0001-identity-boundary.md.
package ids

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
)

const identityKeyEnv = "MAUTRIX_IDENTITY_KEY"

// Lowercase, unpadded base32. Matrix localparts are case-insensitive in practice
// and uppercase requires escaping, so the alphabet is folded to lowercase; the
// output is 26 characters for a 16-byte block.
var tokenEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

var (
	identityCipher cipher.Block
	identityOnce   sync.Once
	identityErr    error
)

func identityBlock() (cipher.Block, error) {
	identityOnce.Do(func() {
		raw := os.Getenv(identityKeyEnv)
		if raw == "" {
			identityErr = fmt.Errorf("%s is not set", identityKeyEnv)
			return
		}
		// Any key material is accepted and stretched to 32 bytes. The env value is
		// expected to be `openssl rand -base64 48`.
		sum := sha256.Sum256([]byte(raw))
		identityCipher, identityErr = aes.NewCipher(sum[:])
	})
	return identityCipher, identityErr
}

// ValidateIdentityKey reports whether the identity key is usable. Called during
// config validation so the bridge refuses to start without it, rather than failing
// later at the first ghost — a bridge running without this key would silently emit
// Telegram IDs into Matrix.
func ValidateIdentityKey() error {
	if _, err := identityBlock(); err != nil {
		return fmt.Errorf("identity masking key unavailable (ADR-0001): %w", err)
	}
	return nil
}

// opaqueToken encrypts a (peer type, Telegram ID) pair into a stable token.
//
// Block layout, exactly one AES block:
//
//	[0]     peer type
//	[1:8]   zero — checked on decode, so a wrong key or corrupt token is rejected
//	        rather than silently decoding to a plausible ID
//	[8:16]  big-endian Telegram ID
//
// A single-block AES call is a pseudorandom permutation, so no IV or mode is
// needed and the mapping stays deterministic, which it must be.
func opaqueToken(pt PeerType, id int64) (string, error) {
	block, err := identityBlock()
	if err != nil {
		return "", err
	}
	var buf [aes.BlockSize]byte
	buf[0] = pt.AsByte()
	binary.BigEndian.PutUint64(buf[8:], uint64(id))

	out := make([]byte, aes.BlockSize)
	block.Encrypt(out, buf[:])
	return tokenEncoding.EncodeToString(out), nil
}

// parseOpaqueToken is the exact inverse of opaqueToken.
func parseOpaqueToken(token string) (PeerType, int64, error) {
	block, err := identityBlock()
	if err != nil {
		return "", 0, err
	}
	raw, err := tokenEncoding.DecodeString(token)
	if err != nil {
		return "", 0, fmt.Errorf("invalid identity token encoding: %w", err)
	}
	if len(raw) != aes.BlockSize {
		return "", 0, fmt.Errorf("invalid identity token length %d", len(raw))
	}
	var buf [aes.BlockSize]byte
	block.Decrypt(buf[:], raw)

	for _, b := range buf[1:8] {
		if b != 0 {
			return "", 0, fmt.Errorf("identity token failed integrity check (wrong key?)")
		}
	}
	pt, err := PeerTypeFromByte(buf[0])
	if err != nil {
		return "", 0, fmt.Errorf("identity token has %w", err)
	}
	return pt, int64(binary.BigEndian.Uint64(buf[8:])), nil
}

// OpaqueDisplayToken returns a short, stable, non-reversible-looking fragment for
// use in agent-facing display names.
//
// Every field the displayname template can otherwise reach — FullName, FirstName,
// LastName, Username, UserID — is customer PII, so without this the only safe
// template is a constant, and every customer renders as the same string. This gives
// agents something distinguishable ("Customer a7f3d9e1") that still cannot be
// turned back into a Telegram identity without the key.
//
// Truncated to 8 characters for legibility. It is a display aid, not an identifier:
// the full token in the Matrix ID remains the thing that is actually unique.
func OpaqueDisplayToken(userID int64) string {
	if userID == 0 {
		return ""
	}
	token, err := opaqueToken(PeerTypeUser, userID)
	if err != nil {
		// Fail closed: never fall back to anything Telegram-derived.
		return "unavailable"
	}
	return token[:8]
}
