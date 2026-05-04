package contextkit

import "testing"

func TestProfileFromEnvDefaultsToStandard(t *testing.T) {
	t.Parallel()

	got := ProfileFromLookup(func(key string) (string, bool) {
		return "", false
	})

	if got != ProfileStandard {
		t.Fatalf("unexpected profile: got=%q want=%q", got, ProfileStandard)
	}
}

func TestProfileFromEnvAcceptsDeepFlag(t *testing.T) {
	t.Parallel()

	got := ProfileFromLookup(func(key string) (string, bool) {
		if key != EnvDeepCompression {
			t.Fatalf("unexpected env key: %s", key)
		}
		return "1", true
	})

	if got != ProfileDeep {
		t.Fatalf("unexpected profile: got=%q want=%q", got, ProfileDeep)
	}
}

func TestProfileFromEnvFallsBackWhenFlagIsNotEnabled(t *testing.T) {
	t.Parallel()

	got := ProfileFromLookup(func(key string) (string, bool) {
		return "0", true
	})

	if got != ProfileStandard {
		t.Fatalf("unexpected profile: got=%q want=%q", got, ProfileStandard)
	}
}

func TestLevelsForProfile(t *testing.T) {
	t.Parallel()

	standard := LevelsForProfile(ProfileStandard)
	if len(standard) != 3 || standard[0] != LevelToolResultBudget || standard[2] != LevelProjection {
		t.Fatalf("unexpected standard levels: %#v", standard)
	}

	deep := LevelsForProfile(ProfileDeep)
	if len(deep) != 5 || deep[0] != LevelToolResultBudget || deep[4] != LevelAutoCompact {
		t.Fatalf("unexpected deep levels: %#v", deep)
	}
}
