// Package ulid mints monotonic, time-sortable ULIDs (DESIGN §6.1.1). IDs are
// Flowbee-minted; this package lives OUTSIDE the deterministic core (the core
// receives IDs, it never mints them — enforced by tools/archcheck).
package ulid

import (
	"crypto/rand"
	"io"
	"sync"
	"time"

	oklid "github.com/oklog/ulid/v2"
)

// Minter mints monotonic ULIDs. Safe for concurrent use.
type Minter struct {
	mu      sync.Mutex
	entropy io.Reader
}

// NewMinter builds a monotonic minter. A nil seed uses crypto/rand.
func NewMinter(seed io.Reader) *Minter {
	if seed == nil {
		seed = rand.Reader
	}
	return &Minter{entropy: oklid.Monotonic(seed, 0)}
}

func (m *Minter) New() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return oklid.MustNew(oklid.Timestamp(time.Now()), m.entropy).String()
}

var defaultMinter = NewMinter(nil)

// New mints a ULID with the package-level monotonic minter.
func New() string { return defaultMinter.New() }
