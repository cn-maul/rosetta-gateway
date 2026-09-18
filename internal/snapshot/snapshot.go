package snapshot

import (
	"sync/atomic"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
)

type Snapshot struct {
	Routes   *routing.RouteIndex
	Providers map[string]*ProviderSnapshot
	Keys     map[string]*KeySnapshot
}

type ProviderSnapshot struct {
	ID       string
	Slug     string
	Name     string
	Endpoint string
	Enabled  bool
}

type KeySnapshot struct {
	ID           string
	KeyHash      string
	Name         string
	Enabled      bool
	QuotaTokens  int64
	UsedTokens   int64
	RPMLimit     int
	TPMLimit     int
}

var current atomic.Pointer[Snapshot]

func Init(s *Snapshot) {
	current.Store(s)
}

func Get() *Snapshot {
	s := current.Load()
	if s == nil {
		return &Snapshot{
			Routes:    routing.NewRouteIndex(),
			Providers: make(map[string]*ProviderSnapshot),
			Keys:      make(map[string]*KeySnapshot),
		}
	}
	return s
}

func Swap(s *Snapshot) {
	current.Store(s)
}
