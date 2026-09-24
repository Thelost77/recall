package adapter

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Thelost77/recall/internal/model"
	_ "modernc.org/sqlite"
)

func TestPiParsesMessagesAndSummary(t *testing.T) {
	path := writeFixture(t, "pi.jsonl", `
{"type":"session","version":3,"id":"pi-session","timestamp":"2026-07-29T10:00:00Z","cwd":"/work/qr"}
{"type":"session_info","id":"name","timestamp":"2026-07-29T10:00:01Z","name":"QR generation"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-07-29T10:00:02Z","message":{"role":"user","content":[{"type":"text","text":"Generate QR codes without assets"}]}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-07-29T10:00:03Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"Use an empty-state dialog."},{"type":"toolCall","name":"read"}]}}
{"type":"compaction","id":"c1","parentId":"a1","timestamp":"2026-07-29T10:00:04Z","summary":"The flow creates unassigned printable codes."}
`)
	parsed := parseOne(t, &Pi{Root: filepath.Dir(path)}, model.Source{Path: path, Key: "source", Harness: "pi"})
	if len(parsed.Sessions) != 1 || parsed.Sessions[0].Name != "QR generation" {
		t.Fatalf("unexpected sessions: %#v", parsed.Sessions)
	}
	if len(parsed.Entries) != 3 {
		t.Fatalf("entry count = %d, want 3", len(parsed.Entries))
	}
	if parsed.Entries[1].Text != "Use an empty-state dialog." {
		t.Fatalf("assistant text = %q", parsed.Entries[1].Text)
	}
}

func TestCodexSalvagesValidLinesAroundMalformedLine(t *testing.T) {
	path := writeFixture(t, "codex.jsonl", `
{"timestamp":"2026-07-01T10:00:00Z","type":"session_meta","payload":{"id":"codex-session","cwd":"/work/qr"}}
{"timestamp":"2026-07-01T10:00:00Z","type":"response_item","payload":{"type":"message","id":"instructions","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\nInjected context"}]}}
{"timestamp":"2026-07-01T10:00:01Z","type":"response_item","payload":{"type":"message","id":"u1","role":"user","content":[{"type":"input_text","text":"Find the empty state"}]}}
{"timestamp":"broken","type" BROKEN}
{"timestamp":"2026-07-01T10:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"duplicate"}}
{"timestamp":"2026-07-01T10:00:03Z","type":"response_item","payload":{"type":"message","id":"a1","role":"assistant","content":[{"type":"output_text","text":"Found it."}]}}
`)
	parsed := parseOne(t, &Codex{Root: filepath.Dir(path)}, model.Source{Path: path, Key: "source", Harness: "codex"})
	if len(parsed.Entries) != 2 {
		t.Fatalf("entry count = %d, want 2", len(parsed.Entries))
	}
	if len(parsed.Warnings) != 1 {
		t.Fatalf("warning count = %d, want 1", len(parsed.Warnings))
	}
}

func TestClaudeExcludesMetadataAndCommandWrappers(t *testing.T) {
	path := writeFixture(t, "claude.jsonl", `
{"type":"user","isMeta":true,"sessionId":"claude-session","uuid":"meta","timestamp":"2026-07-01T10:00:00Z","cwd":"/work","message":{"role":"user","content":"instructions"}}
{"type":"user","sessionId":"claude-session","uuid":"cmd","timestamp":"2026-07-01T10:00:01Z","cwd":"/work","message":{"role":"user","content":"<local-command-stdout>ignored</local-command-stdout>"}}
{"type":"user","sessionId":"claude-session","uuid":"u1","timestamp":"2026-07-01T10:00:02Z","cwd":"/work","message":{"role":"user","content":"Review the QR flow"}}
{"type":"assistant","sessionId":"claude-session","uuid":"a1","parentUuid":"u1","timestamp":"2026-07-01T10:00:03Z","cwd":"/work","message":{"role":"assistant","content":[{"type":"text","text":"The flow is valid."},{"type":"tool_use","name":"read"}]}}
`)
	parsed := parseOne(t, &Claude{Root: filepath.Dir(path)}, model.Source{Path: path, Key: "source", Harness: "claude"})
	if len(parsed.Entries) != 2 {
		t.Fatalf("entry count = %d, want 2", len(parsed.Entries))
	}
	if parsed.Sessions[0].Name != "Review the QR flow" {
		t.Fatalf("session name = %q", parsed.Sessions[0].Name)
	}
}

