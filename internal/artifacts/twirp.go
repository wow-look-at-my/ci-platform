package artifacts

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The Results API is Twirp with JSON encoding. @actions/artifact posts to
// {ACTIONS_RESULTS_URL}/twirp/{service}/{method} with Content-Type
// application/json and reads {"code","msg"} out of any non-2xx body, so an
// error rendered any other way reaches the operator as a bare status code.

// ServicePath is the Twirp service name the artifact client calls.
const ServicePath = "github.actions.results.api.v1.ArtifactService"

// TwirpPrefix is the route prefix for every Results API method.
const TwirpPrefix = "/twirp/" + ServicePath + "/"

// Twirp error codes used by this service, from the Twirp spec.
const (
	CodeInvalidArgument   = "invalid_argument"
	CodeNotFound          = "not_found"
	CodeAlreadyExists     = "already_exists"
	CodeUnauthenticated   = "unauthenticated"
	CodePermissionDenied  = "permission_denied"
	CodeResourceExhausted = "resource_exhausted"
	CodeInternal          = "internal"
)

// twirpStatus maps a Twirp code to the HTTP status the spec pairs it with.
// The client retries 429/500/502/503/504 and fails fast on everything else, so
// the mapping decides whether a client waits or gives up.
func twirpStatus(code string) int {
	switch code {
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeAlreadyExists:
		return http.StatusConflict
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeResourceExhausted:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// twirpError is the body shape the client parses; it surfaces msg verbatim.
type twirpError struct {
	Code string            `json:"code"`
	Msg  string            `json:"msg"`
	Meta map[string]string `json:"meta,omitempty"`
}

func writeTwirpError(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(twirpStatus(code))
	_ = json.NewEncoder(w).Encode(twirpError{Code: code, Msg: msg})
}

func writeTwirpJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// Request and response bodies. protobuf-ts serialises with
// useProtoFieldName:true, so the client sends snake_case; its reader accepts
// both spellings, so responses use snake_case too. int64 fields are JSON
// strings, per the protobuf JSON mapping, and google.protobuf.StringValue
// wrappers serialise as a bare string rather than {"value":...}.

// CreateArtifactRequest opens an artifact and asks for somewhere to put it.
type CreateArtifactRequest struct {
	WorkflowRunBackendID    string `json:"workflow_run_backend_id"`
	WorkflowJobRunBackendID string `json:"workflow_job_run_backend_id"`
	Name                    string `json:"name"`
	// ExpiresAt is an RFC3339 timestamp derived from the action's
	// retention-days input; the service clamps it.
	ExpiresAt string `json:"expires_at,omitempty"`
	Version   int    `json:"version,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
}

// CreateArtifactResponse hands back the upload URL. The client uploads to it
// with the Azure Block Blob SDK, so it must be served by the shim in azure.go.
type CreateArtifactResponse struct {
	OK              bool   `json:"ok"`
	SignedUploadURL string `json:"signed_upload_url"`
}

// FinalizeArtifactRequest closes an artifact with its size and digest.
type FinalizeArtifactRequest struct {
	WorkflowRunBackendID    string `json:"workflow_run_backend_id"`
	WorkflowJobRunBackendID string `json:"workflow_job_run_backend_id"`
	Name                    string `json:"name"`
	// Size is an int64 on the wire, hence a string.
	Size Int64String `json:"size"`
	// Hash is "sha256:<hex>" when the client computed one.
	Hash string `json:"hash,omitempty"`
}

// FinalizeArtifactResponse reports the stored artifact's id.
type FinalizeArtifactResponse struct {
	OK         bool   `json:"ok"`
	ArtifactID string `json:"artifact_id"`
}

// ListArtifactsRequest lists a run's artifacts, optionally filtered.
type ListArtifactsRequest struct {
	WorkflowRunBackendID    string      `json:"workflow_run_backend_id"`
	WorkflowJobRunBackendID string      `json:"workflow_job_run_backend_id"`
	NameFilter              string      `json:"name_filter,omitempty"`
	IDFilter                Int64String `json:"id_filter,omitempty"`
}

// MonolithArtifact is one entry of a ListArtifacts response.
type MonolithArtifact struct {
	WorkflowRunBackendID    string `json:"workflow_run_backend_id"`
	WorkflowJobRunBackendID string `json:"workflow_job_run_backend_id"`
	DatabaseID              string `json:"database_id"`
	Name                    string `json:"name"`
	Size                    string `json:"size"`
	CreatedAt               string `json:"created_at,omitempty"`
	Digest                  string `json:"digest,omitempty"`
}

// ListArtifactsResponse is the list.
type ListArtifactsResponse struct {
	Artifacts []MonolithArtifact `json:"artifacts"`
}

// GetSignedArtifactURLRequest asks where to download an artifact from.
type GetSignedArtifactURLRequest struct {
	WorkflowRunBackendID    string `json:"workflow_run_backend_id"`
	WorkflowJobRunBackendID string `json:"workflow_job_run_backend_id"`
	Name                    string `json:"name"`
}

// GetSignedArtifactURLResponse carries the download URL. The client fetches it
// with an unauthenticated HTTP client, so the URL carries its own signature.
type GetSignedArtifactURLResponse struct {
	SignedURL string `json:"signed_url"`
}

// DeleteArtifactRequest removes an artifact.
type DeleteArtifactRequest struct {
	WorkflowRunBackendID    string `json:"workflow_run_backend_id"`
	WorkflowJobRunBackendID string `json:"workflow_job_run_backend_id"`
	Name                    string `json:"name"`
}

// DeleteArtifactResponse reports what was removed.
type DeleteArtifactResponse struct {
	OK         bool   `json:"ok"`
	ArtifactID string `json:"artifact_id"`
}

// decodeTwirp reads a JSON request body, tolerating the camelCase spelling as
// well: protobuf-ts accepts either on the way in, and so must this.
func decodeTwirp(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	raw := map[string]json.RawMessage{}
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("request body is not JSON: %w", err)
	}
	normalised := make(map[string]json.RawMessage, len(raw))
	for k, val := range raw {
		normalised[toSnake(k)] = val
	}
	b, err := json.Marshal(normalised)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("request body does not match %T: %w", v, err)
	}
	return nil
}

// toSnake converts workflowRunBackendId to workflow_run_backend_id.
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Int64String is a protobuf int64 on the wire: a JSON string. It also accepts
// a bare number, which the JSON mapping permits and a hand-written client may
// send.
type Int64String string

// UnmarshalJSON accepts "123" and 123.
func (v *Int64String) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "null" {
		s = ""
	}
	*v = Int64String(s)
	return nil
}

// String returns the raw value.
func (v Int64String) String() string { return string(v) }

// parseInt64 reads the value, treating empty as zero.
func parseInt64(v Int64String) (int64, error) {
	s := strings.TrimSpace(string(v))
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}
