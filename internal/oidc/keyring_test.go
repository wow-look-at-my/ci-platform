// Keyring rotation, persistence, and JWKS tests.
package oidc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/blob/disk"
	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
	"github.com/wow-look-at-my/ci-platform/internal/oidc"
)

func mustKeyring(t *testing.T) *oidc.Keyring {
	t.Helper()
	kr, _, _ := newKeyring(t, 15*time.Minute)
	return kr
}

func TestIssueRefusesIncompleteSubjects(t *testing.T) {
	svc, _, _ := newService(t, fullSubject(), jobtoken.DefaultScopes)

	_, err := svc.Issue(nil, "aud")
	assert.Error(t, err)

	_, err = svc.Issue(fullSubject(), "")
	assert.Error(t, err)

	blank := &oidc.Subject{RunID: 1, RunAttempt: 1}
	_, err = svc.Issue(blank, "aud")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Repository")
	assert.Contains(t, err.Error(), "WorkflowRef")

	noRun := fullSubject()
	noRun.RunID = 0
	_, err = svc.Issue(noRun, "aud")
	assert.Error(t, err)
}

func TestDiscoveryDocument(t *testing.T) {
	svc, _, _ := newService(t, fullSubject(), jobtoken.DefaultScopes)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + oidc.PathDiscovery)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var doc struct {
		Issuer   string   `json:"issuer"`
		JWKSURI  string   `json:"jwks_uri"`
		Algs     []string `json:"id_token_signing_alg_values_supported"`
		Claims   []string `json:"claims_supported"`
		Response []string `json:"response_types_supported"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	assert.Equal(t, issuer, doc.Issuer)
	assert.Equal(t, issuer+"/.well-known/jwks.json", doc.JWKSURI)
	assert.Equal(t, []string{"RS256"}, doc.Algs)
	assert.Contains(t, doc.Claims, "job_workflow_ref")
	assert.Contains(t, doc.Claims, "runner_environment")
	assert.Contains(t, doc.Response, "id_token")
}

func TestKeyRotation(t *testing.T) {
	ctx := context.Background()
	kr, _, _ := newKeyring(t, 15*time.Minute)
	svc, err := oidc.New(oidc.Options{
		Issuer: issuer, Keyring: kr, Verifier: newSigner(t, jobtoken.DefaultScopes).Verifier(),
		Lookup: func(context.Context, int64, int64, int) (*oidc.Subject, error) { return fullSubject(), nil },
	})
	require.NoError(t, err)

	before, err := svc.Issue(fullSubject(), "aud")
	require.NoError(t, err)
	_, oldKID := kr.Active()

	require.NoError(t, kr.Rotate(ctx))
	_, newKID := kr.Active()
	assert.NotEqual(t, oldKID, newKID)

	after, err := svc.Issue(fullSubject(), "aud")
	require.NoError(t, err)

	// Both keys stay in JWKS, so the token minted before rotation still
	// verifies. Dropping the old key immediately would break tokens in flight.
	jwks := kr.JWKS()
	require.Len(t, jwks.Keys, 2)
	kids := []string{jwks.Keys[0].Kid, jwks.Keys[1].Kid}
	assert.Contains(t, kids, oldKID)
	assert.Contains(t, kids, newKID)

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	keyFn := jwksKeyFunc(t, srv.URL)
	for _, tok := range []string{before, after} {
		_, err := jwt.Parse(tok, keyFn, jwt.WithIssuer(issuer), jwt.WithAudience("aud"))
		assert.NoError(t, err)
	}
}

func TestRetiredKeysAreDroppedOnceTheirTokensExpire(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oidc.json")
	ks, err := oidc.NewFileKeyStore(path)
	require.NoError(t, err)

	now := time.Now()
	clock := func() time.Time { return now }
	kr, err := oidc.NewKeyring(ctx, ks, oidc.KeyringOptions{TokenTTL: 15 * time.Minute, Now: clock})
	require.NoError(t, err)

	require.NoError(t, kr.Rotate(ctx))
	assert.Len(t, kr.JWKS().Keys, 2)

	// Past the TTL the retired key can no longer verify anything, so it goes.
	now = now.Add(30 * time.Minute)
	require.NoError(t, kr.Rotate(ctx))
	jwks := kr.JWKS()
	assert.Len(t, jwks.Keys, 2, "the just-retired key stays; the long-retired one is dropped")
}

func TestKeysArePersistedAndReloaded(t *testing.T) {
	ctx := context.Background()
	kr, ks, path := newKeyring(t, 15*time.Minute)
	_, kid := kr.Active()

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "private keys are not world-readable")

	reloaded, err := oidc.NewKeyring(ctx, ks, oidc.KeyringOptions{TokenTTL: 15 * time.Minute})
	require.NoError(t, err)
	_, reloadedKID := reloaded.Active()
	assert.Equal(t, kid, reloadedKID, "a restart must not invalidate every issued token")
}

func TestKeyringRefusesToStartWhenItCannotPersist(t *testing.T) {
	_, err := oidc.NewKeyring(context.Background(), unwritableKeyStore{}, oidc.KeyringOptions{TokenTTL: time.Minute})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persist keyring")

	_, err = oidc.NewKeyring(context.Background(), nil, oidc.KeyringOptions{TokenTTL: time.Minute})
	assert.Error(t, err)

	ks, err := oidc.NewFileKeyStore(filepath.Join(t.TempDir(), "k.json"))
	require.NoError(t, err)
	_, err = oidc.NewKeyring(context.Background(), ks, oidc.KeyringOptions{})
	assert.Error(t, err, "a zero TTL gives no rule for when a retired key may be dropped")
}

type unwritableKeyStore struct{}

func (unwritableKeyStore) Load(context.Context) ([]oidc.StoredKey, error) { return nil, nil }
func (unwritableKeyStore) Save(context.Context, []oidc.StoredKey) error {
	return errors.New("disk is read-only")
}

func TestCorruptKeyStoreFailsLoudly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oidc.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	ks, err := oidc.NewFileKeyStore(path)
	require.NoError(t, err)

	_, err = oidc.NewKeyring(context.Background(), ks, oidc.KeyringOptions{TokenTTL: time.Minute})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "corrupt")

	require.NoError(t, os.WriteFile(path, []byte(`[{"kid":"x","private_pem":"not pem"}]`), 0o600))
	_, err = oidc.NewKeyring(context.Background(), ks, oidc.KeyringOptions{TokenTTL: time.Minute})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid PEM")

	_, err = oidc.NewFileKeyStore("")
	assert.Error(t, err)
}

func TestBlobKeyStore(t *testing.T) {
	ctx := context.Background()
	bs, err := disk.New(t.TempDir())
	require.NoError(t, err)
	ks, err := oidc.NewBlobKeyStore(bs, "oidc/keys.json")
	require.NoError(t, err)

	kr, err := oidc.NewKeyring(ctx, ks, oidc.KeyringOptions{TokenTTL: time.Minute})
	require.NoError(t, err)
	_, kid := kr.Active()

	reloaded, err := oidc.NewKeyring(ctx, ks, oidc.KeyringOptions{TokenTTL: time.Minute})
	require.NoError(t, err)
	_, reloadedKID := reloaded.Active()
	assert.Equal(t, kid, reloadedKID)

	_, err = oidc.NewBlobKeyStore(nil, "k")
	assert.Error(t, err)
	_, err = oidc.NewBlobKeyStore(bs, "../escape")
	assert.Error(t, err)
}

func TestRunnerEnv(t *testing.T) {
	env := oidc.RunnerEnv("https://ci.example.ghe.com/", "tok")
	assert.Equal(t, "https://ci.example.ghe.com/_apis/oidc/token?api-version=2.0", env[oidc.EnvIDTokenRequestURL])
	assert.Equal(t, "tok", env[oidc.EnvIDTokenRequestToken])
	assert.Contains(t, env[oidc.EnvIDTokenRequestURL], "?",
		"getIDToken appends &audience=, so the URL must already have a query string")
}
