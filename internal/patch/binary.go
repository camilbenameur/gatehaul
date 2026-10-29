package patch

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// A "git diff --binary" patch carries a binary change as one or two sections
// after a "GIT binary patch" line:
//
//	literal <size>   the new content, deflated
//	delta <size>     a git delta against the old content, deflated
//
// followed by lines of base85 and a blank line. The size is the size of the
// data once inflated: the whole new file for a literal, the delta itself for
// a delta, whose header holds the size of the file it builds. Git allocates
// those sizes before it checks anything else, so a few kilobytes of patch can
// make "git apply" ask for gigabytes. scanBinary reads the declared sizes and
// refuses the patch before git sees it. The second section of a file is the
// reverse change, which git parses but does not apply.

const (
	binaryHeader = "GIT binary patch"

	// base85Alphabet is the alphabet of git's base85.c, in value order.
	base85Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!#$%&()*+-;<=>?@^_`{|}~"

	// deltaHeaderBytes bounds what is inflated to read the two sizes at the
	// start of a delta: two varints of at most ten bytes.
	deltaHeaderBytes = 20
)

const (
	kindLiteral = "literal"
	kindDelta   = "delta"
)

var (
	errNoHunk    = errors.New("no literal or delta line after the binary patch line")
	errBadHunk   = errors.New("a binary hunk cannot be read")
	errBadLine   = errors.New("a line of a binary hunk is not valid base85")
	errShortHunk = errors.New("a binary hunk is cut short")
	errBadDelta  = errors.New("the header of a binary delta cannot be read")
)

// base85Index maps a byte to its value in base85Alphabet, or -1.
var base85Index = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(base85Alphabet); i++ {
		t[base85Alphabet[i]] = int8(i)
	}
	return t
}()

// binaryHunk is one section of a binary patch.
type binaryHunk struct {
	kind string
	size int64  // as declared on the "literal" or "delta" line
	data []byte // the deflated payload
}

// scanBinary returns the problems of the binary sections of a patch: a
// declared size over the limits, or a section that cannot be read. It does
// not run git and does not look at anything else. The problems are sorted.
//
// Limits: the new file of a forward section (a literal, or the result of a
// delta) may not exceed o.MaxFileBytes, those files together o.MaxTotalBytes,
// and no section may declare more than o.MaxTotalBytes of inflated data, nor
// all of them more than twice that.
func scanBinary(data []byte, o Options) []Problem {
	if !bytes.Contains(data, []byte(binaryHeader)) {
		return nil
	}
	var col collector
	var results, declared int64
	path := ""
	lines := strings.Split(string(data), "\n")
	// The last element has no newline after it and is not a line.
	for i := 0; i < len(lines)-1; i++ {
		line := lines[i]
		if rest, ok := strings.CutPrefix(line, "diff --git "); ok {
			path = diffPath(rest)
			continue
		}
		if line != binaryHeader {
			continue
		}
		i++
		for n := 0; ; n++ {
			h, next, err := readHunk(lines, i)
			if errors.Is(err, errNoHunk) && n > 0 {
				break
			}
			if err != nil {
				col.add(path, ReasonUnparsable, err.Error())
				return col.sorted()
			}
			i = next

			if h.size > o.MaxTotalBytes {
				col.add(path, ReasonTooLarge, fmt.Sprintf("a binary hunk declares %d bytes of data, the limit is %d", h.size, o.MaxTotalBytes))
			} else {
				declared = satAdd(declared, h.size)
			}
			if n > 0 {
				continue
			}
			result := h.size
			if h.kind == kindDelta {
				if result, err = deltaResultSize(h.data); err != nil {
					col.add(path, ReasonUnparsable, err.Error())
					return col.sorted()
				}
			}
			if result > o.MaxFileBytes {
				col.add(path, ReasonTooLarge, fmt.Sprintf("a binary hunk builds a file of %d bytes, the limit is %d", result, o.MaxFileBytes))
			} else {
				results = satAdd(results, result)
			}
		}
		// The loop above stopped on a line that is not a hunk of this file.
		i--
	}
	if results > o.MaxTotalBytes {
		col.add("", ReasonTooLarge, fmt.Sprintf("the binary hunks build %d bytes in new files, the limit is %d", results, o.MaxTotalBytes))
	}
	if declared/2 > o.MaxTotalBytes {
		col.add("", ReasonTooLarge, fmt.Sprintf("the binary hunks declare %d bytes of data, the limit is twice %d", declared, o.MaxTotalBytes))
	}
	return col.sorted()
}

