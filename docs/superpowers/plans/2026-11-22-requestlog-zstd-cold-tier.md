# 请求日志 zstd 冷层 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 请求日志双窗口留存——滚出 7 天热窗的轮转文件 zstd 归档（原子 tmp→rename→delete），满 30 天删除；归档对索引与查询路径不可见（离线分析用），读路径零功能变更。

**Architecture:** 唯一新功能代码在 `internal/observe/logfile`：sweep tick 在 retention 之后派发热窗外候选给旁路压缩 worker（独立 goroutine，不占 writer goroutine），worker 压完删原文件并触发 OnChange 让 indexer 走现有"消失文件删行"路径。`.zst` 后缀使三处 `.log` 精确匹配（indexer 认领/reconcile/查询扫描）天然不认领。config 加 `hot_retention`（默认 168h，`0` 禁用，`>= retention` fail-closed）。

**Tech Stack:** Go 1.27（module `model-proxy`）、`github.com/klauspost/compress/zstd`（新增唯一直接依赖，纯 Go）、SQLite indexer（零改动）。

**设计文档:** `docs/superpowers/specs/2026-11-22-requestlog-zstd-cold-tier-design.md`

## Global Constraints

- 红线 2：压缩 worker 由 logfile.Logger 拥有，有 stop/wait/final flush（Run 退出路径 `close(archive)` + `<-archiveDone`）；不持跨域锁。
- 测试纪律：时序用 channel/gate/Chtimes 驱动，禁止固定 Sleep 证明时序；不写真实 HOME；纯行为测试跟随 owner package。
- 时龄基准：`.log` 沿用 mtime；`.zst` 的 mtime 是压缩时刻，时龄一律取文件名内 YYYYMMDD 日戳（`archiveDay`）。
- 归档序列固定：写 `X.log.zst.tmp` → rename `X.log.zst` → 删 `X.log`；`.zst` 已存在则只删原文件（崩溃恢复幂等）。
- Shutdown 的 final sweep 只跑 retention，不派发新压缩（避免停机窗口拉长）。
- 用户可见语义变化（查询窗口 30d→7d）必须同步 `README.md`、`config.yaml`、`docs/web-api.md`、`docs/architecture/fusion-shadow-cache.md`（红线 6）。
- 提交信息中文、conventional 前缀，风格对齐 `git log --oneline -5`。
- 每任务收尾至少 `gofmt -l <改动的包路径>` + `go vet ./internal/...` + 本包 `go test -count=1`；Task 6 跑全量矩阵。

---

### Task 1: logfile — zstd 归档核心（依赖、原子压缩、候选派发、worker 生命周期）

**Files:**
- Modify: `go.mod`、`go.sum`（`go get github.com/klauspost/compress`）
- Create: `internal/observe/logfile/archive.go`
- Create: `internal/observe/logfile/archive_test.go`
- Modify: `internal/observe/logfile/logfile.go`（Options、Logger struct、New、Run、sweepAndGuard）

**Interfaces:**
- Produces: `archiveZstd(src string) error`、`isZstdFile(name, prefix string) bool`、`archiveDay(name, prefix string) (time.Time, bool)`（包内）、`Options.HotRetention time.Duration`、`Logger.HotRetention() time.Duration`、`Logger.dispatchArchive(now, activePath)`、`Logger.archiveWorker()`、`Logger.archive chan string`、`Logger.archiveDone chan struct{}`、`Logger.archiveFn func(string) error`（测试 seam，生产 `archiveZstd`）。Task 2/4 依赖这些名字。

- [ ] **Step 1: 加依赖**

```bash
cd /Users/neesenk/model-proxy && go get github.com/klauspost/compress && go mod tidy
```

Expected: go.mod require 块出现 `github.com/klauspost/compress`；`go build ./...` 通过。

- [ ] **Step 2: 写失败测试（archive_test.go）**

创建 `internal/observe/logfile/archive_test.go`，包声明 `package logfile`，imports 含 `errors`、`io`、`os`、`path/filepath`、`strings`、`sync/atomic`、`testing`、`time`、`github.com/klauspost/compress/zstd`：

