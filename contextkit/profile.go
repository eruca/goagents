package contextkit

import (
	"os"
	"strings"
)

const EnvDeepCompression = "CONTEXT_DEEP_COMPRESSION"

type Profile string

const (
	ProfileStandard Profile = "standard"
	ProfileDeep     Profile = "deep"
)

type Level int

const (
	LevelToolResultBudget   Level = 1
	LevelWindowPruning      Level = 2
	LevelProjection         Level = 3
	LevelReversibleCollapse Level = 4
	LevelAutoCompact        Level = 5
)

func ProfileFromEnv() Profile {
	return ProfileFromLookup(os.LookupEnv)
}

func ProfileFromLookup(lookup func(string) (string, bool)) Profile {
	value, ok := lookup(EnvDeepCompression)
	if !ok {
		return ProfileStandard
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return ProfileDeep
	default:
		return ProfileStandard
	}
}

func LevelsForProfile(profile Profile) []Level {
	switch profile {
	case ProfileDeep:
		return []Level{
			LevelToolResultBudget,
			LevelWindowPruning,
			LevelProjection,
			LevelReversibleCollapse,
			LevelAutoCompact,
		}
	default:
		return []Level{
			LevelToolResultBudget,
			LevelWindowPruning,
			LevelProjection,
		}
	}
}
