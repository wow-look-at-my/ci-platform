package s3

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Vectors are the published AWS "Signature Version 4" examples. The canonical
// request and string-to-sign are asserted verbatim so a mismatch names the
// stage that broke rather than just "signature wrong".

var exampleCreds = credentials{
	accessKeyID:     "AKIAIOSFODNN7EXAMPLE",
	secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(amzDateFormat, s)
	require.NoError(t, err)
	return ts
}

// TestSigV4GetObject is the "GET Object" example from the S3 signing docs.
func TestSigV4GetObject(t *testing.T) {
	ts := mustTime(t, "20130524T000000Z")
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	require.NoError(t, err)
	req.Header.Set("Range", "bytes=0-9")

	hdrs, signed := canonicalHeaders(req, map[string]string{
		"x-amz-content-sha256": EmptyPayloadHash,
		"x-amz-date":           "20130524T000000Z",
	})
	assert.Equal(t, "host;range;x-amz-content-sha256;x-amz-date", signed)

	creq := canonicalRequest(req.Method, canonicalURI(req.URL.Path), canonicalQuery(req.URL.Query()), hdrs, signed, EmptyPayloadHash)
	assert.Equal(t, strings.Join([]string{
		"GET",
		"/test.txt",
		"",
		"host:examplebucket.s3.amazonaws.com",
		"range:bytes=0-9",
		"x-amz-content-sha256:" + EmptyPayloadHash,
		"x-amz-date:20130524T000000Z",
		"",
		"host;range;x-amz-content-sha256;x-amz-date",
		EmptyPayloadHash,
	}, "\n"), creq)

	scope := credentialScope(ts, "us-east-1", "s3")
	assert.Equal(t, "20130524/us-east-1/s3/aws4_request", scope)

	sts := stringToSign(ts, scope, creq)
	assert.Equal(t, strings.Join([]string{
		"AWS4-HMAC-SHA256",
		"20130524T000000Z",
		"20130524/us-east-1/s3/aws4_request",
		"7344ae5b7ee6c3e7e6b0fe0640412a37625d1fbfff95c48bbb2dc43964946972",
	}, "\n"), sts)

	assert.Equal(t,
		"f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		signature(exampleCreds.secretAccessKey, ts, "us-east-1", "s3", sts))
}

// TestSigV4PresignedURL is the "Query String Request Authentication" example.
func TestSigV4PresignedURL(t *testing.T) {
	ts := mustTime(t, "20130524T000000Z")
	u, err := url.Parse("https://examplebucket.s3.amazonaws.com/test.txt")
	require.NoError(t, err)

	got := presign(http.MethodGet, u, "examplebucket.s3.amazonaws.com", exampleCreds, "us-east-1", "s3", 86400*time.Second, ts)
	parsed, err := url.Parse(got)
	require.NoError(t, err)
	q := parsed.Query()

	assert.Equal(t, "AWS4-HMAC-SHA256", q.Get("X-Amz-Algorithm"))
	assert.Equal(t, "AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request", q.Get("X-Amz-Credential"))
	assert.Equal(t, "86400", q.Get("X-Amz-Expires"))
	assert.Equal(t, "host", q.Get("X-Amz-SignedHeaders"))
	assert.Equal(t,
		"aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404",
		q.Get("X-Amz-Signature"))
}

// TestSigV4PutObject is the "PUT Object" example: a signed body hash and two
// extra x-amz headers.
func TestSigV4PutObject(t *testing.T) {
	ts := mustTime(t, "20130524T000000Z")
	const bodyHash = "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072"
	req, err := http.NewRequest(http.MethodPut, "https://examplebucket.s3.amazonaws.com/test%24file.text", nil)
	require.NoError(t, err)
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("x-amz-storage-class", "REDUCED_REDUNDANCY")

	hdrs, signed := canonicalHeaders(req, map[string]string{
		"x-amz-content-sha256": bodyHash,
		"x-amz-date":           "20130524T000000Z",
	})
	creq := canonicalRequest(req.Method, canonicalURI(req.URL.Path), canonicalQuery(req.URL.Query()), hdrs, signed, bodyHash)
	sts := stringToSign(ts, credentialScope(ts, "us-east-1", "s3"), creq)

	assert.Equal(t, "date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class", signed)
	assert.Equal(t,
		"98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		signature(exampleCreds.secretAccessKey, ts, "us-east-1", "s3", sts))
}

// TestSigV4WrongSecretFailsLoudly proves the vectors are actually discriminating.
func TestSigV4WrongSecretFailsLoudly(t *testing.T) {
	ts := mustTime(t, "20130524T000000Z")
	sts := stringToSign(ts, credentialScope(ts, "us-east-1", "s3"), "canonical")
	good := signature(exampleCreds.secretAccessKey, ts, "us-east-1", "s3", sts)
	bad := signature(exampleCreds.secretAccessKey+"x", ts, "us-east-1", "s3", sts)
	assert.NotEqual(t, good, bad)
}

func TestURIEncode(t *testing.T) {
	assert.Equal(t, "/a/b%20c", uriEncode("/a/b c", false))
	assert.Equal(t, "%2Fa%2Fb", uriEncode("/a/b", true))
	assert.Equal(t, "a-_.~z", uriEncode("a-_.~z", false))
	assert.Equal(t, "%2B%3D%26", uriEncode("+=&", true))
	assert.Equal(t, "/", canonicalURI(""))
}

func TestCanonicalQuerySortsByEncodedName(t *testing.T) {
	q := url.Values{"b": {"2"}, "a": {"1", "0"}, "x y": {"z w"}}
	assert.Equal(t, "a=0&a=1&b=2&x%20y=z%20w", canonicalQuery(q))
	assert.Equal(t, "", canonicalQuery(nil))
}

func TestSignRequestSetsAuthorization(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://b.example.com/k", nil)
	require.NoError(t, err)
	signRequest(req, credentials{accessKeyID: "AK", secretAccessKey: "SK", sessionToken: "TOK"}, "us-east-1", "s3", EmptyPayloadHash, mustTime(t, "20130524T000000Z"))

	assert.Equal(t, "TOK", req.Header.Get("X-Amz-Security-Token"))
	assert.Equal(t, "20130524T000000Z", req.Header.Get("X-Amz-Date"))
	auth := req.Header.Get("Authorization")
	assert.Contains(t, auth, "AWS4-HMAC-SHA256 Credential=AK/20130524/us-east-1/s3/aws4_request")
	assert.Contains(t, auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token")
	assert.Contains(t, auth, "Signature=")
}

func TestTrimHeaderValueCollapsesSpaces(t *testing.T) {
	assert.Equal(t, "a b c", trimHeaderValue("  a   b  c  "))
}