```go
func TestArchiveZstdRoundTripAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "requests-20260101.log")
	body := strings.Repeat("{\"ts\":\"2026-01-01T00:00:00Z\"}\n", 1000)
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := archiveZstd(src); err != nil {
		t.Fatalf("archiveZstd: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("plain file still present: %v", err)
	}
	if _, err := os.Stat(src + ".zst.tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp leftover after success")
	}
	f, err := os.Open(src + ".zst")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode = %v, %v; want 0600", info, err)
	}
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("round trip mismatch: %d bytes vs %d", len(got), len(body))
	}
}

func TestArchiveZstdIdempotentAfterCrashBetweenRenameAndRemove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "requests-20260101.log")
	if err := os.WriteFile(src, []byte("plain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src+".zst", []byte("already compressed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := archiveZstd(src); err != nil {
		t.Fatalf("archiveZstd: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("plain file kept after idempotent rerun")
	}
	b, err := os.ReadFile(src + ".zst")
	if err != nil || string(b) != "already compressed" {
		t.Fatalf("existing archive must be untouched: %q, %v", b, err)
	}
}

func TestDispatchArchiveOffersOnlyColdRotatedFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	active := write("requests-20261122.log", 0)
	cold := write("requests-20261115.log", 8*24*time.Hour)
	write("requests-20261121.log", 24*time.Hour)      // inside hot window
	if err := os.WriteFile(filepath.Join(dir, "requests-20261114.log.zst"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(Options{Directory: dir, FilePrefix: "requests-", HotRetention: 7 * 24 * time.Hour})
	l.dispatchArchive(now, active)
	select {
	case got := <-l.archive:
		if got != cold {
			t.Fatalf("dispatched %s, want %s", got, cold)
		}
	default:
		t.Fatal("expected the cold rotated file to be dispatched")
	}
	select {
	case extra := <-l.archive:
		t.Fatalf("dispatched extra %s", extra)
	default:
	}
}

func TestDispatchArchiveDisabledWhenHotRetentionZero(t *testing.T) {
	l := New(Options{Directory: t.TempDir(), HotRetention: 0})
	l.dispatchArchive(time.Now(), "")
	select {
	case p := <-l.archive:
		t.Fatalf("dispatched %s with archiving disabled", p)
	default:
	}
}

func TestArchiveWorkerFiresOnChangePerSuccessAndSurvivesErrors(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	if err := os.WriteFile(a, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(Options{Directory: dir, Tag: "t"})
	var calls atomic.Int32
	l.archiveFn = func(p string) error {
		if calls.Add(1) == 1 {
			return errors.New("boom")
		}
		return os.Remove(p)
	}
	changes := make(chan struct{}, 2)
	l.opts.OnChange = func() { changes <- struct{}{} }
	go func() {
		l.archive <- a
		l.archive <- b
		close(l.archive)
	}()
	l.archiveWorker() // returns when the queue closes: worker drained both
	if _, err := os.Stat(a); err != nil {
		t.Fatalf("failed archive must keep the plain file: %v", err)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatalf("successful archive must remove the plain file: %v", err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("OnChange not fired for the successful archive")
	}
}

func TestRunShutdownWaitsForInFlightArchive(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "requests-20260101.log")
	if err := os.WriteFile(src, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(Options{Directory: dir, FilePrefix: "requests-", Retention: time.Hour, HotRetention: time.Hour})
	started := make(chan struct{})
	release := make(chan struct{})
	l.archiveFn = func(p string) error {
		close(started)
		<-release
		return os.Rename(p, p+".done")
	}
	go l.Run()
	l.archive <- src
	<-started // the worker is mid-archive
	shutdownReturned := make(chan struct{})
	go func() {
		l.Shutdown()
		close(shutdownReturned)
	}()
	close(release) // the in-flight archive may now finish
	<-shutdownReturned
	// Shutdown must have waited for the final flush: the rename is visible
	// by the time it returns.
	if _, err := os.Stat(src + ".done"); err != nil {
		t.Fatalf("Shutdown returned before the in-flight archive finished: %v", err)
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

```bash
go test ./internal/observe/logfile/ -run 'ArchiveZstd|DispatchArchive|ArchiveWorker|ShutdownWaitsForInFlight' -count=1
```

Expected: 编译失败——`archiveZstd`/`HotRetention`/`dispatchArchive`/`archiveWorker`/`archive`/`archiveFn` 未定义。

- [ ] **Step 4: 实现 archive.go**

创建 `internal/observe/logfile/archive.go`：

```go
package logfile

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"model-proxy/internal/observe/logx"
)

// zstdSuffix marks archived rotated files: <prefix>*.log.zst. Every query
// path matches the exact .log suffix, so archives are invisible to the
// indexer, the scan fallback and the queries by construction — offline-only
// data (zstd -d | jq), never an online read path.
const zstdSuffix = ".zst"

// archiveQueueDepth bounds how many cold files one sweep tick may hand to
// the worker; a fuller queue retries on the next sweep.
const archiveQueueDepth = 16

func isZstdFile(name, prefix string) bool {
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, zstdSuffix)
}