// satAdd adds two non-negative numbers without overflowing.
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// diffPath reads the path from the rest of a "diff --git " line, which is
// "a/<path> b/<path>" when nothing is renamed. Anything else, such as a
// quoted name, is returned as it is, on one line.
func diffPath(rest string) string {
	if n := len(rest) - len("a/ b/"); n > 0 && n%2 == 0 {
		half := n / 2
		if p := rest[2 : 2+half]; rest[:2] == "a/" && rest[2+half:5+half] == " b/" && rest[5+half:] == p {
			return p
		}
	}
	return oneLine([]byte(rest))
}

// readHunk reads the section that starts at lines[i] and returns it with the
// index of the line after its blank line. It returns errNoHunk when lines[i]
// is not a "literal" or "delta" line. Like git, it accepts nothing it cannot
// read exactly.
func readHunk(lines []string, i int) (binaryHunk, int, error) {
	var h binaryHunk
	if i >= len(lines)-1 {
		return h, i, errNoHunk
	}
	head := lines[i]
	var num string
	switch {
	case strings.HasPrefix(head, kindLiteral+" "):
		h.kind, num = kindLiteral, head[len(kindLiteral)+1:]
	case strings.HasPrefix(head, kindDelta+" "):
		h.kind, num = kindDelta, head[len(kindDelta)+1:]
	default:
		return h, i, errNoHunk
	}
	size, err := strconv.ParseInt(num, 10, 64)
	if size < 0 || (err != nil && (!errors.Is(err, strconv.ErrRange) || size <= 0)) {
		return h, i, errBadHunk
	}
	h.size = size
	i++
	for {
		if i >= len(lines)-1 {
			return h, i, errShortHunk
		}
		line := lines[i]
		i++
		if line == "" {
			return h, i, nil
		}
		chunk, err := decodeLine(line)
		if err != nil {
			return h, i, err
		}
		h.data = append(h.data, chunk...)
	}
}

// decodeLine decodes one data line of a binary hunk: a letter that gives the
// number of bytes (A to Z for 1 to 26, a to z for 27 to 52) and groups of
// five base85 characters, four bytes each. It follows the checks of git's
// apply.c and base85.c.
func decodeLine(line string) ([]byte, error) {
	if len(line) < 6 || (len(line)-1)%5 != 0 {
		return nil, errBadLine
	}
	var n int
	switch c := line[0]; {
	case c >= 'A' && c <= 'Z':
		n = int(c-'A') + 1
	case c >= 'a' && c <= 'z':
		n = int(c-'a') + 27
	default:
		return nil, errBadLine
	}
	capacity := (len(line) - 1) / 5 * 4
	if n > capacity || n <= capacity-4 {
		return nil, errBadLine
	}
	out := make([]byte, 0, capacity)
	for j := 1; j < len(line); j += 5 {
		var acc uint64
		for k := 0; k < 5; k++ {
			d := base85Index[line[j+k]]
			if d < 0 {
				return nil, errBadLine
			}
			acc = acc*85 + uint64(d)
		}
		if acc > math.MaxUint32 {
			return nil, errBadLine
		}
		out = append(out, byte(acc>>24), byte(acc>>16), byte(acc>>8), byte(acc))
	}
	return out[:n], nil
}

// deltaResultSize returns the size of the file that a deflated git delta
// builds: the second varint of its header, after the size of the source. It
// inflates no more than the first few bytes. A size that does not fit in an
// int64 reads as math.MaxInt64.
func deltaResultSize(deflated []byte) (int64, error) {
	zr, err := zlib.NewReader(bytes.NewReader(deflated))
	if err != nil {
		return 0, errBadDelta
	}
	defer func() { _ = zr.Close() }()
	r := io.LimitReader(zr, deltaHeaderBytes)
	if _, err := readVarint(r); err != nil {
		return 0, err
	}
	return readVarint(r)
}

// readVarint reads a size as git's delta header stores it: seven bits per
// byte, least significant first, the high bit set while more bytes follow.
func readVarint(r io.Reader) (int64, error) {
	var v uint64
	over := false
	var b [1]byte
	for shift := uint(0); shift < 70; shift += 7 {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, errBadDelta
		}
		g := uint64(b[0] & 0x7f)
		if shift >= 64 || (g<<shift)>>shift != g {
			over = true
		} else {
			v |= g << shift
		}
		if b[0]&0x80 == 0 {
			if over || v > math.MaxInt64 {
				return math.MaxInt64, nil
			}
			return int64(v), nil
		}
	}
	return 0, errBadDelta
}
