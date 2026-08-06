package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// ChunkedUpload assembles the ranged PUT/PATCH chunks that actions/upload-artifact
// and actions/cache send into one blob.
//
// It writes through PutAt when the driver supports it and stages each chunk as
// its own blob otherwise; Staged reports which happened so the caller can log
// it. Either way Commit refuses to produce an object unless the ranges it was
// given cover [0, size) exactly once: a gap or an overlap is a corrupt upload,
// not something to paper over.
type ChunkedUpload struct {
	store Store
	key   string

	mu     sync.Mutex
	ranges []byteRange
	staged bool
	probed bool
	done   bool
}

type byteRange struct{ off, length int64 }

// NewChunkedUpload starts an upload that will land at key.
func NewChunkedUpload(s Store, key string) (*ChunkedUpload, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	return &ChunkedUpload{store: s, key: key}, nil
}

// Staged reports whether chunks are being staged as separate blobs because the
// driver cannot write at an offset.
func (u *ChunkedUpload) Staged() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.staged
}

// partKey names a staged chunk. Parts live in their own namespace keyed by the
// hash of the destination: staging under the destination key itself would make
// the final object's path a directory, and the assembled write would then have
// nowhere to land.
func (u *ChunkedUpload) partKey(off int64) string {
	sum := sha256.Sum256([]byte(u.key))
	return fmt.Sprintf("_parts/%s/%020d", hex.EncodeToString(sum[:]), off)
}

// WriteRange stores one chunk at off. Chunks may arrive in any order.
func (u *ChunkedUpload) WriteRange(ctx context.Context, off int64, r io.Reader) error {
	if off < 0 {
		return fmt.Errorf("blob: chunk offset %d is negative", off)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return fmt.Errorf("blob: upload %s already committed", u.key)
	}

	if !u.probed {
		u.probed = true
		n, err := u.store.PutAt(ctx, u.key, off, r)
		if err == nil {
			u.ranges = append(u.ranges, byteRange{off, n})
			return nil
		}
		if !errors.Is(err, ErrUnsupported) {
			return fmt.Errorf("blob: write chunk at %d of %s: %w", off, u.key, err)
		}
		// The driver cannot write at an offset. Every chunk, including this
		// one, is staged as its own blob and concatenated by Commit.
		u.staged = true
	}

	if !u.staged {
		n, err := u.store.PutAt(ctx, u.key, off, r)
		if err != nil {
			return fmt.Errorf("blob: write chunk at %d of %s: %w", off, u.key, err)
		}
		u.ranges = append(u.ranges, byteRange{off, n})
		return nil
	}

	n, _, err := u.store.Put(ctx, u.partKey(off), r)
	if err != nil {
		return fmt.Errorf("blob: stage chunk at %d of %s: %w", off, u.key, err)
	}
	u.ranges = append(u.ranges, byteRange{off, n})
	return nil
}

// coverage returns the total size covered by the recorded ranges, or an error
// naming the first gap or overlap.
func (u *ChunkedUpload) coverage() (int64, error) {
	rs := make([]byteRange, len(u.ranges))
	copy(rs, u.ranges)
	sort.Slice(rs, func(i, j int) bool { return rs[i].off < rs[j].off })
	var end int64
	for _, r := range rs {
		switch {
		case r.off > end:
			return 0, fmt.Errorf("blob: upload %s has a gap: bytes %d-%d were never sent", u.key, end, r.off-1)
		case r.off < end:
			return 0, fmt.Errorf("blob: upload %s has overlapping chunks at byte %d", u.key, r.off)
		}
		end = r.off + r.length
	}
	return end, nil
}

