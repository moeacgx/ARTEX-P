package tool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestForgetRetainsUnconfirmedTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.output")
	if err := os.WriteFile(path, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	task := &Task{ID: "held", Status: TaskKilled, OutputPath: path, done: make(chan struct{})}
	m := &Manager{tasks: map[string]*Task{"held": task}, order: []string{"held"}}
	m.Forget("held")
	if _, ok := m.Get("held"); !ok {
		t.Error("未确认退出的任务被删除")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("未确认退出的输出被删除: %v", err)
	}
	close(task.done)
	m.Forget("held")
	if _, ok := m.Get("held"); ok {
		t.Error("已退出任务未被删除")
	}
}
