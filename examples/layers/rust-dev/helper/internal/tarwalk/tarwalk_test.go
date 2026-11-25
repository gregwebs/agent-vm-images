package tarwalk

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// writeXZ compresses raw tar bytes into a temp .tar.xz and returns its path.
func writeXZ(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar.xz")
	handle, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := xz.NewWriter(handle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// tarBytes builds a tar archive with the given format.
func tarBytes(t *testing.T, format tar.Format, entries []tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for index := range entries {
		header := entries[index]
		header.Format = format
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatalf("write header %q: %v", header.Name, err)
		}
		if body, present := bodies[header.Name]; present {
			if _, err := writer.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// rawHeader is a valid 512-byte tar header, so `archive/tar` accepts it in the
// second pass too.
func rawHeader(name string, mode int64, size int64, typeflag byte, link string) []byte {
	block := make([]byte, 512)
	copy(block[0:100], name)
	putOctal(block[100:108], mode)
	putOctal(block[108:116], 0)
	putOctal(block[116:124], 0)
	putOctal(block[124:136], size)
	putOctal(block[136:148], 0)
	block[148] = ' '
	for index := 148; index < 156; index++ {
		block[index] = ' '
	}
	block[156] = typeflag
	copy(block[157:257], link)
	copy(block[257:263], "ustar")
	block[263] = '0'
	block[264] = '0'
	copy(block[265:297], "root")
	copy(block[297:329], "root")
	var sum int64
	for _, b := range block {
		sum += int64(b)
	}
	putOctal(block[148:156], sum)
	block[154] = 0
	block[155] = ' '
	return block
}

func putOctal(field []byte, value int64) {
	text := fmt.Sprintf("%0*o", len(field)-1, value)
	copy(field, text)
	field[len(field)-1] = 0
}

// paxRecord frames one PAX key=value record.
func paxRecord(key, value string) string {
	body := " " + key + "=" + value + "\n"
	length := len(body) + 1
	for {
		next := len(body) + len(strconv.Itoa(length))
		if next == length {
			break
		}
		length = next
	}
	return strconv.Itoa(length) + body
}

func blocks(payload []byte) []byte {
	padding := (512 - len(payload)%512) % 512
	return append(append([]byte{}, payload...), make([]byte, padding)...)
}

func endBlocks() []byte { return make([]byte, 1024) }

// TestDecodedStreamLimit is row 9: the cap counts the whole decoded stream, not
// only member payloads.
func TestDecodedStreamLimit(t *testing.T) {
	archive := writeXZ(t, tarBytes(t, tar.FormatUSTAR, []tar.Header{
		{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "dir/file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2048},
	}, map[string]string{"dir/file": strings.Repeat("x", 2048)}))
	err := Scan(archive, "archive.tar.xz", Limits{DecodedBytes: 1024},
		func(*tar.Header, *tar.Reader, bool) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "decoded stream exceeds 1024 bytes") {
		t.Fatalf("refusal is %v, want the decoded-stream cap", err)
	}
}

// TestXZStreamIntegrity is row 10, with Q3's strictness stated in place: the
// deleted Python reader stopped at the first stream's EOF and ignored trailing
// bytes; SingleStream refuses them. An authenticated archive cannot carry
// trailing bytes (its checksum is compared before this decoder sees it), so this
// is unreachable defence rather than a reachable behaviour difference -- a claim
// now, with no second implementation to make it measurable (#245).
func TestXZStreamIntegrity(t *testing.T) {
	valid := tarBytes(t, tar.FormatUSTAR, []tar.Header{
		{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
	}, map[string]string{"file": "data"})
	path := writeXZ(t, valid)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	visit := func(*tar.Header, *tar.Reader, bool) error { return nil }
	cases := []struct {
		name  string
		bytes []byte
	}{
		{"truncated", raw[:len(raw)-12]},
		{"corrupt", append(append([]byte{}, raw[:24]...), append([]byte{0xff, 0xff},
			raw[26:]...)...)},
		// Non-stream trailing garbage is refused by the decoder; the
		// SingleStream-specific case (a valid second stream) is
		// TestSingleStreamRefusesConcatenatedXZ.
		{"trailing_bytes", append(append([]byte{}, raw...), []byte("garbage")...)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), test.name+".tar.xz")
			if err := os.WriteFile(path, test.bytes, 0o600); err != nil {
				t.Fatal(err)
			}
			err := Scan(path, "archive.tar.xz", Limits{}, visit)
			if err == nil {
				t.Fatalf("%s xz stream was accepted", test.name)
			}
			if !strings.Contains(err.Error(), "archive.tar.xz") {
				t.Errorf("refusal does not name the archive: %v", err)
			}
		})
	}
}

// TestExtendedHeaderBounds is row 11: each GNU/PAX header is bounded, and their
// total is bounded.
func TestExtendedHeaderBounds(t *testing.T) {
	visit := func(*tar.Header, *tar.Reader, bool) error { return nil }
	t.Run("gnu_longname", func(t *testing.T) {
		archive := writeXZ(t, tarBytes(t, tar.FormatGNU, []tar.Header{
			{Name: strings.Repeat("a", 70000), Typeflag: tar.TypeReg, Mode: 0o644},
		}, nil))
		err := Scan(archive, "a.tar.xz", Limits{}, visit)
		if err == nil || !strings.Contains(err.Error(), "extended header") {
			t.Fatalf("refusal is %v, want a bounded extended header", err)
		}
	})
	t.Run("pax_longname", func(t *testing.T) {
		archive := writeXZ(t, tarBytes(t, tar.FormatPAX, []tar.Header{
			{Name: strings.Repeat("b", 70000), Typeflag: tar.TypeReg, Mode: 0o644},
		}, nil))
		err := Scan(archive, "b.tar.xz", Limits{}, visit)
		if err == nil || !strings.Contains(err.Error(), "extended header") {
			t.Fatalf("refusal is %v, want a bounded extended header", err)
		}
	})
	t.Run("cumulative_total", func(t *testing.T) {
		entries := make([]tar.Header, 0, 6)
		for index := 0; index < 6; index++ {
			entries = append(entries, tar.Header{
				Name: strings.Join([]string{strings.Repeat("c", 50000),
					fmt.Sprintf("%d", index)}, "/"), Typeflag: tar.TypeReg, Mode: 0o644})
		}
		archive := writeXZ(t, tarBytes(t, tar.FormatPAX, entries, nil))
		err := Scan(archive, "c.tar.xz", Limits{}, visit)
		if err == nil || !strings.Contains(err.Error(), "extended headers total") {
			t.Fatalf("refusal is %v, want the cumulative extended-header cap", err)
		}
	})
	t.Run("modest_passes", func(t *testing.T) {
		archive := writeXZ(t, tarBytes(t, tar.FormatPAX, []tar.Header{
			{Name: strings.Repeat("m", 300), Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
		}, map[string]string{strings.Repeat("m", 300): "ok"}))
		if err := Scan(archive, "m.tar.xz", Limits{}, visit); err != nil {
			t.Fatalf("a modest PAX header was refused: %v", err)
		}
	})
}

// TestExtendedHeaderBoundaryAttacks is row 12. Each fixture is a hand-built tar
// block sequence, because a conforming writer cannot express these shapes.
func TestExtendedHeaderBoundaryAttacks(t *testing.T) {
	hidden := blocks(rawHeader("hidden", 0o644, 70000, 'L', ""))
	pax := func(kind byte, records string) []byte {
		payload := []byte(records)
		return append(rawHeader("pax", 0o644, int64(len(payload)), kind, ""),
			blocks(payload)...)
	}
	member := func(name string, typeflag byte, size int64) []byte {
		return rawHeader(name, 0o644, size, typeflag, "")
	}
	data := func(size int, content []byte) []byte {
		out := make([]byte, size)
		copy(out, content)
		return out
	}
	cases := []struct {
		name     string
		raw      []byte
		fragment string
	}{
		{"plain_hidden", hidden, "extended header"},
		{"pax_size_override",
			join(pax('x', paxRecord("size", "512")), member("plain", '0', 0), data(512, hidden)),
			""},
		{"pax_global_size_override",
			join(pax('g', paxRecord("size", "512")), member("plain", '0', 0), data(512, hidden)),
			""},
		{"pax_stacked_latest_wins",
			join(pax('x', paxRecord("size", "512")), pax('x', paxRecord("size", "0")),
				member("plain", '0', 0), data(512, hidden)),
			"extended header"},
		{"pax_unreadable_size",
			join(pax('x', paxRecord("size", "abc")), member("x", '0', 0)),
			"unreadable PAX size"},
		{"pax_sparse",
			join(pax('x', paxRecord("GNU.sparse.0", "1")), member("x", '0', 0)),
			"sparse PAX metadata"},
		{"gnu_sparse", member("x", 'S', 0), "GNU sparse"},
		{"v7_dir_data", join(member("x/", 0, 5), data(5, []byte("data"))),
			"V7 directory header"},
		{"pax_size_v7_dir",
			join(pax('x', paxRecord("size", "5")), member("x/", 0, 0)),
			"V7 directory header"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			raw := join(test.raw, endBlocks())
			path := filepath.Join(t.TempDir(), "attack.tar.xz")
			writeXZAt(t, path, raw)
			err := boundExtendedHeadersOnly(t, path)
			if test.fragment == "" {
				if err != nil {
					t.Fatalf("pre-pass refused a well-formed override: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.fragment) {
				t.Fatalf("pre-pass refusal is %v, want %q", err, test.fragment)
			}
		})
	}
	t.Run("v7_dir_zero_passes", func(t *testing.T) {
		raw := join(member("x/", 0, 0), endBlocks())
		path := filepath.Join(t.TempDir(), "v7.tar.xz")
		writeXZAt(t, path, raw)
		if err := boundExtendedHeadersOnly(t, path); err != nil {
			t.Fatalf("a zero-data V7 directory was refused: %v", err)
		}
	})
}

func boundExtendedHeadersOnly(t *testing.T, path string) error {
	t.Helper()
	_, err := boundExtendedHeaders(path, "attack.tar.xz", Limits{}.withDefaults())
	return err
}

func join(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// TestSingleStreamRefusesConcatenatedXZ is the direct Q3 witness: a second valid
// xz stream after the first is decoded by a multi-stream reader and refused by
// SingleStream. The tar-level trailing-bytes row cannot distinguish the two,
// because a tar parser stops before the decoder has to.
func TestSingleStreamRefusesConcatenatedXZ(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concatenated.tar.xz")
	var buffer bytes.Buffer
	for _, text := range []string{"one", "two"} {
		writer, err := xz.NewWriter(&buffer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, closeStream, err := decoded(path, "concatenated.tar.xz", MaxDecodedBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStream()
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("a concatenated second xz stream was accepted")
	}
}

func writeXZAt(t *testing.T, path string, raw []byte) {
	t.Helper()
	handle, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := xz.NewWriter(handle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
}