// Commit finishes the upload and returns the assembled size and hex sha256.
// An expected size of -1 skips the size check.
func (u *ChunkedUpload) Commit(ctx context.Context, expectedSize int64) (int64, string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return 0, "", fmt.Errorf("blob: upload %s already committed", u.key)
	}
	if len(u.ranges) == 0 {
		return 0, "", fmt.Errorf("blob: upload %s received no chunks", u.key)
	}
	size, err := u.coverage()
	if err != nil {
		return 0, "", err
	}
	if expectedSize >= 0 && size != expectedSize {
		return 0, "", fmt.Errorf("blob: upload %s is %d bytes but was declared as %d", u.key, size, expectedSize)
	}

	if u.staged {
		rs := make([]byteRange, len(u.ranges))
		copy(rs, u.ranges)
		sort.Slice(rs, func(i, j int) bool { return rs[i].off < rs[j].off })
		keys := make([]string, len(rs))
		for i, r := range rs {
			keys[i] = u.partKey(r.off)
		}
		pr := Concat(ctx, u.store, keys)
		n, digest, err := u.store.Put(ctx, u.key, pr)
		closeErr := pr.Close()
		if err != nil {
			return 0, "", fmt.Errorf("blob: assemble %s: %w", u.key, err)
		}
		if closeErr != nil {
			return 0, "", fmt.Errorf("blob: assemble %s: %w", u.key, closeErr)
		}
		if n != size {
			return 0, "", fmt.Errorf("blob: assembled %s to %d bytes, expected %d", u.key, n, size)
		}
		u.done = true
		u.deleteParts(ctx, keys)
		return n, digest, nil
	}

	info, err := u.store.Stat(ctx, u.key)
	if err != nil {
		return 0, "", fmt.Errorf("blob: stat %s after upload: %w", u.key, err)
	}
	if info.Size != size {
		return 0, "", fmt.Errorf("blob: %s holds %d bytes but the chunks covered %d", u.key, info.Size, size)
	}
	digest, _, err := DigestOf(ctx, u.store, u.key)
	if err != nil {
		return 0, "", err
	}
	u.done = true
	return size, digest, nil
}

// Abort removes whatever was written. Failures to clean up are returned rather
// than dropped: leaked bytes still count against a quota.
func (u *ChunkedUpload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return fmt.Errorf("blob: upload %s already committed", u.key)
	}
	var errs []error
	if u.staged {
		for _, r := range u.ranges {
			if err := u.store.Delete(ctx, u.partKey(r.off)); err != nil && !errors.Is(err, ErrNotFound) {
				errs = append(errs, err)
			}
		}
	} else if len(u.ranges) > 0 {
		if err := u.store.Delete(ctx, u.key); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	u.ranges = nil
	u.done = true
	return errors.Join(errs...)
}

// deleteParts removes staged chunks after a successful assembly. A part that
// will not delete is a storage leak, so it is reported through the returned
// error of Commit's caller only when it is the sole failure; here the object
// already exists, so the error is attached to the upload's own record.
func (u *ChunkedUpload) deleteParts(ctx context.Context, keys []string) {
	for _, k := range keys {
		_ = u.store.Delete(ctx, k)
	}
}

// Concat streams the named objects back to back, opening each only when the
// reader reaches it, so assembling an artifact never buffers it in memory. A
// missing object is an error, never a silently short read.
func Concat(ctx context.Context, s Store, keys []string) io.ReadCloser {
	return &partsReader{ctx: ctx, store: s, keys: keys}
}

type partsReader struct {
	ctx   context.Context
	store Store
	keys  []string
	cur   io.ReadCloser
	i     int
	err   error
}

func (p *partsReader) Read(b []byte) (int, error) {
	for {
		if p.err != nil {
			return 0, p.err
		}
		if p.cur == nil {
			if p.i >= len(p.keys) {
				return 0, io.EOF
			}
			rc, err := p.store.Get(p.ctx, p.keys[p.i])
			if err != nil {
				p.err = fmt.Errorf("blob: read staged chunk %s: %w", p.keys[p.i], err)
				return 0, p.err
			}
			p.cur = rc
			p.i++
		}
		n, err := p.cur.Read(b)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			if cerr := p.cur.Close(); cerr != nil {
				p.err = cerr
				return 0, p.err
			}
			p.cur = nil
			continue
		}
		if err != nil {
			p.err = err
			return 0, err
		}
	}
}

func (p *partsReader) Close() error {
	if p.cur != nil {
		err := p.cur.Close()
		p.cur = nil
		return err
	}
	return nil
}