func TestClaudeParsesMetadataFromCommandOnlySession(t *testing.T) {
	path := writeFixture(t, "claude-command-only.jsonl", `
{"type":"mode","mode":"normal","sessionId":"claude-session"}
{"type":"system","subtype":"local_command","sessionId":"claude-session","uuid":"system-1","timestamp":"2026-07-01T10:00:00Z","cwd":"/work","gitBranch":"main","content":"<command-name>/fast</command-name>"}
`)
	parsed := parseOne(t, &Claude{Root: filepath.Dir(path)}, model.Source{Path: path, Key: "source", Harness: "claude"})
	if len(parsed.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(parsed.Sessions))
	}
	session := parsed.Sessions[0]
	if session.NativeID != "claude-session" || session.CWD != "/work" || session.GitBranch != "main" {
		t.Fatalf("unexpected session metadata: %#v", session)
	}
	if session.StartedAt.IsZero() || !session.StartedAt.Equal(session.UpdatedAt) {
		t.Fatalf("unexpected session bounds: %v to %v", session.StartedAt, session.UpdatedAt)
	}
	if len(parsed.Entries) != 0 {
		t.Fatalf("entry count = %d, want 0", len(parsed.Entries))
	}
}

func TestOpenCodeIndexesOnlyNonSyntheticTextParts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE session(id TEXT PRIMARY KEY,parent_id TEXT,directory TEXT,title TEXT,time_created INTEGER,time_updated INTEGER,agent TEXT,model TEXT)`,
		`CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT)`,
		`CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,session_id TEXT,time_created INTEGER,data TEXT)`,
		`INSERT INTO session VALUES('ses_1','ses_parent','/work/qr','QR work',1000,2000,'build','model')`,
		`INSERT INTO message VALUES('msg_user','ses_1',1100,'{"role":"user"}')`,
		`INSERT INTO message VALUES('msg_assistant','ses_1',1200,'{"role":"assistant","parentID":"msg_user"}')`,
		`INSERT INTO part VALUES('part_user','msg_user','ses_1',1100,'{"type":"text","text":"Generate codes"}')`,
		`INSERT INTO part VALUES('part_synthetic','msg_user','ses_1',1150,'{"type":"text","text":"continue","synthetic":true}')`,
		`INSERT INTO part VALUES('part_tool','msg_assistant','ses_1',1200,'{"type":"tool","state":{"output":"secret"}}')`,
		`INSERT INTO part VALUES('part_assistant','msg_assistant','ses_1',1300,'{"type":"text","text":"Use the dialog"}')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	value := &OpenCode{DBPath: path}
	defer value.Close()
	sources, err := value.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Metadata["parent_id"] != "ses_parent" {
		t.Fatalf("unexpected sources: %#v", sources)
	}
	parsed, err := value.Parse(context.Background(), sources[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Entries) != 2 {
		t.Fatalf("entry count = %d, want 2", len(parsed.Entries))
	}
	if parsed.Entries[1].Text != "Use the dialog" {
		t.Fatalf("assistant text = %q", parsed.Entries[1].Text)
	}
}

