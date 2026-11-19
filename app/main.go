package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultServiceTier = "default"

type settings struct {
	ServiceTier json.RawMessage `json:"serviceTier,omitempty"`
	Model       string          `json:"model"`
	Effort      *string         `json:"effort"`
	Provider    string          `json:"provider,omitempty"`
}
type thread struct {
	ID        string  `json:"id"`
	Provider  string  `json:"modelProvider"`
	Archived  bool    `json:"archived"`
	RecencyAt *int64  `json:"recencyAt"`
	Model     string  `json:"model"`
	Effort    *string `json:"reasoningEffort"`
}
type serviceTier struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type model struct {
	ServiceTiers []serviceTier `json:"serviceTiers"`
	Name         string        `json:"model"`
	Default      string        `json:"defaultReasoningEffort"`
	Efforts      []struct {
		Name string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
}
type entry struct {
	ID              string    `json:"id"`
	Status          string    `json:"status"`
	Archived        bool      `json:"archived"`
	Before          *settings `json:"before,omitempty"`
	After           *settings `json:"after,omitempty"`
	Error           string    `json:"error,omitempty"`
	ArchiveRestored bool      `json:"archiveRestored,omitempty"`
}
type target struct {
	ServiceTier string `json:"serviceTier,omitempty"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	Provider    string `json:"provider,omitempty"`
}
type report struct {
	Target         target   `json:"target"`
	Entries        []*entry `json:"entries"`
	Complete       bool     `json:"complete"`
	Since          *int64   `json:"since,omitempty"`
	RestoreResults []*entry `json:"restoreResults,omitempty"`
}
type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }

type options struct {
	serviceTier                                                          string
	model, effort, provider, codex, socket, reportDir, resume, restore   string
	apply, models, archived, standalone, wait, retry, reopen, all, force bool
	waitSeconds, workers                                                 int
	threads                                                              stringsFlag
}

func home() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}
func defaultCodex() string {
	if path, err := exec.LookPath("codex"); err == nil {
		return path
	}
	if runtime.GOOS == "darwin" {
		roots := []string{"/Applications"}
		if userHome, err := os.UserHomeDir(); err == nil {
			roots = append([]string{filepath.Join(userHome, "Applications")}, roots...)
		}
		for _, root := range roots {
			for _, app := range []string{"Codex.app", "ChatGPT.app"} {
				resources := filepath.Join(root, app, "Contents", "Resources")
				for _, path := range []string{
					filepath.Join(resources, "codex"),
					filepath.Join(resources, "codex-cli", "CodexCLI.app", "Contents", "MacOS", "codex"),
				} {
					if executable, err := exec.LookPath(path); err == nil {
						return executable
					}
				}
			}
		}
	}
	return "codex"
}
func parse() options {
	var o options
	flag.StringVar(&o.model, "model", "", "목표 모델")
	flag.StringVar(&o.effort, "effort", "", "추론 수준 (생략하면 지원되는 기존 값 유지)")
	flag.Func("service-tier", "답변 속도: default, fast, ultrafast (생략하면 기존 설정 유지)", func(value string) error {
		var err error
		o.serviceTier, err = parseServiceTier(value)
		return err
	})
	flag.StringVar(&o.provider, "provider", "openai", "모델 제공자")
	flag.StringVar(&o.codex, "codex", defaultCodex(), "Codex 실행 파일")
	flag.StringVar(&o.socket, "socket", "", "실행 중인 App Server 제어 소켓")
	flag.StringVar(&o.reportDir, "report-dir", filepath.Join(home(), "model-sync-backups"), "백업·결과 폴더")
	flag.StringVar(&o.resume, "resume", "", "결과 JSON에서 미완료 항목 이어서 실행")
	flag.StringVar(&o.restore, "restore", "", "결과 JSON의 변경 전 설정으로 복구")
	flag.BoolVar(&o.apply, "apply", false, "실제 적용 (생략하면 조회)")
	flag.BoolVar(&o.models, "models", false, "사용 가능한 모델 조회")
	flag.BoolVar(&o.archived, "include-archived", true, "보관 채팅 포함")
	flag.BoolVar(&o.standalone, "standalone", false, "독립 App Server 사용")
	flag.BoolVar(&o.wait, "wait-for-desktop-exit", false, "앱이 종료된 다음 시작")
	flag.BoolVar(&o.force, "force-locked", false, "잠금이 남으면 데스크톱 앱을 종료하고 재시도한 뒤 다시 열기 (외부 터미널 전용)")
	flag.BoolVar(&o.retry, "retry-locked", false, "잠긴 채팅을 다시 시도 (macOS에서는 앱 종료 대기)")
	flag.BoolVar(&o.reopen, "reopen-app", false, "앱 종료 대기 후 성공하면 앱 다시 열기")
	flag.BoolVar(&o.all, "all", false, "기간 제한 없이 전체 채팅 선택 (기본: 최근 30일)")
	flag.IntVar(&o.workers, "workers", 4, "동시 처리 수 (1~16)")
	flag.IntVar(&o.waitSeconds, "wait-seconds", 600, "앱 종료 최대 대기 초")
	flag.Var(&o.threads, "thread", "채팅 ID로 범위 제한 (반복 가능)")
	flag.Parse()
	return o
}
func runningDesktops() ([]string, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("데스크톱 앱 제어는 현재 macOS에서 지원합니다")
	}
	output, err := exec.Command("ps", "-axo", "comm=").Output()
	if err != nil {
		return nil, err
	}
	apps := []string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		app := desktopBundle(strings.TrimSpace(line))
		if app != "" && !seen[app] {
			apps = append(apps, app)
			seen[app] = true
		}
	}
	return apps, nil
}

func desktopBundle(executable string) string {
	if !filepath.IsAbs(executable) {
		return ""
	}
	for _, name := range []string{"ChatGPT", "Codex"} {
		bundle := filepath.Dir(filepath.Dir(filepath.Dir(executable)))
		if filepath.Base(bundle) == name+".app" && executable == filepath.Join(bundle, "Contents", "MacOS", name) {
			return bundle
		}
	}
	return ""
}

func desktopRunning() (bool, error) {
	apps, err := runningDesktops()
	return len(apps) > 0, err
}

// Quit only the known desktop applications, allowing their writers to flush normally.
func requestDesktopQuit(ctx context.Context, apps []string, run func(context.Context, string, ...string) error) error {
	if len(apps) == 0 {
		return errors.New("데스크톱 앱이 실행 중이 아닙니다. 다른 CLI가 보유한 잠금은 해당 프로세스를 종료해야 합니다")
	}
	for _, app := range apps {
		if !filepath.IsAbs(app) || (filepath.Base(app) != "ChatGPT.app" && filepath.Base(app) != "Codex.app") {
			return fmt.Errorf("종료할 수 없는 앱: %s", app)
		}
	}
	for _, app := range apps {
		quitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := run(quitCtx, "/usr/bin/osascript", "-e", fmt.Sprintf("tell application %q to quit", app))
		cancel()
		if err != nil {
			return fmt.Errorf("%s 종료 요청 실패: %w", app, err)
		}
	}
	return nil
}

func runDesktopCommand(ctx context.Context, command string, args ...string) error {
	output, err := exec.CommandContext(ctx, command, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func waitDesktop(ctx context.Context, seconds int) error {
	fmt.Fprintln(os.Stderr, "ChatGPT/Codex 앱 종료를 기다립니다. 종료되면 자동으로 계속합니다.")
	timeout := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timeout.Stop()
	for {
		running, err := desktopRunning()
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return errors.New("앱 종료 대기 시간 초과")
		case <-time.After(time.Second):
		}
	}
}
func connect(ctx context.Context, o options) (*rpc, error) {
	args := []string{"app-server"}
	socket := o.socket
	if !o.standalone && socket == "" {
		path := filepath.Join(home(), "app-server-control/app-server-control.sock")
		if _, err := os.Stat(path); err == nil {
			socket = path
		}
	}
	if socket != "" {
		args = append(args, "proxy", "--sock", socket)
	}
	r, err := startRPC(ctx, o.codex, args)
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("Codex 실행 파일 %q을 찾을 수 없습니다. Codex CLI를 PATH에 추가하거나 --codex로 실행 파일 경로를 지정하세요: %w", o.codex, err)
	}
	return r, err
}
func writeReport(path string, data *report) error {
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".model-sync-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(append(payload, '\n')); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
func readReport(path string) (*report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var result report
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result.Target.Provider == "" {
		result.Target.Provider = "openai"
	}
	if result.Target.Model == "" || result.Entries == nil {
		return nil, errors.New("유효한 동기화 기록이 아닙니다")
	}
	seen := map[string]bool{}
	for _, e := range result.Entries {
		if e == nil || e.ID == "" || seen[e.ID] {
			return nil, errors.New("기록에 빈 항목 또는 중복 채팅이 있습니다")
		}
		seen[e.ID] = true
	}
	return &result, nil
}
func listThreads(ctx context.Context, r *rpc, archived bool) ([]thread, error) {
	rows := []thread{}
	flags := []bool{false}
	if archived {
		flags = append(flags, true)
	}
	seen := map[string]bool{}
	for _, a := range flags {
		page, err := pages[thread](ctx, r, "thread/list", map[string]any{
			"limit": 100, "archived": a, "modelProviders": []string{}, "currentAccountOnly": true, "useStateDbOnly": true,
		})
		if err != nil {
			return nil, err
		}
		for _, item := range page {
			if !seen[item.ID] {
				item.Archived = a
				rows = append(rows, item)
				seen[item.ID] = true
			}
		}
	}
	return rows, nil
}

// Use conversation recency, because settings-only updates also change updatedAt.
func recentThreads(threads []thread, since int64) []thread {
	selected := []thread{}
	for _, t := range threads {
		if t.RecencyAt != nil && *t.RecencyAt > 0 && *t.RecencyAt >= since {
			selected = append(selected, t)
		}
	}
	return selected
}

func resumedSettings(ctx context.Context, r *rpc, id string) (*settings, error) {
	var result struct {
		ServiceTier json.RawMessage `json:"serviceTier"`
		Model       string          `json:"model"`
		Effort      *string         `json:"reasoningEffort"`
		Provider    string          `json:"modelProvider"`
	}
	err := r.call(ctx, "thread/resume", map[string]any{"threadId": id, "excludeTurns": true}, &result)
	if err != nil {
		return nil, err
	}
	return &settings{Model: result.Model, Effort: result.Effort, Provider: result.Provider, ServiceTier: result.ServiceTier}, nil
}
func same(a, b *settings, compareTier bool) bool {
	if a == nil || b == nil || a.Model != b.Model {
		return false
	}
	if compareTier && !sameServiceTier(a.ServiceTier, b.ServiceTier) {
		return false
	}
	if a.Effort == nil || b.Effort == nil {
		return a.Effort == nil && b.Effort == nil
	}
	return *a.Effort == *b.Effort
}
func parseServiceTier(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "default", "standard", "기본":
		return defaultServiceTier, nil
	case "fast", "priority":
		return "priority", nil
	case "ultrafast":
		return "ultrafast", nil
	default:
		return "", errors.New("--service-tier는 default, fast 또는 ultrafast여야 합니다")
	}
}

func supportsServiceTier(m model, tier string) bool {
	if tier == "" || tier == defaultServiceTier {
		return true
	}
	for _, supported := range m.ServiceTiers {
		if supported.ID == tier {
			return true
		}
	}
	return false
}

func serviceTierValue(tier string) json.RawMessage {
	if tier == defaultServiceTier {
		return json.RawMessage("null")
	}
	value, _ := json.Marshal(tier)
	return value
}

func sameServiceTier(a, b json.RawMessage) bool {
	// The API uses null to clear a tier. Accept the explicit standard name too.
	if bytes.Equal(a, []byte(`"default"`)) {
		a = json.RawMessage("null")
	}
	if bytes.Equal(b, []byte(`"default"`)) {
		b = json.RawMessage("null")
	}
	return bytes.Equal(a, b)
}

func supports(m model, effort string) bool {
	for _, e := range m.Efforts {
		if e.Name == effort {
			return true
		}
	}
	return false
}
func done(e *entry) bool {
	return e.Status == "verified" || e.Status == "already-matching" || e.Status == "restored" || e.Status == "already-restored"
}
func classify(err error) string {
	if strings.Contains(err.Error(), "active writer") {
		return "locked"
	}
	return "failed"
}
func process(ctx context.Context, r *rpc, o options, m model, e *entry, restore bool, checkpoint func() error) (err error) {
	if e.Status == "different-provider" {
		return nil
	}
	wasArchived := e.Archived
	loaded := false
	unarchived := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if loaded && !wasArchived {
			// Release this client's session after saving, avoiding hundreds of loaded threads.
			_ = r.call(cleanup, "thread/unsubscribe", map[string]any{"threadId": e.ID}, nil)
		}
		if unarchived {
			if archiveErr := r.call(cleanup, "thread/archive", map[string]any{"threadId": e.ID}, nil); archiveErr != nil {
				e.Status = "archive-restore-failed"
				e.Error = archiveErr.Error()
				err = archiveErr
			} else {
				e.ArchiveRestored = true
			}
		}
	}()
	if wasArchived {
		e.Status = "unarchiving"
		e.ArchiveRestored = false
		if err = checkpoint(); err != nil {
			return err
		}
		if err = r.call(ctx, "thread/unarchive", map[string]any{"threadId": e.ID}, nil); err != nil {
			return err
		}
		unarchived = true
	}
	current, err := resumedSettings(ctx, r, e.ID)
	if err != nil {
		return err
	}
	loaded = true
	if current.Provider != o.provider {
		e.Status = "different-provider"
		return nil
	}
	changeTier := o.serviceTier != ""
	if changeTier && len(current.ServiceTier) == 0 {
		return errors.New("App Server 응답에 현재 답변 속도가 없습니다")
	}
	if restore {
		if changeTier && (len(e.Before.ServiceTier) == 0 || len(e.After.ServiceTier) == 0) {
			return errors.New("백업에 변경 전후 답변 속도가 없습니다")
		}
		if same(current, e.Before, changeTier) {
			e.Status = "already-restored"
			return nil
		}
		if !same(current, e.After, changeTier) || (e.Before.Provider != "" && current.Provider != e.Before.Provider) {
			e.Status = "changed-since-sync"
			return nil
		}
	} else {
		if e.Before == nil {
			e.Before = current
		}
		effort := o.effort
		if effort == "" {
			effort = m.Default
			if current.Effort != nil && supports(m, *current.Effort) {
				effort = *current.Effort
			}
		}
		nextTier := current.ServiceTier
		if changeTier {
			nextTier = serviceTierValue(o.serviceTier)
		}
		e.After = &settings{Model: o.model, Effort: &effort, ServiceTier: nextTier}
		if same(current, e.After, changeTier) {
			e.Status = "already-matching"
			return nil
		}
	}
	e.Status = "applying"
	e.Error = ""
	if err = checkpoint(); err != nil {
		return err
	}
	want := e.After
	if restore {
		want = e.Before
	}
	params := map[string]any{"threadId": e.ID, "model": want.Model, "effort": want.Effort}
	expectedTier := current.ServiceTier
	if changeTier {
		params["serviceTier"] = want.ServiceTier
		expectedTier = want.ServiceTier
	}
	if err = r.call(ctx, "thread/settings/update", params, nil); err != nil {
		return err
	}
	actual, err := resumedSettings(ctx, r, e.ID)
	if err != nil {
		return err
	}
	if !same(actual, want, false) {
		return errors.New("변경 후 모델과 추론 수준이 일치하지 않습니다")
	}
	if !sameServiceTier(actual.ServiceTier, expectedTier) {
		return errors.New("변경 후 답변 속도가 기대값과 일치하지 않습니다")
	}
	e.Status = "verified"
	if restore {
		e.Status = "restored"
	}
	return nil
}
func perform(ctx context.Context, r *rpc, o options, m model, data *report, path string, restore, onlyLocked bool) error {
	data.Complete = false
	if err := writeReport(path, data); err != nil {
		return err
	}
	jobs := make(chan int, len(data.Entries))
	matching := 0
	for i, e := range data.Entries {
		if e.Status == "already-matching" {
			matching++
		}
		if done(e) || e.Status == "different-provider" || e.Status == "changed-since-sync" || e.Status == "missing" || (onlyLocked && e.Status != "locked") {
			continue
		}
		jobs <- i
	}
	close(jobs)
	total := len(jobs)
	workers := o.workers
	if workers == 0 {
		workers = 4
	}
	workers = min(workers, total)
	fmt.Fprintf(os.Stderr, "상세 조회 생략 %d개, 상세 처리 %d개 (동시 처리 %d)\n", matching, total, workers)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	completed := 0
	stop := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
		cancel()
	}
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				if workCtx.Err() != nil {
					return
				}
				local := *data.Entries[i]
				// Workers change private entries; only immutable snapshots enter the report.
				checkpoint := func() error {
					mu.Lock()
					defer mu.Unlock()
					snapshot := local
					data.Entries[i] = &snapshot
					err := writeReport(path, data)
					if err != nil {
						stop(err)
					}
					return err
				}
				err := process(workCtx, r, o, m, &local, restore, checkpoint)
				if err != nil && local.Status != "archive-restore-failed" {
					local.Status = classify(err)
					local.Error = err.Error()
				}
				if checkpoint() != nil {
					return
				}
				mu.Lock()
				completed++
				fmt.Fprintf(os.Stderr, "[%d/%d] %s %s\n", completed, total, local.ID, local.Status)
				var protocolError *rpcError
				if local.Status == "archive-restore-failed" || (errors.As(err, &protocolError) && protocolError.Code == -32601) {
					stop(err)
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	data.Complete = true
	for _, e := range data.Entries {
		if !done(e) && e.Status != "different-provider" {
			data.Complete = false
		}
	}
	return writeReport(path, data)
}

// A complete list snapshot can establish a no-op without loading or locking a chat.
func listedMatch(t thread, o options, m model) bool {
	if o.serviceTier != "" || t.Model == "" || t.Effort == nil {
		return false
	}
	effort := o.effort
	if effort == "" {
		effort = m.Default
		if supports(m, *t.Effort) {
			effort = *t.Effort
		}
	}
	return t.Model == o.model && *t.Effort == effort
}

func resultCounts(data *report) map[string]int {
	counts := map[string]int{}
	for _, e := range data.Entries {
		counts[e.Status]++
	}
	return counts
}

func lockSync() (func(), error) {
	if err := os.MkdirAll(home(), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(home(), "model-sync.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockFile(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("모델 동기화 잠금을 얻을 수 없습니다. 다른 작업이 실행 중인지 확인하세요: %w", err)
	}
	return func() { _ = file.Close() }, nil
}

func execute(ctx context.Context, o options) error {
	if flag.NArg() != 0 {
		return errors.New("알 수 없는 위치 인자입니다. --help를 확인하세요")
	}
	if o.socket != "" && o.standalone {
		return errors.New("--socket과 --standalone은 함께 사용할 수 없습니다")
	}
	if o.resume != "" && o.restore != "" {
		return errors.New("--resume과 --restore은 함께 사용할 수 없습니다")
	}
	if o.all && len(o.threads) > 0 {
		return errors.New("--all과 --thread는 함께 사용할 수 없습니다")
	}
	if o.workers < 1 || o.workers > 16 {
		return errors.New("--workers는 1~16이어야 합니다")
	}
	if o.waitSeconds <= 0 {
		return errors.New("--wait-seconds는 양수여야 합니다")
	}
	if runtime.GOOS != "darwin" && (o.wait || (o.apply && (o.force || o.reopen))) {
		return errors.New("데스크톱 앱 자동 종료·감지·다시 열기는 macOS에서 지원합니다. 사용 중인 Codex를 직접 닫은 뒤 다시 실행하세요")
	}
	if o.apply {
		release, lockErr := lockSync()
		if lockErr != nil {
			return lockErr
		}
		defer release()
	}
	var data *report
	var err error
	isRestore := o.restore != ""
	path := o.resume
	if isRestore {
		path = o.restore
	}
	if path != "" {
		if len(o.threads) > 0 || o.all {
			return errors.New("기록 재개·복구는 --all 또는 --thread와 함께 사용할 수 없습니다")
		}
		data, err = readReport(path)
		if err != nil {
			return err
		}
		if o.model != "" && o.model != data.Target.Model {
			return errors.New("기록과 다른 모델로 재개할 수 없습니다")
		}
		if o.effort != "" && o.effort != data.Target.Effort {
			return errors.New("기록과 다른 추론 수준으로 재개할 수 없습니다")
		}
		if o.serviceTier != "" && o.serviceTier != data.Target.ServiceTier {
			return errors.New("기록과 다른 답변 속도로 재개·복구할 수 없습니다")
		}
		o.serviceTier = data.Target.ServiceTier
		o.model = data.Target.Model
		o.effort = data.Target.Effort
		o.provider = data.Target.Provider
		if !isRestore && data.RestoreResults != nil {
			isRestore = true // Resume an interrupted restore using the existing result file.
		} else if isRestore {
			data.RestoreResults = data.Entries
			var selected []*entry
			for _, old := range data.Entries {
				if old.Before != nil && old.After != nil {
					copy := *old
					copy.Status = "pending"
					selected = append(selected, &copy)
				}
			}
			data.Entries = selected
			path = strings.TrimSuffix(path, filepath.Ext(path)) + "-restored-" + fmt.Sprint(time.Now().UnixNano()) + ".json"
		}
	}
	if !o.models && o.model == "" {
		return errors.New("--model로 목표 모델을 지정해 주세요")
	}
	if o.wait {
		if err = waitDesktop(ctx, o.waitSeconds); err != nil {
			return err
		}
	}
	r, err := connect(ctx, o)
	if err != nil {
		return err
	}
	defer func() {
		if r != nil {
			r.close()
		}
	}()
	catalog, err := pages[model](ctx, r, "model/list", map[string]any{"limit": 100, "modelProvider": o.provider})
	if err != nil {
		return err
	}
	if o.models {
		return json.NewEncoder(os.Stdout).Encode(catalog)
	}
	var chosen model
	for _, m := range catalog {
		if m.Name == o.model {
			chosen = m
		}
	}
	if !isRestore {
		if chosen.Name == "" {
			return errors.New("목표 모델을 사용할 수 없습니다. --models로 확인하세요")
		}
		if o.effort != "" && !supports(chosen, o.effort) {
			return errors.New("목표 모델이 추론 수준을 지원하지 않습니다")
		}
		if !supportsServiceTier(chosen, o.serviceTier) {
			return errors.New("목표 모델이 해당 답변 속도를 지원하지 않습니다. --models로 확인하세요")
		}
	}
	threads, err := listThreads(ctx, r, o.archived || data != nil)
	if err != nil {
		return err
	}
	byID := map[string]thread{}
	for _, t := range threads {
		byID[t.ID] = t
	}
	if data == nil {
		data = &report{Target: target{Model: o.model, Effort: o.effort, Provider: o.provider, ServiceTier: o.serviceTier}, Entries: []*entry{}}
		if len(o.threads) > 0 {
			threads = nil
			seen := map[string]bool{}
			for _, id := range o.threads {
				t, ok := byID[id]
				if !ok {
					return fmt.Errorf("현재 계정·호스트에 없는 채팅: %s", id)
				}
				if !seen[id] {
					threads = append(threads, t)
					seen[id] = true
				}
			}
		}
		if !o.all && len(o.threads) == 0 {
			since := time.Now().Add(-30 * 24 * time.Hour).Unix()
			data.Since = &since
			threads = recentThreads(threads, since)
		}
		for _, t := range threads {
			e := &entry{ID: t.ID, Archived: t.Archived, Status: "pending"}
			if t.Provider != o.provider {
				e.Status = "different-provider"
			} else if listedMatch(t, o, chosen) {
				e.Status = "already-matching"
			}
			data.Entries = append(data.Entries, e)
		}
	} else {
		for _, e := range data.Entries {
			t, ok := byID[e.ID]
			if !ok {
				if !done(e) {
					e.Status = "missing"
					e.Error = "현재 계정·호스트에 없는 채팅"
				}
			} else {
				if e.Status == "missing" {
					e.Status = "pending"
					e.Error = ""
				}
				if !(e.Archived && !e.ArchiveRestored) {
					e.Archived = t.Archived
				}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "대상 %d개, 모델 %s, 추론 수준 %s\n", len(data.Entries), o.model, o.effort)
	if !o.apply {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"mode": "preview", "matched": len(data.Entries), "target": data.Target, "since": data.Since, "results": resultCounts(data)})
	}
	if path == "" {
		path = filepath.Join(o.reportDir, fmt.Sprintf("model-sync-%d.json", time.Now().UnixNano()))
	}
	fmt.Fprintln(os.Stderr, "결과 기록:", path)
	if err = perform(ctx, r, o, chosen, data, path, isRestore, false); err != nil {
		return err
	}
	var forcedApps []string
	if o.retry || o.force {
		locked := false
		for _, e := range data.Entries {
			if e.Status == "locked" {
				locked = true
			}
		}
		if locked {
			r.close()
			r = nil
			if o.force {
				forcedApps, err = runningDesktops()
				if err != nil {
					return err
				}
				fmt.Fprintln(os.Stderr, "잠금 해제를 위해 데스크톱 앱 종료를 요청합니다. 진행 중인 채팅 작업도 중단됩니다.")
				if err = requestDesktopQuit(ctx, forcedApps, runDesktopCommand); err != nil {
					return err
				}
			}
			if runtime.GOOS == "darwin" {
				if err = waitDesktop(ctx, o.waitSeconds); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(os.Stderr, "잠긴 항목을 한 번 다시 시도합니다. 잠금이 남으면 사용 중인 Codex를 종료한 뒤 --resume으로 이어서 실행하세요.")
			}
			r, err = connect(ctx, o)
			if err != nil {
				return err
			}

			if err = perform(ctx, r, o, chosen, data, path, isRestore, true); err != nil {
				return err
			}
		}
	}
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"report": path, "results": resultCounts(data), "complete": data.Complete}); err != nil {
		return err
	}
	if !data.Complete {
		return errors.New("미완료 항목이 있습니다. --resume 결과파일 --apply로 이어서 실행하세요")
	}
	if len(forcedApps) > 0 || (o.reopen && (o.wait || o.retry)) {
		r.close()
		r = nil
		if len(forcedApps) == 0 {
			forcedApps = []string{"ChatGPT"}
		}
		for _, app := range forcedApps {
			if err = runDesktopCommand(ctx, "/usr/bin/open", "-a", app); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func main() {
	o := parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
