package commands

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvFilesLayout(t *testing.T) {
	f := NewEnvFiles("/tmp/_temp", "3_1")
	assert.Equal(t, "/tmp/_temp/output_3_1", f.Output)
	assert.Len(t, f.All(), 5)

	env := f.EnvMap()
	assert.Equal(t, f.Output, env["GITHUB_OUTPUT"])
	assert.Equal(t, f.Env, env["GITHUB_ENV"])
	assert.Equal(t, f.Path, env["GITHUB_PATH"])
	assert.Equal(t, f.StepSummary, env["GITHUB_STEP_SUMMARY"])
	assert.Equal(t, f.State, env["GITHUB_STATE"])

	// A second attempt must not read the first attempt's values.
	assert.NotEqual(t, f.Output, NewEnvFiles("/tmp/_temp", "3_2").Output)
}

func TestParseKeyValuesSimple(t *testing.T) {
	kv, err := ParseKeyValues("a=1\nb=two words\n\nc=\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, kv.Order)
	assert.Equal(t, "1", kv.Values["a"])
	assert.Equal(t, "two words", kv.Values["b"])
	v, ok := kv.Get("c")
	assert.True(t, ok)
	assert.Equal(t, "", v)
}

func TestParseKeyValuesValueMayContainEquals(t *testing.T) {
	kv, err := ParseKeyValues("url=https://x/y?a=b\n")
	require.NoError(t, err)
	assert.Equal(t, "https://x/y?a=b", kv.Values["url"])
}

func TestParseKeyValuesHeredoc(t *testing.T) {
	kv, err := ParseKeyValues("json<<EOF\n{\n  \"a\": 1\n}\nEOF\nafter=1\n")
	require.NoError(t, err)
	assert.Equal(t, "{\n  \"a\": 1\n}", kv.Values["json"])
	assert.Equal(t, "1", kv.Values["after"])
	assert.Equal(t, []string{"json", "after"}, kv.Order)
}

func TestParseKeyValuesEmptyHeredoc(t *testing.T) {
	kv, err := ParseKeyValues("empty<<EOF\nEOF\n")
	require.NoError(t, err)
	assert.Equal(t, "", kv.Values["empty"])
}

func TestParseKeyValuesRejectsDelimiterInsideValue(t *testing.T) {
	_, err := ParseKeyValues("k<<EOF\nline with EOF inside\nEOF\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delimiter")
	assert.Contains(t, err.Error(), `"k"`)
}

func TestParseKeyValuesRejectsUnclosedHeredoc(t *testing.T) {
	_, err := ParseKeyValues("k<<EOF\nvalue\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matching delimiter")
}

func TestParseKeyValuesRejectsMalformed(t *testing.T) {
	for name, input := range map[string]string{
		"no separator":    "just a line\n",
		"empty name":      "=value\n",
		"empty delimiter": "k<<\n",
		"heredoc no name": "<<EOF\nx\nEOF\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseKeyValues(input)
			require.Error(t, err)
		})
	}
}

func TestParseKeyValuesEqualsBeforeHeredocIsAssignment(t *testing.T) {
	// "a=b<<c" is a plain assignment whose value contains "<<".
	kv, err := ParseKeyValues("a=b<<c\n")
	require.NoError(t, err)
	assert.Equal(t, "b<<c", kv.Values["a"])
}

func TestParseKeyValuesHeredocDelimiterMayContainEquals(t *testing.T) {
	kv, err := ParseKeyValues("k<<E=F\nvalue\nE=F\n")
	require.NoError(t, err)
	assert.Equal(t, "value", kv.Values["k"])
}

func TestParseKeyValuesLastWriteWins(t *testing.T) {
	kv, err := ParseKeyValues("a=1\na=2\n")
	require.NoError(t, err)
	assert.Equal(t, "2", kv.Values["a"])
	assert.Equal(t, []string{"a"}, kv.Order)
}

func TestParseKeyValuesCRLF(t *testing.T) {
	kv, err := ParseKeyValues("a=1\r\nb<<EOF\r\nx\r\nEOF\r\n")
	require.NoError(t, err)
	assert.Equal(t, "1", kv.Values["a"])
	assert.Equal(t, "x", kv.Values["b"])
}

func TestParsePathFile(t *testing.T) {
	assert.Equal(t, []string{"/opt/bin", "/usr/local/x"},
		ParsePathFile("/opt/bin\n\n  /usr/local/x  \n"))
	assert.Nil(t, ParsePathFile(""))
}
