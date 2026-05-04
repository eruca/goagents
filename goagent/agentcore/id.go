package agentcore

import "github.com/google/uuid"

type RunID uuid.UUID

func NewRunID() RunID {
	return RunID(uuid.New())
}

func RunIDFromString(value string) (RunID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return RunID(uuid.Nil), err
	}
	return RunID(parsed), nil
}

func (id RunID) IsZero() bool {
	return uuid.UUID(id) == uuid.Nil
}

func (id RunID) String() string {
	return uuid.UUID(id).String()
}
