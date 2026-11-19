package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	ServiceTier json.RawMessage `json:"serviceTier"`
	TierWrites  int             `json:"tierWrites"`
	Model       string          `json:"model"`
	Effort      string          `json:"effort"`
	Archived    bool            `json:"archived"`
	Locked      bool            `json:"locked"`
	Updates     int             `json:"updates"`
}

// The helper simulates only the official settings protocol; it cannot run turns.
func TestRPCServer(t *testing.T) {
	path := os.Getenv("HIT_SYNC_TEST_STATE")
	if path == "" {
		return
	}
	var heldID int
	var heldValue json.RawMessage
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     int                        `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.ID == 0 {
			continue
		}
		if req.Method == "test/reverse" {
			if heldID == 0 {
				heldID = req.ID
				heldValue = req.Params["value"]
				continue
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"id": req.ID, "result": req.Params["value"]})
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"id": heldID, "result": heldValue})
			heldID = 0
			continue
		}
		bytes, _ := os.ReadFile(path)
		state := map[string]*fixture{}
		_ = json.Unmarshal(bytes, &state)
		var id string
		_ = json.Unmarshal(req.Params["threadId"], &id)
		row := state[id]
		var result any = map[string]any{}
		var failure *rpcError
		switch req.Method {
		case "initialize", "thread/unsubscribe":
		case "thread/unarchive":
			row.Archived = false
		case "thread/archive":
			row.Archived = true
		case "thread/resume":
			if row.Locked {
				failure = &rpcError{-32000, "already has an active writer"}
			} else {
				result = map[string]any{"model": row.Model, "reasoningEffort": row.Effort, "modelProvider": "openai", "serviceTier": row.ServiceTier}
			}
		case "thread/settings/update":
			_ = json.Unmarshal(req.Params["model"], &row.Model)
			_ = json.Unmarshal(req.Params["effort"], &row.Effort)
			if tier, ok := req.Params["serviceTier"]; ok {
				row.ServiceTier = tier
				row.TierWrites++
			}
			row.Updates++
		default:
			failure = &rpcError{-32601, "unsupported"}
		}
		updated, _ := json.Marshal(state)
		_ = os.WriteFile(path, updated, 0600)
		response := map[string]any{"id": req.ID, "result": result}
		if failure != nil {
			delete(response, "result")
			response["error"] = failure
		}
		_ = json.NewEncoder(os.Stdout).Encode(response)
	}
	os.Exit(0)
}

func setup(t *testing.T, state map[string]*fixture) (*rpc, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	bytes, _ := json.Marshal(state)
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIT_SYNC_TEST_STATE", path)
	t.Setenv("CODEX_HOME", root)
	r, err := startRPC(context.Background(), os.Args[0], []string{"-test.run=^TestRPCServer$"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	return r, path
}
func readFixture(t *testing.T, path string) map[string]*fixture {
	t.Helper()
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]*fixture{}
	if err := json.Unmarshal(bytes, &state); err != nil {
		t.Fatal(err)
	}
	return state
}
func testModel() model {
	m := model{Name: "new", Default: "medium"}
	m.Efforts = append(m.Efforts, struct {
		Name string `json:"reasoningEffort"`
	}{"medium"})
	return m
}
func TestApplyResumeAndRestore(t *testing.T) {
	r, path := setup(t, map[string]*fixture{
		"a": {Model: "old", Effort: "high", Archived: true},
		"b": {Model: "old", Effort: "high", Locked: true},
	})
	ctx := context.Background()
	o := options{model: "new", effort: "medium", provider: "openai"}
	data := &report{Target: target{Model: "new", Effort: "medium"}, Entries: []*entry{{ID: "a", Archived: true, Status: "pending"}, {ID: "b", Status: "pending"}}}
	reportPath := filepath.Join(t.TempDir(), "report.json")
	if err := perform(ctx, r, o, testModel(), data, reportPath, false, false); err != nil {
		t.Fatal(err)
	}
	if data.Complete || data.Entries[0].Status != "verified" || data.Entries[1].Status != "locked" {
		t.Fatalf("성공과 잠금 상태를 구분하지 못했습니다: %+v", data)
	}
	state := readFixture(t, path)
	if !state["a"].Archived || state["a"].Model != "new" {
		t.Fatal("보관 상태와 변경 설정이 보존되어야 합니다")
	}
	state["b"].Locked = false
	bytes, _ := json.Marshal(state)
	_ = os.WriteFile(path, bytes, 0600)
	resumed, err := readReport(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := perform(ctx, r, o, testModel(), resumed, reportPath, false, true); err != nil {
		t.Fatal(err)
	}
	state = readFixture(t, path)
	if !resumed.Complete || state["a"].Updates != 1 || state["b"].Updates != 1 {
		t.Fatal("재시도는 잠긴 채팅만 처리해야 합니다")
	}
	for _, e := range resumed.Entries {
		e.Status = "pending"
	}
	if err := perform(ctx, r, o, testModel(), resumed, reportPath, true, false); err != nil {
		t.Fatal(err)
	}
	for _, row := range readFixture(t, path) {
		if row.Model != "old" || row.Effort != "high" {
			t.Fatal("백업의 원래 설정으로 복구되어야 합니다")
		}
	}
}
func TestRestorePreservesLaterChanges(t *testing.T) {
	r, path := setup(t, map[string]*fixture{"a": {Model: "user-choice", Effort: "high"}})
	high, medium := "high", "medium"
	e := &entry{ID: "a", Status: "pending", Before: &settings{Model: "old", Effort: &high}, After: &settings{Model: "new", Effort: &medium}}
	if err := process(context.Background(), r, options{provider: "openai"}, testModel(), e, true, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if e.Status != "changed-since-sync" || readFixture(t, path)["a"].Updates != 0 {
		t.Fatal("동기화 후 사용자가 바꾼 설정을 덮어쓰면 안 됩니다")
	}
}
func TestMissingAndFailedAreNotRetriedAsLocked(t *testing.T) {
	r, path := setup(t, map[string]*fixture{"a": {Model: "old", Effort: "high"}})
	data := &report{Entries: []*entry{{ID: "a", Status: "failed"}, {ID: "absent", Status: "missing"}}}
	if err := perform(context.Background(), r, options{}, testModel(), data, filepath.Join(t.TempDir(), "report.json"), false, true); err != nil {
		t.Fatal(err)
	}
	if readFixture(t, path)["a"].Updates != 0 || data.Entries[0].Status != "failed" {
		t.Fatal("잠금 재시도에서 다른 실패 항목은 보존되어야 합니다")
	}
}
func TestConcurrentSyncIsRejected(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	release, err := lockSync()
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockSync(); err == nil {
		second()
		t.Fatal("동시 실행은 거부해야 합니다")
	}
	release()
	release, err = lockSync()
	if err != nil {
		t.Fatal("작업 종료 후 잠금이 해제되어야 합니다:", err)
	}
	release()
}

func TestBackupFailurePreventsUpdate(t *testing.T) {
	r, path := setup(t, map[string]*fixture{"a": {Model: "old", Effort: "high", Archived: true}})
	e := &entry{ID: "a", Status: "pending", Archived: true, ArchiveRestored: true}
	checkpoints := 0
	err := process(context.Background(), r, options{model: "new", effort: "medium", provider: "openai"}, testModel(), e, false, func() error {
		checkpoints++
		if checkpoints == 1 {
			return nil
		}
		if e.Before == nil || e.Before.Model != "old" || e.After == nil {
			t.Fatal("설정 변경 전에 백업을 구성해야 합니다")
		}
		return os.ErrPermission
	})
	if err == nil {
		t.Fatal("백업 실패를 보고해야 합니다")
	}
	state := readFixture(t, path)["a"]
	if state.Updates != 0 || !state.Archived || !e.ArchiveRestored {
		t.Fatal("백업 실패 시 설정을 유지하고 보관 상태를 복구해야 합니다")
	}
}

func TestRecentThreadsUseActivityRatherThanCreationOrSettingsUpdates(t *testing.T) {
	since := int64(1000)
	recent, boundary, old, zero := int64(1100), since, int64(999), int64(0)
	rows := []thread{
		{ID: "recent", RecencyAt: &recent},
		{ID: "boundary", RecencyAt: &boundary, Archived: true},
		{ID: "old", RecencyAt: &old},
		{ID: "unknown"},
		{ID: "zero", RecencyAt: &zero},
	}
	selected := recentThreads(rows, since)
	if len(selected) != 2 || selected[0].ID != "recent" || selected[1].ID != "boundary" {
		t.Fatalf("최근 사용 시각으로 기준일을 포함해 선택해야 합니다: %+v", selected)
	}
	if len(rows) != 5 {
		t.Fatal("원본 채팅 목록을 변경하면 안 됩니다")
	}
}

func TestResumePreservesSavedCutoff(t *testing.T) {
	since := int64(1000)
	path := filepath.Join(t.TempDir(), "report.json")
	original := &report{Target: target{Model: "new"}, Since: &since, Entries: []*entry{{ID: "recent", Status: "pending"}}}
	if err := writeReport(path, original); err != nil {
		t.Fatal(err)
	}
	loaded, err := readReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Since == nil || *loaded.Since != since || len(loaded.Entries) != 1 || loaded.Entries[0].ID != "recent" {
		t.Fatal("재개 시 최초 선택 범위와 기간을 보존해야 합니다")
	}
}

func TestRPCDispatchesOutOfOrderReplies(t *testing.T) {
	r, _ := setup(t, map[string]*fixture{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	failures := make(chan error, 2)
	var wg sync.WaitGroup
	for value := range 2 {
		wg.Go(func() {
			var got int
			err := r.call(ctx, "test/reverse", map[string]any{"value": value}, &got)
			if err == nil && got != value {
				err = fmt.Errorf("다른 요청의 응답을 받았습니다: 기대 %d, 실제 %d", value, got)
			}
			failures <- err
		})
	}
	wg.Wait()
	for range 2 {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
}

func TestParallelUpdatesPreserveEveryBackupAndArchive(t *testing.T) {
	state := map[string]*fixture{}
	data := &report{Target: target{Model: "new"}}
	for i := range 20 {
		id := fmt.Sprint(i)
		state[id] = &fixture{Model: "old", Effort: "high", Archived: i%2 == 0}
		data.Entries = append(data.Entries, &entry{ID: id, Status: "pending", Archived: i%2 == 0})
	}
	r, path := setup(t, state)
	reportPath := filepath.Join(t.TempDir(), "report.json")
	if err := perform(context.Background(), r, options{model: "new", effort: "medium", provider: "openai", workers: 4}, testModel(), data, reportPath, false, false); err != nil {
		t.Fatal(err)
	}
	saved, err := readReport(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Complete || len(saved.Entries) != 20 {
		t.Fatal("병렬 처리 결과가 누락됐습니다")
	}
	actual := readFixture(t, path)
	for _, e := range saved.Entries {
		if e.Before == nil || e.Before.Model != "old" || e.Status != "verified" || actual[e.ID].Updates != 1 || actual[e.ID].Archived != e.Archived {
			t.Fatalf("채팅 %s의 백업·보관 상태·검증 결과가 보존되어야 합니다", e.ID)
		}
	}
}

func TestListedMatchRequiresCompleteModelAndEffort(t *testing.T) {
	medium, high := "medium", "high"
	m := testModel()
	cases := []struct {
		name    string
		thread  thread
		options options
		want    bool
	}{
		{"이미 일치", thread{Model: "new", Effort: &medium}, options{model: "new", effort: "medium"}, true},
		{"모델 미제공", thread{Effort: &medium}, options{model: "new", effort: "medium"}, false},
		{"추론 수준 미제공", thread{Model: "new"}, options{model: "new", effort: "medium"}, false},
		{"추론 수준 불일치", thread{Model: "new", Effort: &high}, options{model: "new", effort: "medium"}, false},
		{"기존 수준 유지", thread{Model: "new", Effort: &medium}, options{model: "new"}, true},
		{"미지원 수준은 기본값으로 변경 필요", thread{Model: "new", Effort: &high}, options{model: "new"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := listedMatch(c.thread, c.options, m); got != c.want {
				t.Fatalf("기대 %v, 실제 %v", c.want, got)
			}
		})
	}
}

func TestForceQuitTargetsOnlyRecognizedApps(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS 데스크톱 종료 테스트")
	}
	var calls [][]string
	err := requestDesktopQuit(context.Background(), []string{"/Applications/ChatGPT.app", "/Applications/Codex.app"}, func(ctx context.Context, command string, args ...string) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("앱 종료 요청에는 제한 시간이 있어야 합니다")
		}
		calls = append(calls, append([]string{command}, args...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][0] != "/usr/bin/osascript" || calls[0][2] != `tell application "/Applications/ChatGPT.app" to quit` || calls[1][2] != `tell application "/Applications/Codex.app" to quit` {
		t.Fatalf("확인한 앱에만 종료 요청을 보내야 합니다: %v", calls)
	}
}

func TestForceQuitDoesNotTerminateOtherProcesses(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS 데스크톱 종료 테스트")
	}
	for _, apps := range [][]string{nil, {"/Applications/ChatGPT.app", "/Applications/Other.app"}} {
		called := false
		err := requestDesktopQuit(context.Background(), apps, func(context.Context, string, ...string) error { called = true; return nil })
		if err == nil || called {
			t.Fatal("알 수 없는 앱이나 다른 CLI의 잠금을 임의로 종료하면 안 됩니다")
		}
	}
}

func TestForceQuitReportsRejectedShutdown(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS 데스크톱 종료 테스트")
	}
	err := requestDesktopQuit(context.Background(), []string{"/Applications/ChatGPT.app"}, func(context.Context, string, ...string) error { return os.ErrPermission })
	if err == nil {
		t.Fatal("앱 종료 요청 실패를 보고해야 합니다")
	}
}

func TestServiceTierApplyAndRestore(t *testing.T) {
	r, path := setup(t, map[string]*fixture{
		"chat": {Model: "new", Effort: "medium", ServiceTier: json.RawMessage(`"priority"`)},
	})
	ctx := context.Background()
	o := options{model: "new", effort: "medium", provider: "openai", serviceTier: "ultrafast"}
	e := &entry{ID: "chat"}
	checkpoint := func() error { return nil }
	if err := process(ctx, r, o, testModel(), e, false, checkpoint); err != nil {
		t.Fatal(err)
	}
	if e.Status != "verified" || string(e.Before.ServiceTier) != `"priority"` || string(e.After.ServiceTier) != `"ultrafast"` {
		t.Fatalf("속도만 달라도 변경하고 전후 값을 기록해야 합니다: %+v", e)
	}
	if err := process(ctx, r, o, testModel(), e, true, checkpoint); err != nil {
		t.Fatal(err)
	}
	row := readFixture(t, path)["chat"]
	if e.Status != "restored" || string(row.ServiceTier) != `"priority"` || row.TierWrites != 2 {
		t.Fatalf("이전 속도로 복구해야 합니다: %+v", row)
	}
}

func TestOmittedTierSurvivesChangesAndLegacyRestore(t *testing.T) {
	r, path := setup(t, map[string]*fixture{
		"chat": {Model: "old", Effort: "high", ServiceTier: json.RawMessage(`"ultrafast"`)},
	})
	ctx := context.Background()
	o := options{model: "new", effort: "medium", provider: "openai"}
	e := &entry{ID: "chat"}
	checkpoint := func() error { return nil }
	if err := process(ctx, r, o, testModel(), e, false, checkpoint); err != nil {
		t.Fatal(err)
	}
	// Older reports omit the tier entirely.
	e.Before.ServiceTier, e.After.ServiceTier = nil, nil
	if err := process(ctx, r, o, testModel(), e, true, checkpoint); err != nil {
		t.Fatal(err)
	}
	row := readFixture(t, path)["chat"]
	if e.Status != "restored" || row.Model != "old" || string(row.ServiceTier) != `"ultrafast"` || row.TierWrites != 0 {
		t.Fatalf("속도 생략과 예전 백업 복구는 속도를 쓰지 않아야 합니다: %+v", row)
	}
}

func TestExplicitTierNeedsDetailEvenWhenModelAndEffortMatch(t *testing.T) {
	medium := "medium"
	row := thread{Model: "new", Effort: &medium}
	o := options{model: "new", effort: medium, serviceTier: "priority"}
	if listedMatch(row, o, testModel()) {
		t.Fatal("목록에는 속도가 없으므로 상세 확인이 필요합니다")
	}
}
