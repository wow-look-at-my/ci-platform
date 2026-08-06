package mask

import (
	"encoding/base64"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaskLiteral(t *testing.T) {
	m := New()
	m.Add("hunter2secret")
	assert.Equal(t, "token is ***", m.Mask("token is hunter2secret"))
	assert.Equal(t, "nothing here", m.Mask("nothing here"))
}

func TestMaskBase64AndURLRenderings(t *testing.T) {
	secret := "s3cr3t-value/with+chars"
	m := New()
	m.Add(secret)

	for name, rendered := range map[string]string{
		"std":     base64.StdEncoding.EncodeToString([]byte(secret)),
		"rawstd":  base64.RawStdEncoding.EncodeToString([]byte(secret)),
		"url":     base64.URLEncoding.EncodeToString([]byte(secret)),
		"rawurl":  base64.RawURLEncoding.EncodeToString([]byte(secret)),
		"query":   url.QueryEscape(secret),
		"pathesc": url.PathEscape(secret),
		"raw":     secret,
	} {
		t.Run(name, func(t *testing.T) {
			out := m.Mask("prefix " + rendered + " suffix")
			assert.NotContains(t, out, rendered, "rendering %q leaked", name)
			assert.Contains(t, out, Placeholder)
		})
	}
}

func TestMaskMultiLineSecretLineByLine(t *testing.T) {
	key := "-----BEGIN KEY-----\nAAAABBBBCCCC\nDDDDEEEEFFFF\n-----END KEY-----"
	m := New()
	m.Add(key)

	// A tool that prints only the middle of a multi-line secret must still be
	// redacted.
	out := m.Mask("leaked: DDDDEEEEFFFF")
	assert.NotContains(t, out, "DDDDEEEEFFFF")

	whole := m.Mask("dump:\n" + key)
	assert.NotContains(t, whole, "AAAABBBBCCCC")
}

func TestMaskPrefersLongestValue(t *testing.T) {
	m := New()
	m.Add("abcdef")
	m.Add("abcdefghij")
	// The longer value must win, otherwise the tail leaks.
	assert.Equal(t, "***", m.Mask("abcdefghij"))
}

func TestMaskSkipsTooShortValues(t *testing.T) {
	m := New()
	m.Add("ab")
	assert.Equal(t, 0, m.Count())
	assert.Equal(t, "ab is fine", m.Mask("ab is fine"))
}

func TestAddAllIgnoresNames(t *testing.T) {
	m := New()
	m.AddAll(map[string]string{"MY_TOKEN": "abcd1234efgh"})
	out := m.Mask("MY_TOKEN=abcd1234efgh")
	assert.Equal(t, "MY_TOKEN=***", out)
}

func TestMaskEmptyAndNoValues(t *testing.T) {
	m := New()
	assert.Equal(t, "", m.Mask(""))
	assert.Equal(t, "untouched", m.Mask("untouched"))
}

func TestMaskConcurrentAddAndMask(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.Add(strings.Repeat("x", 4+i))
			_ = m.Mask("xxxxxxx")
		}(i)
	}
	wg.Wait()
	require.NotZero(t, m.Count())
}

func TestTrimmedSecretAlsoRegistered(t *testing.T) {
	m := New()
	m.Add("  padded-secret  ")
	assert.Equal(t, "***", m.Mask("padded-secret"))
}