// archiveDay extracts the day stamp (YYYYMMDD) a managed file's name
// carries. A .zst's mtime is compression time, not content age, so every
// age check on archives reads the name instead.
func archiveDay(name, prefix string) (time.Time, bool) {
	rest := name[len(prefix):]
	if len(rest) < 8 {
		return time.Time{}, false
	}
	day, err := time.Parse(dayFileLayout, rest[:8])
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

// archiveZstd compresses src (a rotated .log) to src+".zst" atomically:
// write src+".zst.tmp" → rename to ".zst" → remove src. It is idempotent
// across crashes: when the ".zst" already exists (a crash between rename
// and remove), only src is removed; a leftover ".tmp" is replaced
// (O_TRUNC) by the next run.
func archiveZstd(src string) (err error) {
	dst := src + zstdSuffix
	if _, statErr := os.Stat(dst); statErr == nil {
		return os.Remove(src)
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, logFileMode)
	if err != nil {
		return err
	}
	enc, err := zstd.NewWriter(out)
	if err != nil {
		_ = out.Close()
		return err
	}
	if _, err = io.Copy(enc, in); err != nil {
		_ = enc.Close()
		_ = out.Close()
		return err
	}
	if err = enc.Close(); err != nil {
		_ = out.Close()
		return err
	}
	if err = out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// dispatchArchive offers rotated .log files whose mtime fell out of the hot
// window to the archive worker — non-blocking; a full queue retries on the
// next sweep. It runs on the writer goroutine AFTER the retention sweep, so
// files already past total retention are deleted rather than compressed.
// The active file and non-.log entries are never offered.
func (l *Logger) dispatchArchive(now time.Time, activePath string) {
	if l.opts.HotRetention <= 0 {
		return
	}
	cutoff := now.Add(-l.opts.HotRetention)
	entries, err := os.ReadDir(l.opts.Directory)
	if err != nil {
		logx.Warnf("[%s] archive readdir %s: %v", l.opts.Tag, l.opts.Directory, err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !isLogFile(name, l.opts.FilePrefix) {
			continue
		}
		path := filepath.Join(l.opts.Directory, name)
		if activePath != "" && path == activePath {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		select {
		case l.archive <- path:
		default:
			logx.Warnf("[%s] archive queue full; %s retried next sweep", l.opts.Tag, name)
			return
		}
	}
}

// archiveWorker drains the archive queue off the writer goroutine —
// compressing a rotated file takes seconds and must never stall record
// writes. Each finished archive fires OnChange so the tailing index sees
// the vanished plain file on its next pass. Run's exit path closes the
// queue and waits on archiveDone: the final flush.
func (l *Logger) archiveWorker() {
	defer close(l.archiveDone)
	for src := range l.archive {
		if err := l.archiveFn(src); err != nil {
			logx.Warnf("[%s] archive %s failed (kept plain; retried next sweep): %v",
				l.opts.Tag, filepath.Base(src), err)
			continue
		}
		l.changed()
	}
}

// HotRetention returns the configured hot window (0 = archiving disabled).
func (l *Logger) HotRetention() time.Duration {
	if l == nil {
		return 0
	}
	return l.opts.HotRetention
}
```

- [ ] **Step 5: 修改 logfile.go（Options/Logger/New/Run/sweepAndGuard）**

`Options` 在 `OnChange func()` 字段后追加：

```go
	// HotRetention, when > 0, zstd-archives rotated files whose mtime fell
	// out of the window: the sweep tick hands them to a side goroutine that
	// compresses them to <name>.zst (atomic tmp+rename) and removes the
	// plain file. Archived files are offline-only: no query path matches
	// the .zst suffix. 0 keeps every file plain for its whole retention.
	HotRetention time.Duration
```

`Logger` struct 在 `line []byte` 后追加：

```go
	// archive hands cold rotated files to the archive worker; archiveDone
	// closes when the worker exits. Run's exit path closes the queue and
	// waits on archiveDone before reporting closed — the final flush.
	archive     chan string
	archiveDone chan struct{}
	// archiveFn is the compression seam; production uses archiveZstd.
	archiveFn func(src string) error
```

`New()` 的 return 前追加初始化（`freeSpace: volumeFreeSpace,` 之后）：

```go
	return &Logger{
		opts:         opts,
		encodes:      make(chan EncodeFunc, opts.QueueCapacity),
		done:         make(chan struct{}),
		closed:       make(chan struct{}),
		freeSpace:    volumeFreeSpace,
		archive:      make(chan string, archiveQueueDepth),
		archiveDone:  make(chan struct{}),
		archiveFn:    archiveZstd,
	}
```

`Run()`：在 `writer := NewWriter(...)` / `defer writer.Close()` 之后、`ticker :=` 之前插入：

```go
	// The archive worker owns compression I/O off the writer goroutine;
	// only started when a hot window is configured (EnsureDir already
	// succeeded above, so the worker's directory exists).
	archiveStarted := false
	if l.opts.HotRetention > 0 {
		archiveStarted = true
		go l.archiveWorker()
	}
```

`Run()` 的 `<-l.done:` 分支 `default:` 内，`l.sweep(time.Now(), writer.Path()); l.changed()` 之后、`return` 之前插入（final sweep 不派发新压缩，只等在途的收尾）：

```go
				if archiveStarted {
					close(l.archive)
					<-l.archiveDone
				}
```

`sweepAndGuard` 替换为（guard 之后派发；changed 保持最后）：

```go
// sweepAndGuard runs one retention pass, then the disk-space guard, then
// the hot-window archive dispatch on the same tick. Dispatch runs last so
// files past total retention are deleted by the sweep instead of being
// compressed pointlessly; a burst day can outrun any age window, and the
// guard's oldest-first deletion is the floor retention cannot provide.
func (l *Logger) sweepAndGuard(now time.Time, activePath string) {
	l.sweep(now, activePath)
	l.guard(activePath)
	l.dispatchArchive(now, activePath)
	l.changed()
}
```

- [ ] **Step 6: 跑测试确认通过**

```bash
go test ./internal/observe/logfile/ -count=1
go vet ./internal/observe/logfile/
```

Expected: 全部 PASS（含既有测试——Run 的改动向后兼容，HotRetention=0 时行为与现状一致）。

- [ ] **Step 7: 提交**

```bash
git add go.mod go.sum internal/observe/logfile/
git commit -m "feat(logfile): 请求日志 zstd 冷层核心——热窗候选派发、旁路压缩 worker 与原子归档"
```

---

### Task 2: logfile — retention / disk-guard / Prune 可删集合纳入 .zst

**Files:**
- Modify: `internal/observe/logfile/logfile.go`（sweep、rotatedFiles、Prunable）
- Test: `internal/observe/logfile/archive_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `isZstdFile`、`archiveDay`。
- Produces: sweep 删除过期 `.zst`（按文件名日戳）；`rotatedFiles`/`Prunable` 列出 `.zst`（guard 与 `internal/cli/prune` 的删除候选）。

- [ ] **Step 1: 写失败测试（archive_test.go 追加）**

```go
func TestSweepDeletesExpiredZstdByNameDateNotMtime(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	// Name-day old, mtime fresh (compressed recently): must be deleted.
	expired := filepath.Join(dir, "requests-20261020.log.zst")
	if err := os.WriteFile(expired, []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(expired, now, now); err != nil {
		t.Fatal(err)
	}
	// Name-day young, mtime ancient: must be kept.
	fresh := filepath.Join(dir, "requests-20261121.log.zst")
	if err := os.WriteFile(fresh, []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fresh, now.Add(-100*24*time.Hour), now.Add(-100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	l := New(Options{Directory: dir, FilePrefix: "requests-", Retention: 30 * 24 * time.Hour})
	l.sweep(now, "")
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired .zst kept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh .zst deleted: %v", err)
	}
}

func TestGuardDeletesZstdOldestFirst(t *testing.T) {
	dir := t.TempDir()
	young := filepath.Join(dir, "requests-20261121.log")
	oldZ := filepath.Join(dir, "requests-20261101.log.zst")
	for _, p := range []string{young, oldZ} {
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l := New(Options{
		Directory: dir, FilePrefix: "requests-",
		MinFreeBytes: 4, MaxUsedPercent: 0,
	})
	// free=0 < 4 triggers; the guard deletes oldest-first by name, so the
	// 20261101 .zst goes before the 20261121 .log, and the 4 freed bytes
	// project free to 4 — no longer below the floor — stopping the pass.
	l.freeSpace = func(string) (free, total int64, err error) {
		return 0, 0, nil
	}
	l.guard(filepath.Join(dir, "requests-20261122.log"))
	if _, err := os.Stat(oldZ); !os.IsNotExist(err) {
		t.Fatalf("guard did not delete the oldest .zst: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatalf("guard deleted past the trigger: %v", err)
	}
}

func TestPrunableListsZstdByArchiveDay(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	z := filepath.Join(dir, "requests-20261020.log.zst")
	if err := os.WriteFile(z, []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(z, now, now); err != nil { // fresh mtime, old name-day
		t.Fatal(err)
	}
	logf := filepath.Join(dir, "requests-20261121.log")
	if err := os.WriteFile(logf, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	targets, err := Prunable(dir, "requests-", now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Name != "requests-20261020.log.zst" {
		t.Fatalf("targets = %+v, want only the old .zst", targets)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/observe/logfile/ -run 'SweepDeletesExpiredZstd|GuardDeletesZstd|PrunableListsZstd' -count=1
```

Expected: FAIL——sweep/guard/Prunable 目前都过滤掉 `.zst`。

- [ ] **Step 3: 实现**

`sweep` 的循环体替换为（cutoff 之前的部分不动）：

```go
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(l.opts.Directory, name)
		if activePath != "" && path == activePath {
			continue
		}
		var expired bool
		switch {
		case isLogFile(name, l.opts.FilePrefix):
			info, err := entry.Info()
			if err != nil {
				continue
			}
			expired = info.ModTime().Before(cutoff)
		case isZstdFile(name, l.opts.FilePrefix):
			// A .zst's mtime is compression time, not content age.
			if day, ok := archiveDay(name, l.opts.FilePrefix); ok {
				expired = day.Before(cutoff)
			}
		default:
			continue
		}
		if expired {
			if err := os.Remove(path); err != nil {
				logx.Warnf("[%s] sweep remove %s: %v", l.opts.Tag, name, err)
			}
		}
	}
```

`rotatedFiles` 的过滤行 `if entry.IsDir() || !isLogFile(name, filePrefix) || name == activeName {` 替换为：

```go
		if entry.IsDir() || !(isLogFile(name, filePrefix) || isZstdFile(name, filePrefix)) || name == activeName {
```

并把 doc comment 中 `prefix*.log` 的描述改为 `prefix*.log and prefix*.log.zst`。

`Prunable` 的过滤行同上替换，mtime 判断改为对 `.zst` 用 `archiveDay`：

```go
		var aged bool
		switch {
		case isLogFile(name, filePrefix):
			info, err := entry.Info()
			if err != nil {
				continue
			}
			aged = info.ModTime().Before(cutoff)
		case isZstdFile(name, filePrefix):
			if day, ok := archiveDay(name, filePrefix); ok {
				aged = day.Before(cutoff)
			}
		default:
			continue
		}
		if !aged {
			continue
		}
```

（后续 `entry.Info()` 取 Size 的既有代码保留。）

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/observe/logfile/ ./internal/cli/prune/ -count=1
```

Expected: PASS（`internal/cli/prune` 既有测试不回归——Prunable 扩展对纯 `.log` 目录是超集不变）。

- [ ] **Step 5: 提交**

```bash
git add internal/observe/logfile/logfile.go internal/observe/logfile/archive_test.go
git commit -m "feat(logfile): retention/disk-guard/prune 可删集合纳入 .zst（时龄取文件名日戳）"
```

---

### Task 3: config — `request_log.hot_retention` 字段与 fail-closed 校验

**Files:**
- Modify: `internal/config/config.go`（RequestLogConfig、HotRetentionDuration、LoadConfigFromBytes 校验）
- Modify: `internal/config/default_template.go`（模板注释行）
- Test: `internal/config/config_test.go`（追加）

**Interfaces:**
- Produces: `RequestLogConfig.HotRetention string`（yaml `hot_retention`）、`RequestLogConfig.HotRetentionDuration() time.Duration`（缺省 168h，`"0"`=0，解析失败回落 168h）。Task 4 依赖 `HotRetentionDuration()`。

- [ ] **Step 1: 写失败测试（config_test.go 追加，用既有 LoadConfigFromBytes 模式）**

```go
func TestRequestLogHotRetentionDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", 168 * time.Hour},             // default 7d
		{"0", 0},                          // disable archiving
		{"48h", 48 * time.Hour},           // explicit
		{"not-a-duration", 168 * time.Hour}, // malformed falls back
	}
	for _, tc := range cases {
		got := RequestLogConfig{HotRetention: tc.raw}.HotRetentionDuration()
		if got != tc.want {
			t.Errorf("HotRetentionDuration(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestLoadRejectsHotRetentionNotInsideRetention(t *testing.T) {
	base := "request_log:\n  enabled: true\n  retention: %s\n  hot_retention: %s\n"
	cases := []struct {
		retention, hot, wantErr string
	}{
		{"720h", "168h", ""},           // ok: hot < retention
		{"720h", "720h", "must be < request_log.retention"},
		{"720h", "1000h", "must be < request_log.retention"},
		{"48h", "168h", "must be < request_log.retention"}, // default hot outside a short retention
		{"0", "168h", ""},             // keep-forever retention: any hot ok
		{"720h", "0", ""},             // archiving disabled: always ok
	}
	for _, tc := range cases {
		_, err := LoadConfigFromBytes("config.yaml", []byte(fmt.Sprintf(base, tc.retention, tc.hot)))
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("retention=%s hot=%s: unexpected error %v", tc.retention, tc.hot, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("retention=%s hot=%s: error = %v, want containing %q", tc.retention, tc.hot, err, tc.wantErr)
		}
	}
}
```

（注意 `{"48h", "168h", ...}` 一行依赖"hot 未显式配置时缺省 168h"也参与校验——这是设计决定：fail-closed 优先于隐式降级，错误信息指引用户显式设置。）

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/config/ -run 'HotRetention' -count=1
```

Expected: 编译失败——`HotRetention` 字段不存在。

- [ ] **Step 3: 实现**

`RequestLogConfig` 的 `Retention string` 字段后追加：

```go
	// HotRetention is the hot window after which rotated files are
	// zstd-archived (requests-*.log.zst, offline-only: indexed queries and
	// the Requests page cover hot files only; decompress with
	// `zstd -d < file.zst | jq`). Default "168h" (7 days); "0" keeps every
	// file plain for its whole retention. Must be < retention (or retention
	// "0" = keep forever); restart-only like the rest of the block.
	HotRetention string `yaml:"hot_retention"`
```

`RetentionDuration()` 之后追加：

```go
// HotRetentionDuration returns the hot window after which rotated files are
// zstd-archived, defaulting to 7 days. "0" or a zero duration keeps every
// file plain for its whole retention (archiving disabled).
func (r RequestLogConfig) HotRetentionDuration() time.Duration {
	if r.HotRetention == "" {
		return 168 * time.Hour
	}
	if d, err := time.ParseDuration(r.HotRetention); err == nil {
		return d
	}
	return 168 * time.Hour
}
```

`LoadConfigFromBytes` 中 `cfg.RequestLog = raw.RequestLog`（config.go:1567 附近）之后追加：

```go
	// The hot window must sit strictly inside total retention: an archive
	// transition at/after the retention cutoff would compress files the
	// retention sweep is about to delete. hot_retention "0" (archiving
	// disabled) is always valid; retention "0" (keep forever) bounds
	// nothing.
	if hot := cfg.RequestLog.HotRetentionDuration(); hot > 0 {
		if retention := cfg.RequestLog.RetentionDuration(); retention > 0 && hot >= retention {
			return nil, fmt.Errorf(
				"request_log.hot_retention (%s) must be < request_log.retention (%s); use \"0\" to disable archiving",
				cfg.RequestLog.HotRetention, cfg.RequestLog.Retention)
		}
	}
```

`default_template.go` 的 request_log 注释块（`#   # aggregate_streams: true` 附近）追加注释行：

```yaml
  #   hot_retention: 168h
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/config/ -count=1
go vet ./internal/config/
```

- [ ] **Step 5: 提交**

```bash
git add internal/config/
git commit -m "feat(config): request_log.hot_retention 热窗配置与 fail-closed 校验"
```

---

### Task 4: requestlog + app — 热窗接线（两条流共享）与启用行广播

**Files:**
- Modify: `internal/observe/requestlog/logger.go`（Options 字段、New 传递、accessor）
- Modify: `internal/app/observe_adapters.go`（两条流的 Options + 启用日志行）
- Test: `internal/observe/requestlog/logger_test.go`（若无则建，追加）、`internal/app/request_log_test.go`（追加）

**Interfaces:**
- Consumes: Task 3 的 `HotRetentionDuration()`、Task 1 的 `logfile.Options.HotRetention`、`Logger.HotRetention()`。
- Produces: `requestlog.Options.HotRetention time.Duration`、`requestlog.Logger.HotRetention() time.Duration`。

- [ ] **Step 1: 写失败测试**

`internal/observe/requestlog/logger_test.go`（不存在则创建，`package requestlog`）：

```go
func TestLoggerPassesHotRetentionToSink(t *testing.T) {
	l := New(Options{Directory: t.TempDir(), HotRetention: 48 * time.Hour})
	if got := l.HotRetention(); got != 48*time.Hour {
		t.Fatalf("HotRetention() = %v, want 48h", got)
	}
	if got := New(Options{Directory: t.TempDir()}).HotRetention(); got != 0 {
		t.Fatalf("default HotRetention() = %v, want 0 (disabled)", got)
	}
}
```

`internal/app/request_log_test.go` 追加（镜像 `TestReload_WarnsWhenRequestLogEnabledButInactive` 的 Proxy 构造方式，按该测试实际的构造辅助函数调整——目标断言不变）：

```go
func TestInitRequestLogWiresHotRetention(t *testing.T) {
	cfg := configdomain.Defaults() // 或既有测试的 config 构造辅助
	cfg.RequestLog.Enabled = true
	cfg.RequestLog.Retention = "720h"
	cfg.RequestLog.HotRetention = "168h"
	p := newTestProxy(t) // 既有辅助；无则按邻近测试构造 &Proxy{}
	p.initRequestLog(cfg)
	if got := p.reqLog.HotRetention(); got != 168*time.Hour {
		t.Fatalf("reqLog hot retention = %v, want 168h", got)
	}
	if cfg.RequestLog.MCPSplit {
		if got := p.mcpReqLog.HotRetention(); got != 168*time.Hour {
			t.Fatalf("mcpReqLog hot retention = %v, want 168h", got)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/observe/requestlog/ ./internal/app/ -run 'HotRetention' -count=1
```

Expected: 编译失败——字段/方法不存在。

- [ ] **Step 3: 实现**

`requestlog.Options` 在 `AggregateStream` 字段后追加：

```go
	// HotRetention, when > 0, zstd-archives rotated files older than the
	// window (see logfile.Options.HotRetention): indexed queries, the scan
	// fallback and the Requests page then cover hot files only — archives
	// are offline analysis data. 0 keeps all files plain. Restart-only.
	HotRetention time.Duration
```

`requestlog.New` 的 `logfile.New(logfile.Options{...})` 字面量追加 `HotRetention: options.HotRetention,`（与 `Retention:` 同款位置）。

`logger.go` 末尾追加 accessor：

```go
// HotRetention returns the configured hot window (0 = archiving disabled).
func (l *Logger) HotRetention() time.Duration {
	if l == nil {
		return 0
	}
	return l.sink.HotRetention()
}
```

`observe_adapters.go` `initRequestLog`：两条流的 `requestlog.New(requestlog.Options{...})` 各追加一行 `HotRetention: config.HotRetentionDuration(),`；启用日志行（`retention %s%s%s` 格式）追加热窗注记——在 `guardNote` 定义后追加：

```go
	hotNote := ""
	if hot := config.HotRetentionDuration(); hot > 0 {
		hotNote = fmt.Sprintf(", hot window %s (older rotated files zstd-archived, offline-only)", hot)
	}
```

`logx.Infof` 的格式串从 `retention %s%s)` 改为 `retention %s%s%s)`，实参依次 `guardNote, hotNote`。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/observe/requestlog/ ./internal/app/ -count=1
```

- [ ] **Step 5: 提交**

```bash
git add internal/observe/requestlog/logger.go internal/observe/requestlog/logger_test.go internal/app/
git commit -m "feat(requestlog,app): 热窗接线（两条流共享）与启用行广播"
```

---

### Task 5: requestlog — 锁定测试：.zst 对索引/查询不可见、归档后清行

**Files:**
- Test: `internal/observe/requestlog/index_files_test.go`（追加）
- Test: `internal/observe/requestlog/query_test.go`（若扫描测试在别的文件，追加到最近的 Query* 测试文件）

**Interfaces:**
- Consumes: 既有测试辅助 `appendRecordLines`、`newTestIndexer`、`mustReconcile`、`assertIndexedFiles`（index_test.go:45/70/80、index_files_test.go:50）、导出 API `QueryRecordsIn`（query.go:144）、`Indexer.Detail`。

- [ ] **Step 1: 写测试（这些是锁定既有行为的契约测试，直接写、直接通过；若红即说明实现前提被打破，停下来按 systematic-debugging 处理）**

`index_files_test.go` 追加：

```go
// TestReconcileDropsRowsWhenPlainFileZstdArchived: the archive transition
// writes <name>.zst and removes the plain .log. The indexer must never
// claim the .zst (suffix contract) and must evict the vanished plain
// file's rows — the existing unclaimed-file deletion path — leaving the
// archived ids a graceful Detail miss, not an error.
func TestReconcileDropsRowsWhenPlainFileZstdArchived(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "requests-20260730.log")
	appendRecordLines(t, active,
		Record{Ts: "2026-07-30T01:00:00Z", RequestID: "r1", SessionID: "s", Provider: "p", Status: 200},
		Record{Ts: "2026-07-30T01:01:00Z", RequestID: "r2", SessionID: "s", Provider: "p", Status: 200},
	)
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)
	assertIndexedFiles(t, indexer, map[string]string{
		"r1": "requests-20260730.log",
		"r2": "requests-20260730.log",
	})
	if err := os.WriteFile(active+".zst", []byte("compressed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, indexer)
	assertIndexedFiles(t, indexer, map[string]string{})
	if records, err := indexer.Detail("r1"); err != nil || len(records) != 0 {
		t.Fatalf("Detail(r1) after archive = %+v, %v; want graceful empty", records, err)
	}
}
```

query 侧追加（放 `query_test.go` 或该包最近的扫描回退测试文件）：

```go
// TestQueryScanSkipsZstdArchives: the scan fallback matches the exact .log
// suffix — a .zst archive (or its .tmp) is never opened, let alone parsed.
func TestQueryScanSkipsZstdArchives(t *testing.T) {
	dir := t.TempDir()
	appendRecordLines(t, filepath.Join(dir, "requests-20260730.log"),
		Record{Ts: "2026-07-30T01:00:00Z", RequestID: "r1", SessionID: "s", Provider: "p", Status: 200})
	for _, name := range []string{"requests-20260729.log.zst", "requests-20260729.log.zst.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not json, must never be read"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	records, err := QueryRecordsIn(dir, "requests-", Filter{})
	if err != nil {
		t.Fatalf("QueryRecordsIn: %v", err)
	}
	if len(records) != 1 || records[0].RequestID != "r1" {
		t.Fatalf("records = %+v, want exactly r1 from the plain .log", records)
	}
}
```

- [ ] **Step 2: 跑测试**

```bash
go test ./internal/observe/requestlog/ -run 'ZstdArchived|SkipsZstdArchives' -count=1 -v
```

Expected: PASS（锁定既有行为）。FAIL 则停下来诊断——设计前提（三处 `.log` 精确匹配）被打破。

- [ ] **Step 3: 提交**

```bash
git add internal/observe/requestlog/
git commit -m "test(requestlog): 锁定 .zst 对索引/查询不可见与归档后清行"
```

---

### Task 6: docs 同步 + 全量验证

**Files:**
- Modify: `config.yaml`（request_log 块）、`README.md`（request log 章节）、`docs/web-api.md`（可查窗口）、`docs/architecture/fusion-shadow-cache.md`（机制段）

**Interfaces:** 无代码；红线 6 的文档同步。

- [ ] **Step 1: config.yaml** —— `retention: 720h` 行后追加：

```yaml
  # Hot window: rotated files older than this are zstd-archived
  # (<name>.log.zst, offline-only — the Requests page, /api/requests and the
  # index cover hot files only; read an archive with `zstd -d < f.zst | jq`).
  # Must be < retention; "0" disables archiving. Restart-only. Default 168h.
  hot_retention: 168h
```

- [ ] **Step 2: README.md** —— request log 说明处（搜索 `request_log`）追加一条：热窗 7 天外的轮转文件压缩为 `.log.zst` 归档、查询窗口即热窗、离线读取命令。

- [ ] **Step 3: docs/web-api.md** —— `/api/requests`（及 sessions/trace）的覆盖窗口描述改为"热窗（`request_log.hot_retention`，默认 7d）；更早记录为 zstd 离线归档"。

- [ ] **Step 4: docs/architecture/fusion-shadow-cache.md** —— 请求日志机制段（**节拍**段之后）追加一段：

```
**冷层归档**（`request_log.hot_retention`，默认 168h、`0` 禁用、restart-only）：sweep tick 在 retention
删除与磁盘护栏之后，把滚出热窗的轮转 `.log`（mtime 基准）交给 logfile 的旁路压缩 worker（独立
goroutine——压缩耗时不得阻塞写路径；owner/stop/wait/final flush 由 Logger 生命周期承担）。worker 以
`X.log.zst.tmp` → rename `X.log.zst` → 删原文件的原子序列落盘（崩溃后 `.zst` 已在则只删原文件，幂等），
完成即触发 OnChange 让 indexer 走现有「未认领消失文件删行」路径清索引行并按 session 失效缓存。
`.zst` 后缀使 indexer 认领/reconcile/查询扫描三处 `.log` 精确匹配天然不认领——归档是纯离线数据
（`zstd -d | jq`），没有在线读路径，查询与 Requests 页的可查窗口即热窗。retention/disk-guard/prune 的
可删集合包含 `.zst`；`.zst` 时龄取文件名 YYYYMMDD 日戳（其 mtime 是压缩时刻）。
```

- [ ] **Step 5: 全量验证矩阵**

```bash
git diff --check
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
gofmt -l .
scripts/static.sh
scripts/cover.sh
GOOS=linux GOARCH=arm64 go build ./... && GOOS=windows GOARCH=amd64 go build ./...
```

并执行 `docs/engineering/testing.md` 末尾检查清单（本任务修改了 `docs/`）。

- [ ] **Step 6: 提交**

```bash
git add config.yaml README.md docs/
git commit -m "docs: 双窗口 30/7 请求日志留存——README/config/web-api/fusion-shadow-cache 同步"
```

---

## Self-Review 记录

- **Spec coverage**：设计文档的机制（worker/原子性/恢复/生命周期/sweep-guard-prune 扩展/config/接线/锁定测试/docs）逐条对应 Task 1–6；「用户可见语义变化」在 Task 6 文档化。无遗漏。
- **Placeholder**：Task 4 Step 1 的 `newTestProxy`/`configdomain.Defaults` 标注了"按邻近既有测试调整"——其余代码均为完整实现；Task 2 Step 1 对 guard 测试的自由变量问题在任务内显式修正说明。已尽量消除模糊。
- **类型一致性**：`HotRetention time.Duration`（logfile/requestlog Options）、`HotRetention string`（config）+ `HotRetentionDuration()`、`archiveZstd/isZstdFile/archiveDay` 命名在各任务间一致。
