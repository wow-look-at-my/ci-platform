package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SigV4 is hand-rolled rather than taken from the AWS SDK: the SDK pulls in
// ~30 modules to sign four verbs, and this platform's dependency budget is
// spent elsewhere.

const (
	algorithm  = "AWS4-HMAC-SHA256"
	terminator = "aws4_request"
	// EmptyPayloadHash is sha256 of the empty string, the payload hash for any
	// request with no body.
	EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	// UnsignedPayload tells S3 the body is not covered by the signature, which
	// is what a presigned URL and a streaming PUT both use.
	UnsignedPayload = "UNSIGNED-PAYLOAD"

	amzDateFormat = "20060102T150405Z"
	dateFormat    = "20060102"
)

type credentials struct {
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
}

// uriEncode percent-encodes per SigV4 rules. S3 canonical URIs keep "/"
// literal; query components encode it.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'),
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/':
			if encodeSlash {
				b.WriteString("%2F")
			} else {
				b.WriteByte('/')
			}
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalURI is the encoded path. An empty path is "/".
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	return uriEncode(path, false)
}

// canonicalQuery sorts and encodes the query string.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, kv{uriEncode(k, true), uriEncode(v, true)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// trimHeaderValue collapses runs of spaces, which SigV4 requires before the
// value is hashed.
func trimHeaderValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// canonicalHeaders returns the header block and the signed-header list. host
// is taken from the request rather than the header map, because Go keeps it
// out of Header.
func canonicalHeaders(r *http.Request, extra map[string]string) (string, string) {
	values := map[string]string{}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	values["host"] = trimHeaderValue(host)
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		switch lk {
		case "authorization", "user-agent", "content-length", "connection", "expect", "transfer-encoding":
			// Not signed: hop-by-hop, set by the transport, or the signature
			// itself. content-length is omitted because Go rewrites it.
			continue
		}
		joined := make([]string, len(vs))
		for i, v := range vs {
			joined[i] = trimHeaderValue(v)
		}
		values[lk] = strings.Join(joined, ",")
	}
	for k, v := range extra {
		values[strings.ToLower(k)] = trimHeaderValue(v)
	}
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(values[n])
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

// canonicalRequest builds the string whose hash the signature covers.
func canonicalRequest(method, uri, query, headers, signedHeaders, payloadHash string) string {
	return strings.Join([]string{
		method,
		uri,
		query,
		headers,
		signedHeaders,
		payloadHash,
	}, "\n")
}

func credentialScope(t time.Time, region, service string) string {
	return strings.Join([]string{t.UTC().Format(dateFormat), region, service, terminator}, "/")
}

// stringToSign is the second SigV4 intermediate; it is separated out so a
// signature mismatch can be diagnosed one stage at a time.
func stringToSign(t time.Time, scope, canonicalReq string) string {
	sum := sha256.Sum256([]byte(canonicalReq))
	return strings.Join([]string{
		algorithm,
		t.UTC().Format(amzDateFormat),
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// signingKey derives the date/region/service scoped key.
func signingKey(secret string, t time.Time, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), t.UTC().Format(dateFormat))
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, terminator)
}

func signature(secret string, t time.Time, region, service, sts string) string {
	return hex.EncodeToString(hmacSHA256(signingKey(secret, t, region, service), sts))
}

// signRequest adds the SigV4 Authorization header to r. payloadHash is the hex
// sha256 of the body, or UnsignedPayload for a streamed body.
func signRequest(r *http.Request, creds credentials, region, service, payloadHash string, t time.Time) {
	t = t.UTC()
	r.Header.Set("X-Amz-Date", t.Format(amzDateFormat))
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.sessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", creds.sessionToken)
	}
	hdrs, signed := canonicalHeaders(r, nil)
	creq := canonicalRequest(r.Method, canonicalURI(r.URL.Path), canonicalQuery(r.URL.Query()), hdrs, signed, payloadHash)
	scope := credentialScope(t, region, service)
	sig := signature(creds.secretAccessKey, t, region, service, stringToSign(t, scope, creq))
	r.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, creds.accessKeyID, scope, signed, sig))
}

// presign returns a URL carrying the signature in its query, for handing to a
// client that cannot send an Authorization header.
func presign(method string, u *url.URL, host string, creds credentials, region, service string, ttl time.Duration, t time.Time) string {
	t = t.UTC()
	scope := credentialScope(t, region, service)
	q := u.Query()
	q.Set("X-Amz-Algorithm", algorithm)
	q.Set("X-Amz-Credential", creds.accessKeyID+"/"+scope)
	q.Set("X-Amz-Date", t.Format(amzDateFormat))
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(ttl/time.Second), 10))
	q.Set("X-Amz-SignedHeaders", "host")
	if creds.sessionToken != "" {
		q.Set("X-Amz-Security-Token", creds.sessionToken)
	}
	hdrs := "host:" + trimHeaderValue(host) + "\n"
	creq := canonicalRequest(method, canonicalURI(u.Path), canonicalQuery(q), hdrs, "host", UnsignedPayload)
	sig := signature(creds.secretAccessKey, t, region, service, stringToSign(t, scope, creq))
	q.Set("X-Amz-Signature", sig)

	signed := *u
	signed.RawQuery = canonicalQuery(q)
	return signed.String()
}
