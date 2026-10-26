package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/billinghttp"
	"github.com/cn-maul/rosetta-gateway/internal/ingress"
)

func TestR9_OrganizationTimeParametersRejectExplicitInvalidValues(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	_, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, ingress.NewOpenAIChat())

	paths := []string{
		"/v1/organization/costs",
		"/v1/organization/usage/completions",
	}
	queries := []string{
		"start_time=abc",
		"start_time=0",
		"start_time=-1",
		"end_time=abc",
		"end_time=0",
		"end_time=-1",
	}
	for _, path := range paths {
		for _, q := range queries {
			rec := httptest.NewRecorder()
			req := authedGet(path + "?" + q)
			if path == "/v1/organization/costs" {
				billinghttp.OrgCosts(db)(rec, req)
			} else {
				billinghttp.OrgUsageCompletions(db)(rec, req)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s?%s status=%d body=%s, want 400", path, q, rec.Code, rec.Body.String())
			}
		}
	}

	// Absent parameters retain the documented 30-day fallback.
	rec := httptest.NewRecorder()
	billinghttp.OrgCosts(db)(rec, authedGet("/v1/organization/costs"))
	if rec.Code != http.StatusOK {
		t.Fatalf("absent time parameters status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}
