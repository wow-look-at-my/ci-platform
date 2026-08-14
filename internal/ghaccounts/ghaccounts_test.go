package ghaccounts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_RejectsAnEmptyList(t *testing.T) {
	_, err := New("CIPLATFORM_ALLOWED_OWNERS", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no accounts listed")
	assert.Contains(t, err.Error(), "CIPLATFORM_ALLOWED_OWNERS", "the operator has to be told what to fix")

	_, err = New("owners", []string{"", "   "})
	require.Error(t, err, "whitespace is not a decision")
}

func TestContains_IsCaseInsensitiveAndTrimmed(t *testing.T) {
	s, err := New("owners", []string{" PazerOP ", "wow-look-at-my"})
	require.NoError(t, err)

	for _, login := range []string{"PazerOP", "pazerop", "PAZEROP", " pazerop "} {
		assert.True(t, s.Contains(login), login)
	}
	assert.True(t, s.Contains("WOW-LOOK-AT-MY"))
	assert.False(t, s.Contains("pazerop-evil"))
	assert.False(t, s.Contains("evil-pazerop"))
	assert.False(t, s.Contains(""))
	assert.False(t, s.IsAnyone())
}

func TestNew_AnyoneServesEverybodyButOnlyOnItsOwn(t *testing.T) {
	s, err := New("owners", []string{Anyone})
	require.NoError(t, err)
	assert.True(t, s.IsAnyone())
	assert.True(t, s.Contains("a-total-stranger"))

	// Listing both states two intentions, and guessing which one is meant is
	// how an instance ends up serving everybody by accident.
	_, err = New("owners", []string{Anyone, "PazerOP"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "two different intentions")
}

func TestNew_RejectsARepositoryWhereAnAccountBelongs(t *testing.T) {
	_, err := New("owners", []string{"PazerOP/ci-platform"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an account login")
}

func TestLoginsAndString(t *testing.T) {
	s, err := New("owners", []string{"wow-look-at-my", "PazerOP", "pazerop"})
	require.NoError(t, err)
	assert.Equal(t, []string{"PazerOP", "wow-look-at-my"}, s.Logins(), "deduped and sorted")
	assert.Equal(t, "PazerOP, wow-look-at-my", s.String())

	any, err := New("owners", []string{Anyone})
	require.NoError(t, err)
	assert.Contains(t, any.String(), Anyone)
}

func TestParse(t *testing.T) {
	assert.Equal(t, []string{"PazerOP", "wow-look-at-my"}, Parse("PazerOP, wow-look-at-my"))
	assert.Equal(t, []string{"PazerOP", "wow-look-at-my"}, Parse("PazerOP\nwow-look-at-my"))
	assert.Empty(t, Parse("  "))
}
