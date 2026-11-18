// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fixture returns the bytes of testdata/epg.xml.
func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/epg.xml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var fixtureChannels = []Channel{
	{ID: "daserste.example", Names: []string{"Das Erste", "ARD Das Erste"}},
	{ID: "ert1.example", Names: []string{"ΕΡΤ1", "ERT1 HD"}},
	{ID: "ertworld-hd.example", Names: []string{"ERT World HD"}},
	{ID: "ertworld.example", Names: []string{"ERT World"}},
	{ID: "radio-one.example", Names: []string{"Radio One"}},
	{ID: "skai.example", Names: []string{"ΣΚΑΪ"}},
}

func TestParseFixture(t *testing.T) {
	plain := fixture(t)
	for name, in := range map[string][]byte{"plain": plain, "gzip": gz(t, plain)} {
		t.Run(name, func(t *testing.T) {
			r, err := Open(bytes.NewReader(in), MaxSize)
			if err != nil {
				t.Fatal(err)
			}
			x, err := Parse(r)
			if err != nil {
				t.Fatal(err)
			}
			if got := x.Channels(); !reflect.DeepEqual(got, fixtureChannels) {
				t.Errorf("channels = %#v, want %#v", got, fixtureChannels)
			}
		})
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []Channel
		wantErr string
	}{
		{
			name: "sorted by id",
			in:   `<tv><channel id="b"><display-name>B</display-name></channel><channel id="a"><display-name>A</display-name></channel></tv>`,
			want: []Channel{{ID: "a", Names: []string{"A"}}, {ID: "b", Names: []string{"B"}}},
		},
		{
			name: "duplicate ids merge, names deduplicated",
			in:   `<tv><channel id="a"><display-name>One</display-name></channel><channel id="a"><display-name>Two</display-name><display-name>One</display-name></channel></tv>`,
			want: []Channel{{ID: "a", Names: []string{"One", "Two"}}},
		},
		{
			name: "whitespace collapsed, empty names dropped",
			in:   "<tv><channel id=\" a \"><display-name>\n  Das \t Erste </display-name><display-name>  </display-name></channel></tv>",
			want: []Channel{{ID: "a", Names: []string{"Das Erste"}}},
		},
		{
			name: "channel without id skipped, without name kept",
			in:   `<tv><channel><display-name>X</display-name></channel><channel id="a"/></tv>`,
			want: []Channel{{ID: "a"}},
		},
		{
			name: "stops at first programme",
			in:   `<tv><channel id="a"/><programme channel="a"/><channel id="late"/></tv>`,
			want: []Channel{{ID: "a"}},
		},
		{
			name: "latin-1",
			in:   "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><tv><channel id=\"a\"><display-name>Caf\xe9</display-name></channel></tv>",
			want: []Channel{{ID: "a", Names: []string{"Café"}}},
		},
		{name: "no channels", in: `<tv></tv>`, wantErr: "no channels"},
		{name: "empty", in: ``, wantErr: "no channels"},
		{name: "not xml", in: `<html><body>oops</body`, wantErr: "xmltv"},
		{name: "truncated", in: `<tv><channel id="a"><display-name>A`, wantErr: "xmltv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x, err := Parse(strings.NewReader(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := x.Channels(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("channels = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestOpen(t *testing.T) {
	data := []byte("<tv>" + strings.Repeat(" ", 100) + "</tv>")
	tests := []struct {
		name    string
		in      []byte
		limit   int64
		want    []byte
		wantErr error
		wantMsg string
	}{
		{name: "plain", in: data, limit: 1000, want: data},
		{name: "gzip", in: gz(t, data), limit: 1000, want: data},
		{name: "plain at limit", in: data, limit: int64(len(data)), want: data},
		{name: "plain over limit", in: data, limit: int64(len(data)) - 1, wantErr: ErrTooLarge},
		{name: "gzip over limit", in: gz(t, data), limit: int64(len(data)) - 1, wantErr: ErrTooLarge},
		{name: "corrupt gzip", in: append([]byte{0x1f, 0x8b, 8}, bytes.Repeat([]byte{0xff}, 20)...), limit: 1000, wantMsg: "gzip"},
		{name: "one byte", in: []byte("<"), limit: 1000, want: []byte("<")},
		{name: "empty", in: nil, limit: 1000, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Open(bytes.NewReader(tt.in), tt.limit)
			var got []byte
			if err == nil {
				got, err = io.ReadAll(r)
			}
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantMsg != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
					t.Fatalf("err = %v, want %q", err, tt.wantMsg)
				}
			case err != nil:
				t.Fatal(err)
			case !bytes.Equal(got, tt.want):
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseSizeLimitReported(t *testing.T) {
	r, err := Open(strings.NewReader("<tv>"+strings.Repeat(" ", 50)+"</tv>"), 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(r); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func readFixture() ([]byte, error) { return os.ReadFile("testdata/epg.xml") }
