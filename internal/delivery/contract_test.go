package delivery

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const (
	repoRoot = "../.."
)

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

func requireContains(t *testing.T, name, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s must contain %q", name, want)
		}
	}
}

func requireNotContains(t *testing.T, name, text string, unwanted ...string) {
	t.Helper()
	for _, item := range unwanted {
		if strings.Contains(text, item) {
			t.Errorf("%s must not contain %q", name, item)
		}
	}
}

func TestR12M0001CIUsesRepositoryHygieneScript(t *testing.T) {
	ci := readRepoFile(t, ".github/workflows/ci.yml")
	attrs := readRepoFile(t, ".gitattributes")

	requireContains(t, ".github/workflows/ci.yml", ci,
		"pwsh -NoProfile -File scripts/check-repo-hygiene.ps1")
	requireContains(t, ".gitattributes", attrs,
		".gitignore text eol=lf",
		".dockerignore text eol=lf",
		".zcodeignore text eol=lf")
}

func TestR12M0002ReleaseRunsGeneratedContractAndModuleChecks(t *testing.T) {
	docker := readRepoFile(t, ".github/workflows/docker.yml")
	requireContains(t, ".github/workflows/docker.yml", docker,
		"go generate ./internal/config",
		"web/src/generated/defaults.ts",
		"go generate ./cmd/gateway",
		"web/src/generated/routes.ts",
		"go generate ./internal/admin",
		"web/src/generated/dto-contract.ts",
		"go mod tidy -diff",
		"go mod verify")
}

func TestR12M0003ImageMetadataMatchesTrackedLicense(t *testing.T) {
	docker := readRepoFile(t, ".github/workflows/docker.yml")
	requireNotContains(t, ".github/workflows/docker.yml", docker,
		"org.opencontainers.image.licenses=MIT")
}

func TestR12M1801ManualDispatchCannotPublishVersionTag(t *testing.T) {
	docker := readRepoFile(t, ".github/workflows/docker.yml")
	requireContains(t, ".github/workflows/docker.yml", docker,
		"type=raw,value=${{ steps.ver.outputs.version }},enable=${{ github.ref_type == 'tag' }}",
		"type=raw,value=latest,enable=${{ github.ref_type == 'tag' }}",
		"type=raw,value=edge,enable=${{ github.ref_type != 'tag' }}")
}

func TestR12M1802LauncherProbesHealthz(t *testing.T) {
	gateway := readRepoFile(t, "gateway.ps1")
	requireContains(t, "gateway.ps1", gateway,
		"http://127.0.0.1:$Port/healthz")
	requireNotContains(t, "gateway.ps1", gateway,
		"Invoke-WebRequest -Uri \"http://127.0.0.1:$Port/admin/\"")
}

