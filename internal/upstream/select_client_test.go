package upstream

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
)

func multiCredentialPool(t *testing.T) (*Pool, [32]byte) {
	t.Helper()
	cfg := newTestConfig()
	cfg.Bootstrap.Providers[0].Credentials = []config.BootstrapCredential{
		{Label: "first", APIKey: "sk-first"},
		{Label: "second", APIKey: "sk-second"},
	}
	p := newTestPool()
	if err := p.BuildFromConfig(cfg); err != nil {
		t.Fatalf("build pool: %v", err)
	}
	return p, sha256.Sum256([]byte("sk-first"))
}

func TestSelectClientPinsCredentialAndVersion(t *testing.T) {
	p, version := multiCredentialPool(t)
	view := p.ClientView()

	for i := 0; i < 20; i++ {
		client, credentialID, gotVersion, err := view.SelectClient("testprov", "", "testprov-first", version)
		if err != nil {
			t.Fatalf("pinned select: %v", err)
		}
		if client == nil || credentialID != "testprov-first" || gotVersion != version {
			t.Fatalf("pinned select returned %q version=%x", credentialID, gotVersion)
		}
	}
}

func TestSelectClientRejectsPinnedCredentialAfterCooldown(t *testing.T) {
	p, version := multiCredentialPool(t)
	p.MarkCredentialCooldown("testprov-first", time.Minute)

	_, _, _, err := p.ClientView().SelectClient("testprov", "", "testprov-first", version)
	if err == nil || !strings.Contains(err.Error(), "original upstream credential") {
		t.Fatalf("a cooled pinned credential must not fall back to another key, got %v", err)
	}
}

func TestSelectClientRejectsPinnedCredentialVersionChange(t *testing.T) {
	p, _ := multiCredentialPool(t)
	wrong := sha256.Sum256([]byte("rotated-key"))

	_, _, _, err := p.ClientView().SelectClient("testprov", "", "testprov-first", wrong)
	if err == nil {
		t.Fatalf("pinned select must reject a credential whose key changed")
	}
}
