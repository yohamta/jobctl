package cmd

import (
	"fmt"
	"testing"

	"github.com/dagu-dev/dagu/internal/dag/scheduler"
	"github.com/stretchr/testify/require"
)

func TestRetryCommand(t *testing.T) {
	t.Run("RetryDAG", func(t *testing.T) {
		setup := setupTest(t)
		defer setup.cleanup()

		dagFile := testDAGFile("retry.yaml")

		// Run a DAG.
		testRunCommand(t, startCmd(), cmdTest{args: []string{"start", `--params="foo"`, dagFile}})

		// Find the request ID.
		eng := setup.engine
		status, err := eng.GetStatus(dagFile)
		require.NoError(t, err)
		require.Equal(t, status.Status.Status, scheduler.StatusSuccess)
		require.NotNil(t, status.Status)

		reqID := status.Status.RequestID

		// Retry with the request ID.
		testRunCommand(t, retryCmd(), cmdTest{
			args:        []string{"retry", fmt.Sprintf("--req=%s", reqID), dagFile},
			expectedOut: []string{"param is foo"},
		})
	})
}
