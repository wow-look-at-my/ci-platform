// Package azureshim serves the subset of the Azure Block Blob REST API that
// the Azure Storage SDK uses when it uploads, backed by a blob.Store.
//
// It exists because @actions/artifact hands CreateArtifact's signed_upload_url
// to BlockBlobClient.uploadStream, which always stages blocks and then commits
// a block list. Serving artifact uploads therefore means speaking Put Block and
// Put Block List, whatever the bytes actually land on. Nothing here talks to
// Azure; only the wire shape is shared.
//
// Implemented: PUT ?comp=block&blockid=, PUT ?comp=blocklist, and the
// single-shot PUT with x-ms-blob-type: BlockBlob.
package azureshim

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
)

// Azure headers the SDK sends or reads.
const (
	HeaderBlobType        = "x-ms-blob-type"
	HeaderBlobContentType = "x-ms-blob-content-type"
	HeaderVersion         = "x-ms-version"
	HeaderRequestID       = "x-ms-request-id"
	HeaderErrorCode       = "x-ms-error-code"
)

// APIVersion is echoed back on every response. The SDK does not require a
// particular value, but it does log it, and an empty one reads as a broken
// endpoint during an incident.
const APIVersion = "2021-08-06"

// Error is a failure rendered in Azure's shape so the SDK surfaces something
// useful rather than "unexpected status".
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Errorf builds an Error.
func Errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Target is where a request's bytes belong.
type Target struct {
	// Key is the blob.Store key the committed object lands at.
	Key string
	// Ref is an opaque caller-side identity (an artifact id, a cache id) passed
	// back to OnCommit.
	Ref string
}

// Options configures a Handler.
type Options struct {
	// Store receives the bytes.
	Store blob.Store
	// Resolve authenticates the request and says where it writes. Returning an
	// Error rejects the upload in Azure's error shape.
	Resolve func(*http.Request) (Target, *Error)
	// OnCommit runs after the object is assembled, with its final size and hex
	// sha256. An error here fails the upload: the client must not believe an
	// artifact landed when the record of it did not.
	OnCommit func(ctx context.Context, t Target, size int64, digest, contentType string) error
	// MaxBlocks bounds how many blocks one upload may stage.
	MaxBlocks int
}

// DefaultMaxBlocks is Azure's own per-blob block limit.
const DefaultMaxBlocks = 50000

// Handler serves the block-blob subset.
type Handler struct {
	store     blob.Store
	resolve   func(*http.Request) (Target, *Error)
	onCommit  func(ctx context.Context, t Target, size int64, digest, contentType string) error
	maxBlocks int
}

// New validates opts.
func New(opts Options) (*Handler, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("azureshim: a blob store is required")
	case opts.Resolve == nil:
		return nil, errors.New("azureshim: a Resolve function is required; the endpoint must authenticate every upload")
	case opts.OnCommit == nil:
		return nil, errors.New("azureshim: an OnCommit function is required; an upload nobody records is an upload that silently vanishes")
	}
	h := &Handler{store: opts.Store, resolve: opts.Resolve, onCommit: opts.OnCommit, maxBlocks: opts.MaxBlocks}
	if h.maxBlocks <= 0 {
		h.maxBlocks = DefaultMaxBlocks
	}
	return h, nil
}

// blockKey stages one block. The block id is hex-encoded because Azure block
// ids are arbitrary base64 and would otherwise not be a safe path element.
func blockKey(objectKey, blockID string) string {
	return objectKey + ".blocks/" + hex.EncodeToString([]byte(blockID))
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(HeaderVersion, APIVersion)
	w.Header().Set(HeaderRequestID, requestID(r))

	if r.Method != http.MethodPut {
		writeAzureError(w, Errorf(http.StatusMethodNotAllowed, "UnsupportedHttpVerb",
			"this endpoint accepts PUT only, got %s", r.Method))
		return
	}
	target, aerr := h.resolve(r)
	if aerr != nil {
		writeAzureError(w, aerr)
		return
	}
	if err := blob.ValidateKey(target.Key); err != nil {
		writeAzureError(w, Errorf(http.StatusBadRequest, "InvalidUri", "%v", err))
		return
	}

	switch r.URL.Query().Get("comp") {
	case "block":
		h.putBlock(w, r, target)
	case "blocklist":
		h.putBlockList(w, r, target)
	case "":
		h.putBlob(w, r, target)
	default:
		writeAzureError(w, Errorf(http.StatusBadRequest, "InvalidQueryParameterValue",
			"comp=%s is not implemented by this endpoint", r.URL.Query().Get("comp")))
	}
}

