package peerflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/PeerDB-io/peerdb/flow/activities"
	"github.com/PeerDB-io/peerdb/flow/generated/protos"
)

// runWaitForNewRows runs QRepWaitForNewRowsWorkflow with QRepHasNewRows answering hasNewRows,
// and returns how far into the run (workflow time) the check happened.
func runWaitForNewRows(
	t *testing.T, writeType protos.QRepWriteType, hasNewRows bool,
) (*testsuite.TestWorkflowEnvironment, time.Duration) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(QRepWaitForNewRowsWorkflow)
	env.RegisterActivity(&activities.FlowableActivity{})

	start := env.Now()
	var checkedAt time.Duration
	env.OnActivity("QRepHasNewRows", mock.Anything, mock.Anything, mock.Anything).Return(
		func(context.Context, *protos.QRepConfig, *protos.QRepPartition) (bool, error) {
			checkedAt = env.Now().Sub(start)
			return hasNewRows, nil
		})
	env.OnActivity("PeerDBFullRefreshOverwriteMode", mock.Anything, mock.Anything).Return(false, nil).Maybe()

	config := &protos.QRepConfig{
		FlowJobName:               "test_qrep",
		WaitBetweenBatchesSeconds: 900,
		WriteMode:                 &protos.QRepWriteMode{WriteType: writeType},
	}
	env.ExecuteWorkflow(QRepWaitForNewRowsWorkflow, config, &protos.QRepPartition{PartitionId: "last"})
	require.True(t, env.IsWorkflowCompleted())
	return env, checkedAt
}

func TestQRepWaitForNewRowsWaitsBeforeTheFirstCheck(t *testing.T) {
	env, checkedAt := runWaitForNewRows(t, protos.QRepWriteType_QREP_WRITE_MODE_UPSERT, true)

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 900*time.Second, checkedAt,
		"new rows must not start the next run before the refresh interval has passed")
}

func TestQRepWaitForNewRowsSleepsAgainWhenNothingIsNew(t *testing.T) {
	env, checkedAt := runWaitForNewRows(t, protos.QRepWriteType_QREP_WRITE_MODE_APPEND, false)

	require.Equal(t, 900*time.Second, checkedAt)
	require.True(t, workflow.IsContinueAsNewError(env.GetWorkflowError()))
}

func TestQRepWaitForNewRowsLeavesOverwriteMirrorsAlone(t *testing.T) {
	env, checkedAt := runWaitForNewRows(t, protos.QRepWriteType_QREP_WRITE_MODE_OVERWRITE, true)

	require.NoError(t, env.GetWorkflowError())
	require.Zero(t, checkedAt)
}
