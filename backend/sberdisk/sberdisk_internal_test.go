package sberdisk

import (
	"testing"
	"time"

	"github.com/rclone/rclone/backend/sberdisk/api"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadCookies(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{
			name:  "bare token",
			input: "abc123",
			want:  map[string]string{"_sa": "abc123"},
		},
		{
			name:  "cookie header",
			input: "_sa=abc123; other=value",
			want:  map[string]string{"_sa": "abc123", "other": "value"},
		},
		{
			name:  "cookie prefix",
			input: "Cookie: _sa=abc123",
			want:  map[string]string{"_sa": "abc123"},
		},
		{
			name:  "newlines",
			input: "_sa=abc123;\nother=value",
			want:  map[string]string{"_sa": "abc123", "other": "value"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cookies := readCookies(test.input)
			got := make(map[string]string, len(cookies))
			for _, cookie := range cookies {
				got[cookie.Name] = cookie.Value
			}
			assert.Equal(t, test.want, got)
		})
	}
}

func TestRevealCookie(t *testing.T) {
	raw := "_sa=abc123; other=value"
	assert.Equal(t, raw, revealCookie(raw))

	obscured, err := obscure.Obscure(raw)
	require.NoError(t, err)
	assert.Equal(t, raw, revealCookie(obscured))

	// A bare cookie value is not a valid obscured value and must survive.
	assert.Equal(t, "abc123", revealCookie("abc123"))
}

func TestAbsPath(t *testing.T) {
	f := &Fs{
		opt: Options{Enc: encoder.Display | encoder.EncodeInvalidUtf8},
	}
	f.setRoot("folder/sub")
	assert.Equal(t, "/folder/sub", f.absPath(""))
	assert.Equal(t, "/folder/sub/file.txt", f.absPath("file.txt"))
	f.setRoot("")
	assert.Equal(t, "/", f.absPath(""))
	assert.Equal(t, "/file.txt", f.absPath("file.txt"))
}

func TestParseTime(t *testing.T) {
	item := &api.File{UploadAt: "Mon Oct 26 2026 22:30:37 GMT+0000"}
	got := parseTime(item)
	require.False(t, got.IsZero())
	assert.Equal(t, 2026, got.Year())
	assert.Equal(t, time.October, got.Month())
	assert.Equal(t, 26, got.Day())

	item = &api.File{LastChangeAt: "2026-10-26T22:30:37Z", UploadAt: "Mon Oct 26 2026 22:30:37 GMT+0000"}
	got = parseTime(item)
	assert.Equal(t, time.Date(2026, time.October, 26, 22, 30, 37, 0, time.UTC), got)

	assert.True(t, parseTime(&api.File{}).IsZero())
}