func TestGrokIndexesPromptsAndAssistantProse(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "%2Fwork%2Fqr", "grok-session")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	summary := `{
		"info": {"id": "grok-session", "cwd": "/work/qr"},
		"generated_title": "QR generation",
		"created_at": "2026-07-29T10:00:00Z",
		"updated_at": "2026-07-29T10:05:00Z",
		"head_branch": "main",
		"git_remotes": ["https://github.com/example/qr.git"],
		"parent_session_id": "parent-session"
	}`
	if err := os.WriteFile(filepath.Join(sessionDir, "summary.json"), []byte(summary), 0o600); err != nil {
		t.Fatal(err)
	}
	updates := `
{"timestamp":1790016875,"method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"<user_query>Generate QR codes</user_query>"}},"_meta":{"eventId":"user-1","agentTimestampMs":1790016875029}}}
{"timestamp":1790016878,"method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"hidden reasoning"}}}}
{"timestamp":1790016879,"method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"list_dir"}}}
{"timestamp":1790016880,"method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","content":{"type":"text","text":"secret tool output"}}}}
{"timestamp":1790016885,"method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Use an empty-state dialog."}},"_meta":{"eventId":"assistant-1","agentTimestampMs":1790016885000}}}
{"timestamp":"broken","type" BROKEN}
`
	if err := os.WriteFile(filepath.Join(sessionDir, "updates.jsonl"), []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "chat_history.jsonl"), []byte(`{"type":"user","content":"do not index this file"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "prompt_history.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := &Grok{Root: root}
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || !strings.HasSuffix(sources[0].Path, "updates.jsonl") {
		t.Fatalf("unexpected sources: %#v", sources)
	}
	parsed := parseOne(t, adapter, sources[0])
	session := parsed.Sessions[0]
	if session.NativeID != "grok-session" || session.Name != "QR generation" || session.CWD != "/work/qr" {
		t.Fatalf("unexpected session: %#v", session)
	}
	if session.GitBranch != "main" || session.GitRemote != "https://github.com/example/qr.git" || session.ParentID != "parent-session" {
		t.Fatalf("unexpected git metadata: %#v", session)
	}
	if len(parsed.Entries) != 2 {
		t.Fatalf("entry count = %d, want 2", len(parsed.Entries))
	}
	if parsed.Entries[0].Role != "user" || parsed.Entries[0].Text != "Generate QR codes" {
		t.Fatalf("user entry = %#v", parsed.Entries[0])
	}
	if parsed.Entries[1].Text != "Use an empty-state dialog." {
		t.Fatalf("assistant text = %q", parsed.Entries[1].Text)
	}
	if len(parsed.Warnings) != 1 {
		t.Fatalf("warning count = %d, want 1", len(parsed.Warnings))
	}
	wantUserTime := time.UnixMilli(1790016875029).UTC()
	if !parsed.Entries[0].Timestamp.Equal(wantUserTime) {
		t.Fatalf("user timestamp = %s, want %s", parsed.Entries[0].Timestamp, wantUserTime)
	}
}

func TestGrokFallsBackToDirectoryIdentity(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "slug-hash", "dir-session")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "slug-hash", ".cwd"), []byte("/work/from-cwd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	updates := `{"timestamp":1790016875,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Analyze this repo"}}}}` + "\n"
	path := filepath.Join(sessionDir, "updates.jsonl")
	if err := os.WriteFile(path, []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed := parseOne(t, &Grok{Root: root}, model.Source{Path: path, Key: "source", Harness: "grok"})
	session := parsed.Sessions[0]
	if session.NativeID != "dir-session" || session.CWD != "/work/from-cwd" || session.Name != "Analyze this repo" {
		t.Fatalf("unexpected session: %#v", session)
	}
	if len(parsed.Entries) != 1 || parsed.Entries[0].NativeID != "message-1" {
		t.Fatalf("unexpected entries: %#v", parsed.Entries)
	}
	encodedDir := filepath.Join(root, "%2Fwork%2Fencoded", "encoded-session")
	if err := os.MkdirAll(encodedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	encodedPath := filepath.Join(encodedDir, "updates.jsonl")
	if err := os.WriteFile(encodedPath, []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed = parseOne(t, &Grok{Root: root}, model.Source{Path: encodedPath, Key: "source", Harness: "grok"})
	if parsed.Sessions[0].CWD != "/work/encoded" || parsed.Sessions[0].NativeID != "encoded-session" {
		t.Fatalf("unexpected encoded session: %#v", parsed.Sessions[0])
	}
}

func parseOne(t *testing.T, value model.Adapter, source model.Source) model.ParsedSource {
	t.Helper()
	parsed, err := value.Parse(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
