package server

import "testing"

func TestIntentStopFixturesLeaveNoRowsPG(t *testing.T) {
	m := intentStopPGManager(t)
	counts := func() (int, int) {
		var tasks, profiles int
		if err := m.pg.QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
			t.Fatal(err)
		}
		if err := m.pg.QueryRow(`SELECT count(*) FROM llm_profiles`).Scan(&profiles); err != nil {
			t.Fatal(err)
		}
		return tasks, profiles
	}
	beforeTasks, beforeProfiles := counts()
	for i := 0; i < 2; i++ {
		t.Run("chain", TestReplaceTaskLLMProfilesReopensChainUnavailablePG)
		t.Run("deadline", TestRunIntentTaskDeadlineIsNotWorkerRunLimitPG)
	}
	afterTasks, afterProfiles := counts()
	if afterTasks != beforeTasks || afterProfiles != beforeProfiles {
		t.Fatalf("fixture 净增 tasks=%d profiles=%d", afterTasks-beforeTasks, afterProfiles-beforeProfiles)
	}
}