// putBlock stages one block.
func (h *Handler) putBlock(w http.ResponseWriter, r *http.Request, t Target) {
	blockID := r.URL.Query().Get("blockid")
	if blockID == "" {
		writeAzureError(w, Errorf(http.StatusBadRequest, "MissingRequiredQueryParameter",
			"comp=block requires a blockid"))
		return
	}
	defer r.Body.Close()
	if _, _, err := h.store.Put(r.Context(), blockKey(t.Key, blockID), r.Body); err != nil {
		writeAzureError(w, Errorf(http.StatusInternalServerError, "InternalError",
			"staging block for %s failed: %v", t.Key, err))
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// blockList is the body of a Put Block List request.
type blockList struct {
	XMLName     xml.Name `xml:"BlockList"`
	Committed   []string `xml:"Committed"`
	Uncommitted []string `xml:"Uncommitted"`
	Latest      []string `xml:"Latest"`
}

// ordered returns the block ids in the order they appear in the document.
// Azure orders the committed blob by document order across all three element
// types, and the SDK writes Latest entries.
func orderedBlockIDs(body []byte) ([]string, error) {
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	var ids []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "Latest", "Committed", "Uncommitted":
			var id string
			if err := dec.DecodeElement(&id, &start); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// putBlockList assembles the staged blocks into the final object.
func (h *Handler) putBlockList(w http.ResponseWriter, r *http.Request, t Target) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeAzureError(w, Errorf(http.StatusBadRequest, "InvalidXmlDocument", "reading the block list failed: %v", err))
		return
	}
	ids, err := orderedBlockIDs(body)
	if err != nil {
		writeAzureError(w, Errorf(http.StatusBadRequest, "InvalidXmlDocument", "the block list is not valid XML: %v", err))
		return
	}
	if len(ids) == 0 {
		writeAzureError(w, Errorf(http.StatusBadRequest, "InvalidBlockList",
			"the block list named no blocks, so there is nothing to commit"))
		return
	}
	if len(ids) > h.maxBlocks {
		writeAzureError(w, Errorf(http.StatusBadRequest, "BlockCountExceedsLimit",
			"the block list names %d blocks, over the %d block limit", len(ids), h.maxBlocks))
		return
	}

	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = blockKey(t.Key, id)
		if _, err := h.store.Stat(r.Context(), keys[i]); err != nil {
			// A block list naming a block that was never staged means the
			// upload lost data; assembling the rest would produce a corrupt
			// artifact that looks fine.
			writeAzureError(w, Errorf(http.StatusBadRequest, "InvalidBlockList",
				"block %q was named in the block list but never staged: %v", id, err))
			return
		}
	}

	src := blob.Concat(r.Context(), h.store, keys)
	size, digest, putErr := h.store.Put(r.Context(), t.Key, src)
	closeErr := src.Close()
	if putErr != nil {
		writeAzureError(w, Errorf(http.StatusInternalServerError, "InternalError",
			"committing %s failed: %v", t.Key, putErr))
		return
	}
	if closeErr != nil {
		writeAzureError(w, Errorf(http.StatusInternalServerError, "InternalError",
			"committing %s failed: %v", t.Key, closeErr))
		return
	}
	for _, k := range keys {
		_ = h.store.Delete(r.Context(), k)
	}

	if err := h.onCommit(r.Context(), t, size, digest, r.Header.Get(HeaderBlobContentType)); err != nil {
		writeAzureError(w, Errorf(http.StatusInternalServerError, "InternalError",
			"recording %s failed: %v", t.Key, err))
		return
	}
	writeCommitted(w, digest)
}

// putBlob handles the single-shot upload path.
func (h *Handler) putBlob(w http.ResponseWriter, r *http.Request, t Target) {
	if bt := r.Header.Get(HeaderBlobType); !strings.EqualFold(bt, "BlockBlob") {
		writeAzureError(w, Errorf(http.StatusBadRequest, "UnsupportedHeader",
			"only BlockBlob uploads are supported, got %s=%q", HeaderBlobType, bt))
		return
	}
	defer r.Body.Close()
	size, digest, err := h.store.Put(r.Context(), t.Key, r.Body)
	if err != nil {
		writeAzureError(w, Errorf(http.StatusInternalServerError, "InternalError",
			"writing %s failed: %v", t.Key, err))
		return
	}
	if err := h.onCommit(r.Context(), t, size, digest, r.Header.Get(HeaderBlobContentType)); err != nil {
		writeAzureError(w, Errorf(http.StatusInternalServerError, "InternalError",
			"recording %s failed: %v", t.Key, err))
		return
	}
	writeCommitted(w, digest)
}

func writeCommitted(w http.ResponseWriter, digest string) {
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusCreated)
}

type azureErrorBody struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func writeAzureError(w http.ResponseWriter, e *Error) {
	w.Header().Set(HeaderErrorCode, e.Code)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(e.Status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(azureErrorBody{Code: e.Code, Message: e.Message})
}

func requestID(r *http.Request) string {
	if id := r.Header.Get("x-ms-client-request-id"); id != "" {
		return id
	}
	return "ci-platform"
}
