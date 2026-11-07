//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"testing"
)

func TestCrossNodeConnectivityE2E(t *testing.T) {
	cmd := exec.Command("bash", "./test/e2e/e2e.sh")
	cmd.Dir = "../.."
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("E2E connectivity suite failed: %v", err)
	}
}
