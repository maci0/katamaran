package migration

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"testing"

	"github.com/maci0/katamaran/internal/qmptest"
)

type recordedQMPCommand struct {
	Execute   string          `json:"execute"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type qmpRecorder struct {
	mu       sync.Mutex
	commands []recordedQMPCommand
}

func (r *qmpRecorder) add(cmd recordedQMPCommand) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cmd.Arguments != nil {
		cmd.Arguments = append(json.RawMessage(nil), cmd.Arguments...)
	}
	r.commands = append(r.commands, cmd)
}

func (r *qmpRecorder) Commands() []recordedQMPCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedQMPCommand, len(r.commands))
	for i, cmd := range r.commands {
		out[i] = cmd
		if cmd.Arguments != nil {
			out[i].Arguments = append(json.RawMessage(nil), cmd.Arguments...)
		}
	}
	return out
}

func startRecordingQMP(t *testing.T, respond func(net.Conn, recordedQMPCommand) string) (string, *qmpRecorder) {
	t.Helper()
	rec := &qmpRecorder{}
	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		writeQMP(t, conn, `{"QMP":{"version":{"qemu":{"micro":0,"minor":2,"major":6}}}}`)

		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var capReq recordedQMPCommand
		if err := json.Unmarshal(line, &capReq); err != nil {
			t.Errorf("unmarshal qmp_capabilities request: %v", err)
			return
		}
		if capReq.Execute != "qmp_capabilities" {
			t.Errorf("handshake execute = %q, want qmp_capabilities", capReq.Execute)
			return
		}
		writeQMP(t, conn, `{"return":{}}`)

		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var cmd recordedQMPCommand
			if err := json.Unmarshal(line, &cmd); err != nil {
				t.Errorf("unmarshal QMP request: %v; raw=%s", err, string(line))
				return
			}
			rec.add(cmd)
			if resp := respond(conn, cmd); resp != "" {
				writeQMP(t, conn, resp)
			}
		}
	})
	return sock, rec
}

func writeQMP(t *testing.T, conn net.Conn, msg string) {
	t.Helper()
	if _, err := conn.Write([]byte(msg + "\n")); err != nil {
		t.Errorf("write QMP response: %v", err)
	}
}

func findRecordedCommand(t *testing.T, commands []recordedQMPCommand, execute string) recordedQMPCommand {
	t.Helper()
	for _, cmd := range commands {
		if cmd.Execute == execute {
			return cmd
		}
	}
	t.Fatalf("missing QMP command %q; got %v", execute, recordedCommandNames(commands))
	return recordedQMPCommand{}
}

func decodeRecordedArgs(t *testing.T, cmd recordedQMPCommand, dst any) {
	t.Helper()
	if len(cmd.Arguments) == 0 {
		t.Fatalf("QMP command %q had no arguments", cmd.Execute)
	}
	if err := json.Unmarshal(cmd.Arguments, dst); err != nil {
		t.Fatalf("unmarshal %s arguments: %v; raw=%s", cmd.Execute, err, string(cmd.Arguments))
	}
}

func assertRecordedSubsequence(t *testing.T, commands []recordedQMPCommand, want []string) {
	t.Helper()
	pos := 0
	for _, cmd := range commands {
		if pos < len(want) && cmd.Execute == want[pos] {
			pos++
		}
	}
	if pos != len(want) {
		t.Fatalf("QMP command order missing subsequence %v; got %v", want, recordedCommandNames(commands))
	}
}

func recordedCommandNames(commands []recordedQMPCommand) []string {
	names := make([]string, len(commands))
	for i, cmd := range commands {
		names[i] = cmd.Execute
	}
	return names
}
