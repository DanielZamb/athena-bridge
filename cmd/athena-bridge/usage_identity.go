package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type usageIdentity struct {
	launcher, agent, model string
	effort                 int
	session                []byte
}

var usageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var usageUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func usageModelName(value string) string {
	if usageName.MatchString(value) && !strings.HasPrefix(value, "sk-") && !strings.HasPrefix(value, "sk_") {
		return value
	}
	return ""
}

// Launcher and agent are independent. Only named signals are inspected, and
// session values are hashed before leaving this function.
func detectUsageIdentity(env func(string) string, terminal bool) usageIdentity {
	on := func(name string) bool {
		value := strings.ToLower(env(name))
		return value != "" && value != "0" && value != "false"
	}
	first := func(names ...string) string {
		for _, name := range names {
			if value := env(name); value != "" {
				return name + ":" + value
			}
		}
		return ""
	}
	i := usageIdentity{agent: "unknown"}
	switch {
	case env("__CFBundleIdentifier") == "com.t3tools.t3code" || env("T3CODE_CLI_PATH") != "" || env("CODEX_INTERNAL_ORIGINATOR_OVERRIDE") == "T3 Code":
		i.launcher = "t3-code"
	case on("GITHUB_ACTIONS"):
		i.launcher = "github-actions"
	case on("GITLAB_CI"):
		i.launcher = "gitlab-ci"
	case on("CI"):
		i.launcher = "ci"
	case terminal:
		i.launcher = "terminal"
	}
	if override := env("ATHENA_BRIDGE_LAUNCHER"); override != "" {
		i.launcher = override
	}
	aiAgent, _, _ := strings.Cut(env("AI_AGENT"), "_")
	switch {
	case usageName.MatchString(aiAgent):
		i.agent = strings.ToLower(aiAgent)
	case on("CLAUDECODE") || env("CLAUDE_CODE_ENTRYPOINT") != "":
		i.agent = "claude-code"
	case env("CODEX_THREAD_ID") != "" || env("CODEX_SESSION_ID") != "" || on("CODEX_CI"):
		i.agent = "codex"
	case on("CURSOR_AGENT"):
		i.agent = "cursor"
	case terminal && !on("GITHUB_ACTIONS") && !on("GITLAB_CI") && !on("CI"):
		i.agent = "human"
	}
	session := ""
	switch i.agent {
	case "claude-code":
		session = first("CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID")
	case "codex":
		session = first("CODEX_THREAD_ID", "CODEX_SESSION_ID")
	case "cursor":
		session = first("CURSOR_TRACE_ID")
	}
	if session == "" {
		if on("GITHUB_ACTIONS") && env("GITHUB_RUN_ID") != "" {
			session = "github:" + env("GITHUB_RUN_ID") + ":" + env("GITHUB_RUN_ATTEMPT") + ":" + env("GITHUB_JOB")
		} else if on("GITLAB_CI") {
			session = first("CI_JOB_ID")
		}
	}
	if override := env("ATHENA_BRIDGE_SESSION"); override != "" {
		session = "override:" + override
	}
	if override := env("ATHENA_BRIDGE_AGENT"); override != "" {
		i.agent = override
	}
	if session != "" {
		i.session = usageHash(i.agent + ":" + session)
	}
	i.model = usageModelName(env("ATHENA_BRIDGE_MODEL"))
	if env("ATHENA_BRIDGE_MODEL") == "" && i.agent == "claude-code" {
		i.model = usageModelName(env("ANTHROPIC_MODEL"))
	}
	effort := env("ATHENA_BRIDGE_EFFORT")
	if effort == "" && i.agent == "claude-code" {
		effort = env("CLAUDE_EFFORT")
	}
	i.effort = usageEffort(effort)
	return i
}

func usageEffort(value string) int {
	return map[string]int{"low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}[strings.ToLower(value)]
}

func usageProjectSlug(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, cwd)
}

