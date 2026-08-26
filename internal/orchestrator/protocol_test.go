package orchestrator

import (
	"errors"
	"testing"
)

func TestParseActions(t *testing.T) {
	valid := ActionsSentinel + `{"actions":[{"type":"bid","bounty":"b0001","price":50}]}`
	tests := []struct {
		name    string
		stdout  string
		wantErr bool
		want    int // len(actions) when no error
	}{
		{name: "empty stdout", stdout: "", wantErr: true},
		{name: "no sentinel", stdout: "thinking...\ndone\n", wantErr: true},
		{name: "bad json after sentinel", stdout: ActionsSentinel + "{nope\n", wantErr: true},
		{name: "valid single line", stdout: valid, want: 1},
		{name: "sentinel amid chatter", stdout: "log a\n" + valid + "\nlog b\n", want: 1},
		{name: "leading whitespace", stdout: "   " + valid + "\n", want: 1},
		{name: "empty actions list", stdout: ActionsSentinel + `{"actions":[]}`, want: 0},
		{name: "last sentinel wins", stdout: ActionsSentinel + `{"actions":[]}` + "\n" + valid + "\n", want: 1},
		{
			// Strictly the last: a broken sentinel line after a valid one is
			// the agent's own fault, not something we paper over.
			name:    "last sentinel wins even when broken",
			stdout:  valid + "\n" + ActionsSentinel + "{broken\n",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acts, err := ParseActions(tc.stdout)
			if tc.wantErr {
				if !errors.Is(err, ErrBadStepOutput) {
					t.Fatalf("err = %v, want ErrBadStepOutput", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(acts) != tc.want {
				t.Fatalf("actions = %+v, want %d of them", acts, tc.want)
			}
		})
	}
}

// Field fidelity: what the agent wrote is what the orchestrator reads.
func TestParseActionsFields(t *testing.T) {
	acts, err := ParseActions(ActionsSentinel + `{"actions":[{"type":"submit","bounty":"b0002","answer":"42"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 {
		t.Fatalf("actions = %+v, want 1", acts)
	}
	if a := acts[0]; a.Type != ActionSubmit || a.Bounty != "b0002" || a.Answer != "42" {
		t.Fatalf("action = %+v, want submit/b0002/42", acts[0])
	}
}
