// Package sensitive protects known submitted values without retaining plaintext.
// Matching is exact; it is not a detector for arbitrary encodings or transforms.
package sensitive

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"hash"
	"strings"
	"sync"
)

const MaxValueBytes = 4096
const maxValues = 32
const prefixBytes = 8
const replacement = "[REDACTED]"

type fingerprint struct {
	length int
	digest [sha256.Size]byte
}

// Values belongs to one browser identity. It stores keyed digests and lengths,
// never submitted bytes. Its zero value refuses registration and fails closed.
type Values struct {
	mu     sync.Mutex
	key    [32]byte
	ready  bool
	closed bool
	known  map[fingerprint]struct{}
	// Short keyed prefixes avoid hashing every full candidate window. Prefixes
	// are digests too: no plaintext fragment or reversible encoding is retained.
	prefixes map[int]map[[sha256.Size]byte][]fingerprint
}

func New() *Values {
	values := &Values{known: make(map[fingerprint]struct{}), prefixes: make(map[int]map[[sha256.Size]byte][]fingerprint)}
	_, err := rand.Read(values.key[:])
	values.ready = err == nil
	return values
}

// Formatting session internals must not expose fingerprints or their private key.
func (*Values) String() string   { return "protected session values" }
func (*Values) GoString() string { return "protected session values" }

type contextKey struct{}

func WithValues(ctx context.Context, values *Values) context.Context {
	return context.WithValue(ctx, contextKey{}, values)
}

// HasValues reports ownership of a tracker, including a retired tracker whose
// registration must fail closed. It does not expose its fingerprints or values.
func HasValues(ctx context.Context) bool {
	values, _ := ctx.Value(contextKey{}).(*Values)
	return values != nil
}

// Remember registers all values atomically. The caller still owns and must clear
// the supplied bytes. Existing fingerprints are never evicted to make room.
func Remember(ctx context.Context, submitted ...[]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	values, _ := ctx.Value(contextKey{}).(*Values)
	if values == nil {
		return errors.New("credential protection requires a managed browser session")
	}
	values.mu.Lock()
	defer values.mu.Unlock()
	if !values.ready || values.closed {
		return errors.New("browser credential protection is unavailable")
	}
	if len(submitted) == 0 {
		return errors.New("credential protection requires a value")
	}
	mac := hmac.New(sha256.New, values.key[:])
	pending := make(map[fingerprint][sha256.Size]byte)
	for _, value := range submitted {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(value) == 0 || len(value) > MaxValueBytes {
			return errors.New("protected values must contain 1 to 4096 bytes")
		}
		full := fingerprint{length: len(value), digest: sum(mac, value)}
		if _, exists := values.known[full]; exists {
			continue
		}
		pending[full] = sum(mac, value[:min(len(value), prefixBytes)])
		if len(values.known)+len(pending) > maxValues {
			return errors.New("browser credential protection capacity was reached; use a new session")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for full, prefix := range pending {
		size := min(full.length, prefixBytes)
		if values.prefixes[size] == nil {
			values.prefixes[size] = make(map[[sha256.Size]byte][]fingerprint)
		}
		values.prefixes[size][prefix] = append(values.prefixes[size][prefix], full)
		values.known[full] = struct{}{}
	}
	return nil
}

// Redact removes exact registered byte sequences. A cleared tracker suppresses
// late callback text wholesale; a context without a tracker remains unchanged.
func Redact(ctx context.Context, text string) string {
	values, _ := ctx.Value(contextKey{}).(*Values)
	if values == nil || text == "" {
		return text
	}
	values.mu.Lock()
	defer values.mu.Unlock()
	if !values.ready || values.closed {
		return replacement
	}
	if len(values.known) == 0 {
		return text
	}
	mac := hmac.New(sha256.New, values.key[:])
	var output strings.Builder
	end, markerEnd := -1, 0
	for offset := 0; offset < len(text); offset++ {
		if strings.HasPrefix(text[offset:], replacement) {
			markerEnd = offset + len(replacement)
		}
		matched := 0
		for size, prefixes := range values.prefixes {
			if len(text)-offset < size {
				continue
			}
			prefix := sum(mac, []byte(text[offset:offset+size]))
			for _, candidate := range prefixes[prefix] {
				// Public replacement markers are stable, including with short
				// credentials. Still protect a value crossing a marker boundary.
				if offset < markerEnd && offset+candidate.length <= markerEnd {
					continue
				}
				if candidate.length <= matched || candidate.length > len(text)-offset {
					continue
				}
				if candidate.length == size || sum(mac, []byte(text[offset:offset+candidate.length])) == candidate.digest {
					matched = candidate.length
				}
			}
		}
		if matched == 0 {
			continue
		}
		if end < 0 {
			output.WriteString(text[:offset])
		} else if offset > end {
			output.WriteString(replacement)
			output.WriteString(text[end:offset])
		}
		end = max(end, offset+matched)
	}
	if end < 0 {
		return text
	}
	output.WriteString(replacement)
	output.WriteString(text[end:])
	return output.String()
}

func sum(mac hash.Hash, value []byte) (digest [sha256.Size]byte) {
	mac.Reset()
	_, _ = mac.Write(value)
	mac.Sum(digest[:0])
	return digest
}

// Clear retires this tracker. Closed identities are never made usable again.
func (values *Values) Clear() {
	if values == nil {
		return
	}
	values.mu.Lock()
	defer values.mu.Unlock()
	clear(values.key[:])
	clear(values.known)
	clear(values.prefixes)
	values.ready, values.closed = false, true
}
