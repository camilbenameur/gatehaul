package patch

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math"
	"strings"
	"testing"
)

const (
	testMiB    = 1 << 20
	testHuge   = 1 << 40
	testOver63 = 1 << 63
)

// testBase85 is git's base85 alphabet, written out again here so that the
// tests do not rest on the code they check.
const testBase85 = "0123456789" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ" + "abcdefghijklmnopqrstuvwxyz" + "!#$%&()*+-;<=>?@^_`{|}~"

func testDeflate(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testVarint encodes a size the way a git delta header does.
func testVarint(v uint64) []byte {
	var b []byte
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// testBase85Lines encodes raw as the lines of a binary hunk: at most 52 bytes
// per line, a length letter, then five characters for each group of four
// bytes (the last group is padded with zeros).
func testBase85Lines(raw []byte) string {
	var sb strings.Builder
	for len(raw) > 0 {
		n := min(len(raw), 52)
		chunk := raw[:n]
		raw = raw[n:]
		if n <= 26 {
			sb.WriteByte(byte('A' + n - 1))
		} else {
			sb.WriteByte(byte('a' + n - 27))
		}
		for i := 0; i < n; i += 4 {
			var acc uint32
			for j := 0; j < 4; j++ {
				acc <<= 8
				if i+j < n {
					acc |= uint32(chunk[i+j])
				}
			}
			var group [5]byte
			for k := 4; k >= 0; k-- {
				group[k] = testBase85[acc%85]
				acc /= 85
			}
			sb.Write(group[:])
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// testHunk is one section of a binary patch: the line that declares the
// size, the deflated payload in base85, and the blank line.
func testHunk(t *testing.T, kind string, declared int64, payload []byte) string {
	t.Helper()
	return fmt.Sprintf("%s %d\n", kind, declared) + testBase85Lines(testDeflate(t, payload)) + "\n"
}

// testDelta is a delta header (the size of the source, then of the result)
// followed by one copy instruction.
func testDelta(source, result uint64) []byte {
	d := append(testVarint(source), testVarint(result)...)
	return append(d, 0x91, 0x00, 0x02)
}

func testBinaryPatch(path string, hunks ...string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"index " + strings.Repeat("1", 40) + ".." + strings.Repeat("2", 40) + " 100644\n" +
		"GIT binary patch\n" + strings.Join(hunks, "")
}

// deltaPatch is a binary patch with a forward delta that builds a file of the
// given size, and the usual reverse literal.
func deltaPatch(t *testing.T, path string, result uint64) string {
	t.Helper()
	d := testDelta(testMiB, result)
	return testBinaryPatch(path, testHunk(t, kindDelta, int64(len(d)), d), testHunk(t, kindLiteral, 0, nil))
}

func scan(patch string, o Options) []Problem {
	return scanBinary([]byte(patch), o.withDefaults())
}

func requireOneProblem(t *testing.T, got []Problem, path, reason string) {
	t.Helper()
	if len(got) != 1 || got[0].Path != path || got[0].Reason != reason {
		t.Fatalf("problems = %+v, want one %s on %q", got, reason, path)
	}
}

func TestScanBinaryRefusesAHugeDeclaredDelta(t *testing.T) {
	// A few dozen bytes of delta that ask git for a terabyte.
	requireOneProblem(t, scan(deltaPatch(t, "big.bin", testHuge), Options{}), "big.bin", ReasonTooLarge)
	// A result that does not fit in 63 bits is just as large.
	requireOneProblem(t, scan(deltaPatch(t, "big.bin", testOver63), Options{}), "big.bin", ReasonTooLarge)
	requireOneProblem(t, scan(deltaPatch(t, "big.bin", math.MaxUint64), Options{}), "big.bin", ReasonTooLarge)
}

func TestScanBinaryDeltaLimits(t *testing.T) {
	o := Options{MaxFileBytes: 1000, MaxTotalBytes: 1500}
	if got := scan(deltaPatch(t, "ok.bin", 1000), o); len(got) != 0 {
		t.Fatalf("a result at the limit must pass: %+v", got)
	}
	requireOneProblem(t, scan(deltaPatch(t, "big.bin", 1001), o), "big.bin", ReasonTooLarge)

	// Two files that are each fine, but not together.
	both := deltaPatch(t, "one.bin", 1000) + deltaPatch(t, "two.bin", 1000)
	requireOneProblem(t, scan(both, o), "", ReasonTooLarge)
}

func TestScanBinaryLiteralLimits(t *testing.T) {
	o := Options{MaxFileBytes: 100, MaxTotalBytes: 1000}
	small := []byte("small")
	ok := testBinaryPatch("a.bin", testHunk(t, kindLiteral, 100, small), testHunk(t, kindLiteral, 900, small))
	if got := scan(ok, o); len(got) != 0 {
		t.Fatalf("limits are not exceeded, got %+v", got)
	}
	// The size is what the patch declares, whatever the data really holds.
	big := testBinaryPatch("a.bin", testHunk(t, kindLiteral, 101, small))
	requireOneProblem(t, scan(big, o), "a.bin", ReasonTooLarge)
	huge := testBinaryPatch("a.bin", testHunk(t, kindLiteral, 99999999999999, small))
	requireOneProblem(t, scan(huge, Options{}), "a.bin", ReasonTooLarge)
	overflow := testBinaryPatch("a.bin", "literal 99999999999999999999999\n"+testBase85Lines(testDeflate(t, small))+"\n")
	requireOneProblem(t, scan(overflow, Options{}), "a.bin", ReasonTooLarge)

	// The reverse section is not applied, but git inflates it: it has a limit.
	reverse := testBinaryPatch("a.bin", testHunk(t, kindLiteral, 5, small), testHunk(t, kindLiteral, 1001, small))
	requireOneProblem(t, scan(reverse, o), "a.bin", ReasonTooLarge)
}

func TestScanBinaryAllSectionsTogether(t *testing.T) {
	o := Options{MaxFileBytes: 100, MaxTotalBytes: 100}
	small := []byte("small")
	var sb strings.Builder
	for i := 0; i < 5; i++ {
		sb.WriteString(testBinaryPatch(fmt.Sprintf("f%d.bin", i), testHunk(t, kindLiteral, 1, small), testHunk(t, kindLiteral, 100, small)))
	}
	requireOneProblem(t, scan(sb.String(), o), "", ReasonTooLarge)
}

func TestScanBinaryPathOfEachFile(t *testing.T) {
	o := Options{MaxFileBytes: 100}
	small := []byte("small")
	p := testBinaryPatch("first.bin", testHunk(t, kindLiteral, 10, small), testHunk(t, kindLiteral, 10, small)) +
		testBinaryPatch("dir/second.bin", testHunk(t, kindLiteral, 500, small))
	requireOneProblem(t, scan(p, o), "dir/second.bin", ReasonTooLarge)
}

func TestScanBinaryGitOutput(t *testing.T) {
	// What "git diff --binary" wrote for a 3000-byte file with one byte
	// changed: a delta whose result is 3000 bytes, and its reverse.
	genuine := "diff --git a/f.bin b/f.bin\n" +
		"index 79e1d6442d3808f831b26cab89fd483cd86884fb..b57e767290628bcbf5ccce1eeeb90c3f4620c8a0 100644\n" +
		"GIT binary patch\n" +
		"delta 12\n" +
		"TcmdlXzC(OM3S-2^)L?D^A4UXV\n" +
		"\n" +
		"delta 12\n" +
		"TcmdlXzC(OM3ge=UslnU;Ah-mo\n" +
		"\n"
	if got := scan(genuine, Options{MaxFileBytes: 3000}); len(got) != 0 {
		t.Fatalf("a genuine patch must pass: %+v", got)
	}
	requireOneProblem(t, scan(genuine, Options{MaxFileBytes: 2999}), "f.bin", ReasonTooLarge)
	// The same text without its last blank line is cut short.
	requireOneProblem(t, scan(strings.TrimSuffix(genuine, "\n"), Options{}), "f.bin", ReasonUnparsable)
}

func TestScanBinaryEncoderAgreesWithGit(t *testing.T) {
	// The test encoder must produce what git produced for the same bytes:
	// the deflated 12-byte delta of the genuine patch above.
	want := "TcmdlXzC(OM3S-2^)L?D^A4UXV\n"
	raw, err := decodeLine(strings.TrimSuffix(want, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := testBase85Lines(raw); got != want {
		t.Fatalf("encoder gave %q, git gave %q", got, want)
	}
}

func TestScanBinaryUnreadableHunks(t *testing.T) {
	good := testHunk(t, kindLiteral, 5, []byte("small"))
	firstData := strings.Split(good, "\n")[1]
	tests := map[string]string{
		"no hunk":            "GIT binary patch\nnot a hunk\n",
		"no section at all":  "GIT binary patch\n",
		"bad size":           "GIT binary patch\nliteral x\n" + firstData + "\n\n",
		"negative size":      "GIT binary patch\nliteral -5\n" + firstData + "\n\n",
		"bad alphabet":       "GIT binary patch\nliteral 5\n" + firstData[:3] + " " + firstData[4:] + "\n\n",
		"bad length letter":  "GIT binary patch\nliteral 5\n!" + firstData[1:] + "\n\n",
		"wrong line length":  "GIT binary patch\nliteral 5\n" + firstData + "0\n\n",
		"length over data":   "GIT binary patch\nliteral 5\nZ" + firstData[1:] + "\n\n",
		"cut short":          "GIT binary patch\nliteral 5\n" + firstData + "\n",
		"base85 overflow":    "GIT binary patch\nliteral 5\nD|||||\n\n",
		"delta not deflated": "GIT binary patch\ndelta 4\nD00000\n\n",
		"delta cut short":    "GIT binary patch\n" + testHunk(t, kindDelta, 2, []byte{0x80}),
	}
	for name, body := range tests {
		got := scan("diff --git a/x.bin b/x.bin\n"+body, Options{})
		if len(got) != 1 || got[0].Reason != ReasonUnparsable || got[0].Path != "x.bin" {
			t.Errorf("%s: problems = %+v, want one %s on x.bin", name, got, ReasonUnparsable)
		}
	}
}

func TestScanBinaryIgnoresOtherPatches(t *testing.T) {
	text := "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-a\n+b\n"
	if got := scan(text, Options{}); len(got) != 0 {
		t.Fatalf("a text patch has no binary section: %+v", got)
	}
	// The words in a hunk body carry a prefix, so they are not a header.
	body := text + " GIT binary patch\n+GIT binary patch\n"
	if got := scan(body, Options{}); len(got) != 0 {
		t.Fatalf("a line of a text hunk is not a header: %+v", got)
	}
	// Git wants exactly "GIT binary patch" and a newline: with a carriage
	// return it does not see a binary section.
	if got := scan("diff --git a/x b/x\nGIT binary patch\r\nliteral 5\n", Options{}); len(got) != 0 {
		t.Fatalf("a header with a carriage return is not a header: %+v", got)
	}
}

func TestDecodeLine(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 26, 27, 51, 52} {
		raw := make([]byte, n)
		for i := range raw {
			raw[i] = byte(i*37 + 11)
		}
		line := strings.TrimSuffix(testBase85Lines(raw), "\n")
		got, err := decodeLine(line)
		if err != nil || !bytes.Equal(got, raw) {
			t.Errorf("round trip of %d bytes: %v, %v", n, got, err)
		}
	}
}

func TestReadVarint(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, testMiB, testHuge + 5, math.MaxInt64} {
		got, err := readVarint(bytes.NewReader(testVarint(v)))
		if err != nil || uint64(got) != v {
			t.Errorf("readVarint(%d) = %d, %v", v, got, err)
		}
	}
	if got, err := readVarint(bytes.NewReader(testVarint(testOver63))); err != nil || got != math.MaxInt64 {
		t.Errorf("a size over 63 bits must clamp, got %d, %v", got, err)
	}
	if _, err := readVarint(bytes.NewReader([]byte{0x80, 0x80})); err == nil {
		t.Error("a varint that stops in the middle must fail")
	}
	if _, err := readVarint(bytes.NewReader(bytes.Repeat([]byte{0xff}, 12))); err == nil {
		t.Error("a varint of more than ten bytes must fail")
	}
}

func TestDiffPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a/x b/x", "x"},
		{"a/dir/f.bin b/dir/f.bin", "dir/f.bin"},
		{"a/with space b/with space", "with space"},
		{"a/x b/y", "a/x b/y"},
		{`"a/x\ty" "b/x\ty"`, `"a/x\ty" "b/x\ty"`},
		{"", ""},
	}
	for _, tt := range tests {
		if got := diffPath(tt.in); got != tt.want {
			t.Errorf("diffPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
