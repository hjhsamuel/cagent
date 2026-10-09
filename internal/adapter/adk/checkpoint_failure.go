package adk

import (
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
)

func checkpointFailure(reason string) error {
	switch reason {
	case "":
		return nil
	case "execution_outcome_uncertain":
		return agent.ErrUncertain
	default:
		return ErrModel
	}
}

func CheckpointFailure(cp domain.Checkpoint) error {
	if !IsPendingCheckpoint(cp) {
		return nil
	}
	var saved struct{ Failure string }
	if err := decodeJSON(cp.Data, &saved); err != nil {
		return invalid("checkpoint.data")
	}
	return checkpointFailure(saved.Failure)
}

func (*Runtime) CheckpointFailure(cp domain.Checkpoint) error { return CheckpointFailure(cp) }
