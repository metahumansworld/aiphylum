package generators

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/metahunmei/dungeon/internal/bounty"
	"github.com/metahunmei/dungeon/internal/ledger"
	"github.com/metahunmei/dungeon/internal/proxy"
)

var repoRoot = filepath.Join("..", "..")

func needPython(t *testing.T) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	return py
}

func genDir() string { return filepath.Join(repoRoot, "generators") }

// Same seed and tier must yield the same task; different seeds different
// tasks. This is what makes episodes replayable and supply infinite.
func TestScriptDeterminism(t *testing.T) {
	needPython(t)
	for _, s := range Dir(genDir()) {
		t.Run(s.GenName, func(t *testing.T) {
			a, err := s.Generate(7, 2)
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.Generate(7, 2)
			if err != nil {
				t.Fatal(err)
			}
			if a != b {
				t.Fatalf("same seed, different tasks:\n%+v\n%+v", a, b)
			}
			c, err := s.Generate(8, 2)
			if err != nil {
				t.Fatal(err)
			}
			if a.Answer == c.Answer && a.Rubric == c.Rubric && a.Prompt == c.Prompt {
				t.Fatalf("different seeds, same task: %+v", a)
			}
		})
	}
}

// The arith answer must be what a precedence-respecting evaluation of the
// prompt's expression produces — checked with an independent Go evaluator so
// the generator isn't grading its own homework.
func TestArithAnswer(t *testing.T) {
	needPython(t)
	s := Script{GenName: "arith", Path: filepath.Join(genDir(), "arith.py")}
	for _, tc := range []struct{ seed, tier int64 }{{1, 1}, {5, 2}, {9, 3}, {12, 4}} {
		task, err := s.Generate(tc.seed, int(tc.tier))
		if err != nil {
			t.Fatal(err)
		}
		expr := specField(t, task.Prompt, "expr")
		if got := evalExpr(t, expr); got != strings.TrimSpace(task.Answer) {
			t.Fatalf("seed %d tier %d: expr %q evaluates to %s, answer says %s",
				tc.seed, tc.tier, expr, got, task.Answer)
		}
	}
}

// The oracle seam, end to end: the generator's precomputed answer must be
// exactly what an agent gets by running the prescribed chain with the real
// SDK against the real proxy and StubProvider. Any drift in request bytes,
// hashing, or reply formatting across the three implementations fails here.
func TestOracleAnswerMatchesSDK(t *testing.T) {
	py := needPython(t)

	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	if err := l.CreateAccount(ctx, "att", ledger.KindAgent, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "att", 100_000, "test grant", "grant:att"); err != nil {
		t.Fatal(err)
	}

	table := proxy.NewPriceTable()
	table.Set("stub-1", proxy.Price{InputPerTok: 1000, OutputPerTok: 1000})
	px := proxy.New(l, table, &proxy.StubProvider{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	px.Authorize("tok-att", "att")
	srv := httptest.NewServer(px)
	defer srv.Close()

	board := bounty.NewBoard()
	board.RegisterGenerator(Script{GenName: "oracle", Path: filepath.Join(genDir(), "oracle.py")})
	b, err := board.Post("oracle", 7, 3, 0, 30)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(py, filepath.Join("testdata", "oracle_driver.py"))
	cmd.Stdin = strings.NewReader(b.Prompt)
	cmd.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Join(repoRoot, "sdk", "python")+":"+filepath.Join(genDir(), "agents"),
		"DUNGEON_PROXY_URL="+srv.URL,
		"DUNGEON_TOKEN=tok-att",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("driver: %v\nstderr: %s", err, stderr.String())
	}
	answer := strings.TrimSpace(stdout.String())

	if !b.Verify(answer) {
		t.Fatalf("driver's chained answer %q does not verify against the generator's hidden answer (digest %s)",
			answer, b.AnswerDigest())
	}
	if !strings.HasPrefix(answer, "stub:") || len(answer) != len("stub:")+16 {
		t.Fatalf("answer %q does not look like a stub reply", answer)
	}

	// The chain burned real credits through the proxy — the economic loop is
	// exercised, not mocked.
	bal, err := l.Balance(ctx, "att")
	if err != nil {
		t.Fatal(err)
	}
	if bal >= 100_000 {
		t.Fatal("solving the oracle chain burned nothing; the metering loop was bypassed")
	}
}

// --- helpers -------------------------------------------------------------

func specField(t *testing.T, prompt, key string) string {
	t.Helper()
	for _, line := range strings.Split(prompt, "\n") {
		if rest, ok := strings.CutPrefix(line, "spec: "); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(rest), &m); err != nil {
				t.Fatalf("bad spec line %q: %v", rest, err)
			}
			v, ok := m[key].(string)
			if !ok {
				t.Fatalf("spec has no string %q: %v", key, m)
			}
			return v
		}
	}
	t.Fatalf("no spec line in prompt %q", prompt)
	return ""
}

// evalExpr evaluates "a op b op c ..." over + - * with normal precedence.
func evalExpr(t *testing.T, expr string) string {
	t.Helper()
	tokens := strings.Fields(expr)
	if len(tokens)%2 != 1 {
		t.Fatalf("malformed expression %q", expr)
	}
	// First pass: fold * runs; second: sum the terms.
	var terms []int64
	var pending byte = '+'
	for i := 0; i < len(tokens); i += 2 {
		n, err := strconv.ParseInt(tokens[i], 10, 64)
		if err != nil {
			t.Fatalf("bad operand %q in %q: %v", tokens[i], expr, err)
		}
		switch pending {
		case '+':
			terms = append(terms, n)
		case '-':
			terms = append(terms, -n)
		case '*':
			terms[len(terms)-1] *= n
		}
		if i+1 < len(tokens) {
			pending = tokens[i+1][0]
		}
	}
	var sum int64
	for _, v := range terms {
		sum += v
	}
	return strconv.FormatInt(sum, 10)
}

// Suite provenance belongs to the loader that read the suite file, never to a
// generator. A script that labelled its own output imported would wear an
// asterisk it had not earned; one that named a real suite would file a
// generated instance under that suite's record. Both are refused at the seam
// where a script's word becomes a task.
func TestScriptCannotClaimASuite(t *testing.T) {
	py := needPython(t)

	path := filepath.Join(t.TempDir(), "liar.py")
	body := "import json\n" +
		"print(json.dumps({\"prompt\":\"p\",\"answer\":\"a\"," +
		"\"reference_tokens\":10,\"suite\":\"precedence-handbook\"}))\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	s := Script{GenName: "liar", Path: path, Python: py}
	_, err := s.Generate(1, 1)
	if err == nil {
		t.Fatal("a generator claimed a suite and the task was accepted")
	}
	if !strings.Contains(err.Error(), "suite") {
		t.Errorf("err = %v, want it to name the claimed suite as the reason", err)
	}

	// The same script without the claim is a perfectly good generator — the
	// refusal is about provenance, not about the task.
	clean := filepath.Join(t.TempDir(), "honest.py")
	if err := os.WriteFile(clean, []byte(strings.Replace(body,
		",\"suite\":\"precedence-handbook\"", "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Script{GenName: "honest", Path: clean, Python: py}).Generate(1, 1); err != nil {
		t.Fatalf("the same task without the suite claim was refused: %v", err)
	}
}