func usageSessionModel(ctx context.Context, env func(string) string, agent string) (model, effort string) {
	if ctx.Err() != nil || (agent != "claude-code" && agent != "codex") {
		return "", ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	var path string
	if agent == "claude-code" {
		id := env("CLAUDE_CODE_SESSION_ID")
		if id == "" {
			id = env("CLAUDE_SESSION_ID")
		}
		if !usageUUID.MatchString(id) {
			return "", ""
		}
		base := env("CLAUDE_CONFIG_DIR")
		if base == "" {
			base = filepath.Join(home, ".claude")
		}
		cwd, err := os.Getwd()
		if err != nil {
			return "", ""
		}
		path = filepath.Join(base, "projects", usageProjectSlug(cwd), id+".jsonl")
	} else {
		id := env("CODEX_THREAD_ID")
		if id == "" {
			id = env("CODEX_SESSION_ID")
		}
		if !usageUUID.MatchString(id) {
			return "", ""
		}
		base := env("CODEX_HOME")
		if base == "" {
			base = filepath.Join(home, ".codex")
		}
		base = filepath.Join(base, "sessions")
		remaining := 1024
		// UUIDv7 encodes creation time. Codex uses the local calendar on this
		// machine; check neighboring UTC dates too for timezone changes.
		if id[14] == '7' {
			stamp, _ := hex.DecodeString(strings.ReplaceAll(id[:13], "-", ""))
			var ms int64
			for _, b := range stamp {
				ms = ms<<8 | int64(b)
			}
			date := time.UnixMilli(ms)
			seen := map[string]bool{}
			for _, day := range []time.Time{date.Local(), date.UTC(), date.UTC().AddDate(0, 0, -1), date.UTC().AddDate(0, 0, 1)} {
				name := day.Format("2006/01/02")
				if seen[name] {
					continue
				}
				seen[name] = true
				if path = findUsageRollout(ctx, filepath.Join(base, name), id, 0, &remaining); path != "" {
					break
				}
			}
		} else {
			path = findUsageRollout(ctx, base, id, 0, &remaining)
		}
	}
	return usageTranscriptModel(ctx, path, agent)
}

// Read directory entries in bounded batches, do not follow directory symlinks,
// and stop after 1024 entries or four levels even without a deadline.
func findUsageRollout(ctx context.Context, dir, id string, depth int, remaining *int) string {
	if ctx.Err() != nil || *remaining <= 0 || depth > 3 {
		return ""
	}
	f, err := os.Open(dir)
	if err != nil {
		return ""
	}
	defer f.Close()
	for ctx.Err() == nil && *remaining > 0 {
		entries, err := f.ReadDir(min(64, *remaining))
		for _, entry := range entries {
			if ctx.Err() != nil || *remaining <= 0 {
				return ""
			}
			*remaining--
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if found := findUsageRollout(ctx, path, id, depth+1, remaining); found != "" {
					return found
				}
			} else if strings.HasPrefix(entry.Name(), "rollout-") && strings.HasSuffix(entry.Name(), ".jsonl") && strings.Contains(entry.Name(), id) {
				return path
			}
		}
		if err != nil {
			return ""
		}
	}
	return ""
}

// The last 64 KiB holds the current model for most sessions. A long tool-heavy
// turn can push it out, so fall back to the first record in the opening 256 KiB.
func usageTranscriptModel(ctx context.Context, path, agent string) (model, effort string) {
	if path == "" || ctx.Err() != nil {
		return "", ""
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return "", ""
	}
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return "", ""
	}
	const tailBytes, headBytes = 64 << 10, 256 << 10
	offset := max(0, after.Size()-tailBytes)
	if model, effort = usageScanTranscript(ctx, f, offset, tailBytes, agent, true); model != "" || offset == 0 {
		return model, effort
	}
	return usageScanTranscript(ctx, f, 0, min(headBytes, offset), agent, false)
}

func usageScanTranscript(ctx context.Context, f *os.File, offset, size int64, agent string, latest bool) (model, effort string) {
	data, err := io.ReadAll(io.NewSectionReader(f, offset, size))
	if err != nil || ctx.Err() != nil {
		return "", ""
	}
	if offset > 0 {
		_, data, _ = bytes.Cut(data, []byte{'\n'})
	}
	lines := bytes.Split(data, []byte{'\n'})
	for k := range lines {
		if ctx.Err() != nil {
			return "", ""
		}
		n := k
		if latest {
			n = len(lines) - 1 - k
		}
		// Only model and effort fields can leave the parser. Content, IDs, tokens,
		// paths and reasoning from transcripts never enter the usage record.
		var record struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
			Payload struct {
				Model  string `json:"model"`
				Effort string `json:"effort"`
			} `json:"payload"`
		}
		if json.Unmarshal(lines[n], &record) != nil {
			continue
		}
		if agent == "claude-code" && record.Type == "assistant" {
			model = record.Message.Model
		} else if agent == "codex" && (record.Type == "turn_context" || record.Type == "session_meta") {
			model, effort = record.Payload.Model, record.Payload.Effort
		}
		if value := usageModelName(model); value != "" {
			return value, effort
		}
	}
	return "", ""
}
