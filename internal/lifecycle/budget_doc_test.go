package lifecycle

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestR1_DeploymentShutdownBudgetMatchesLifecycleConstant(t *testing.T) {
	raw, err := os.ReadFile("../../DOCKER.md")
	if err != nil {
		t.Fatalf("read DOCKER.md: %v", err)
	}
	doc := string(raw)
	budget := DefaultShutdownBudget()
	checks := []string{
		fmt.Sprintf("名义预算是 %ds", int64(budget.Nominal().Seconds())),
		fmt.Sprintf("上界为 %ds", int64(budget.UpperBound().Seconds())),
		fmt.Sprintf("stop_grace_period: %ds", int64(budget.ComposeGracePeriod().Seconds())),
	}
	for _, want := range checks {
		if !strings.Contains(doc, want) {
			t.Fatalf("DOCKER.md does not contain %q", want)
		}
	}
}