func TestR12M1803LauncherOnlyStopsVerifiedProcesses(t *testing.T) {
	gateway := readRepoFile(t, "gateway.ps1")
	requireContains(t, "gateway.ps1", gateway,
		"RosettaGatewayLauncher.ProcessOwnership",
		"Join-Path $Root 'gateway-launcher.ps1'")

	launcher := readRepoFile(t, "gateway.ps1") + "\n" +
		readRepoFile(t, "gateway-launcher.ps1")
	requireContains(t, "launcher sources", launcher,
		"Test-GatewayProcess",
		"Test-GatewayWrapper",
		"Get-ProcessIdentity")
	requireNotContains(t, "launcher sources", launcher,
		"Stop-Process -Id $lp -Force",
		"Stop-Process -Id ([int]$oldPid) -Force",
		"Stop-Process -Id $pidToStop -Force")

	// A PID-file wrapper is only owned by this launcher when its command line
	// names the project's gateway binary. Unrelated processes must be reported,
	// not terminated.
	requireContains(t, "launcher sources", launcher,
		"Get-CimInstance Win32_Process -Filter \"ProcessId = $ProcessId\"",
		"CommandLine")

	if runtime.GOOS != "windows" {
		t.Skip("the launcher identity helper is a Windows PowerShell contract")
	}
	helperTest := filepath.Join(repoRoot, "scripts", "gateway-launcher-contract.ps1")
	out, err := exec.Command(
		"pwsh", "-NoProfile", "-File", helperTest,
		"-LauncherPath", filepath.Join(repoRoot, "gateway-launcher.ps1"),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("launcher identity contract: %v\n%s", err, out)
	}
	var got struct {
		ExactBinary   bool `json:"exactBinary"`
		DifferentPath bool `json:"differentPath"`
		Wrapper       bool `json:"wrapper"`
		Unrelated     bool `json:"unrelated"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode launcher contract output: %v\n%s", err, out)
	}
	if !got.ExactBinary || got.DifferentPath || !got.Wrapper || got.Unrelated {
		t.Fatalf("launcher ownership decisions = %+v", got)
	}
}

func TestR12M1804ComposePersistsWholeStateDirectory(t *testing.T) {
	docker := readRepoFile(t, "DOCKER.md")
	compose := composeBlock(t, docker)
	requireContains(t, "DOCKER.md compose example", compose,
		"- ./rosetta:/data")
	requireNotContains(t, "DOCKER.md compose example", compose,
		"./rosetta/master.key:/data/master.key",
		"./rosetta/session_secret:/data/session_secret")
}

func composeBlock(t *testing.T, docker string) string {
	t.Helper()
	const start = "## docker compose"
	i := strings.Index(docker, start)
	if i < 0 {
		t.Fatal("DOCKER.md is missing the docker compose section")
	}
	rest := docker[i+len(start):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestR12M1901SlowTestsHonorShortMode(t *testing.T) {
	perf := readRepoFile(t, "cmd/gateway/perf_test.go")
	stress := readRepoFile(t, "internal/store/stress_billing_test.go")

	for _, name := range []string{
		"TestPerfDataPlaneNotLimitedByAuthConcurrencyCap",
		"TestPerfGoroutineLeak",
		"TestPerfGoroutineLeakStream",
		"TestPerfUpstreamConnectionRelease",
		"TestPerfClientDisconnectReleasesResources",
		"TestPerfMemoryGrowth",
		"TestPerfTTFTWatchdogOnHangingUpstream",
		"TestPerfUsageQueueSaturation",
	} {
		requireShortGuard(t, "cmd/gateway/perf_test.go", perf, name)
	}
	for _, name := range []string{
		"TestStress_RemainderAccumulationIsExact",
		"TestStress_TinyAmountsAreNotLost",
		"TestStress_FullLifecycle_ChargeMatchesFrozenCostExactly",
	} {
		requireShortGuard(t, "internal/store/stress_billing_test.go", stress, name)
	}
}

func requireShortGuard(t *testing.T, file, source, testName string) {
	t.Helper()
	re := regexp.MustCompile(`(?s)func ` + regexp.QuoteMeta(testName) + `\(t \*testing\.T\) \{.*?\n\}`)
	match := re.FindString(source)
	if match == "" {
		t.Fatalf("%s: could not find %s", file, testName)
	}
	if !strings.Contains(match, "testing.Short()") {
		t.Errorf("%s: %s must skip its load/timing workload under -short", file, testName)
	}
}

func TestR12M2004DesignPointsAtLocalRaceSetup(t *testing.T) {
	design := readRepoFile(t, "DESIGN.md")
	requireContains(t, "DESIGN.md", design,
		"CONTRIBUTING.md")
	requireNotContains(t, "DESIGN.md", design,
		"`-race` 本机不可用 | Windows 开发机无 gcc")
}

func TestR12M2001BudgetContractStillPassesR12(t *testing.T) {
	docker := readRepoFile(t, "DOCKER.md")
	requireContains(t, "DOCKER.md", docker,
		"正常关停的名义预算是 26s",
		"上界为 30s",
		"stop_grace_period: 31s")
}
