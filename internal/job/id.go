package job

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// crockford is the Crockford base32 alphabet used by ULIDs.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a job id: the host number, a dash, then a ULID (26 characters
// of Crockford base32: 48 bits of milliseconds, then 80 random bits). Ids sort
// by creation time within one host.
func NewID(host int, now time.Time, entropy io.Reader) (string, error) {
	if host < 1 || host > 9999 {
		return "", fmt.Errorf("host number %d is out of range 1..9999", host)
	}
	ms := now.UnixMilli()
	if ms < 0 || ms >= 1<<48 {
		return "", fmt.Errorf("time %s does not fit in 48 bits of milliseconds", now.UTC().Format(time.RFC3339))
	}
	var b [16]byte
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	if _, err := io.ReadFull(entropy, b[6:]); err != nil {
		return "", fmt.Errorf("reading entropy for a job id: %w", err)
	}
	return strconv.Itoa(host) + "-" + encodeULID(b), nil
}

// encodeULID writes the 128 bits of b as 26 base32 characters, the first one
// carrying the 3 highest bits.
func encodeULID(b [16]byte) string {
	var out [26]byte
	idx := len(out) - 1
	var acc uint
	bits := 0
	for i := len(b) - 1; i >= 0; i-- {
		acc |= uint(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[idx] = crockford[acc&31]
			idx--
			acc >>= 5
			bits -= 5
		}
	}
	if bits > 0 {
		out[idx] = crockford[acc&31]
	}
	return string(out[:])
}

// HostNum returns the host number at the start of a job id.
func HostNum(id string) (int, bool) {
	head, _, ok := strings.Cut(id, "-")
	if !ok || head == "" || head[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(head)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// ULID returns the part of a job id after the host number.
func ULID(id string) string {
	_, rest, _ := strings.Cut(id, "-")
	return rest
}

// BranchKey returns the key that stands for a full ref name in remote paths
// (docs/SPEC.md §5): a slug of the short ref name, then the first 6 hex digits
// of the sha256 of the full ref. The hash keeps "a/b", "A" and "é" apart.
// A detached HEAD uses the ref "HEAD".
func BranchKey(ref string) string {
	short := ref
	for _, p := range []string{"refs/heads/", "refs/tags/", "refs/remotes/", "refs/"} {
		if s, ok := strings.CutPrefix(ref, p); ok {
			short = s
			break
		}
	}
	sum := sha256.Sum256([]byte(ref))
	return slug(short) + "-" + hex.EncodeToString(sum[:])[:6]
}

// slug lowers s to [a-z0-9-], collapses and trims dashes, and keeps at most
// 32 characters. An empty result is "ref".
func slug(s string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			sb.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(sb.String(), "-")
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	if out == "" {
		return "ref"
	}
	return out
}

// errEmptyPrefix is returned for a lookup with no characters.
var errEmptyPrefix = errors.New("empty job id")
