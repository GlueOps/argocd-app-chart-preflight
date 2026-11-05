package argocd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain doubles as a fake `argocd admin dashboard`, so Server is tested
// against a real child process and a real listener.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_DASHBOARD"); mode != "" {
		if mode == "crash" {
			fmt.Fprintln(os.Stderr, "secrets is forbidden")
			os.Exit(20)
		}
		port := os.Args[len(os.Args)-3] // admin dashboard --port P --address A
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		_ = http.ListenAndServe("127.0.0.1:"+port, mux)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeServer(t *testing.T, mode string) *Server {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Bin: bin, Home: t.TempDir(), Env: []string{"FAKE_DASHBOARD=" + mode}, StartTimeout: 10 * time.Second}
	t.Cleanup(s.Stop)
	return s
}

func healthy(t *testing.T, s *Server) bool {
	t.Helper()
	res, err := http.Get("http://" + s.Conn()[1] + "/healthz")
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == http.StatusOK
}

func TestServerStartsListensOnLoopbackAndStops(t *testing.T) {
	s := fakeServer(t, "serve")
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn := s.Conn()
	if len(conn) != 3 || conn[0] != "--server" || !strings.HasPrefix(conn[1], "127.0.0.1:") || conn[2] != "--plaintext" {
		t.Errorf("conn = %v", conn)
	}
	if !healthy(t, s) {
		t.Error("server is not answering")
	}
	s.Stop()
	s.Stop() // idempotent
	if healthy(t, s) {
		t.Error("server still answers after Stop")
	}
}

func TestServerStartFailureCarriesItsOutput(t *testing.T) {
	s := fakeServer(t, "crash")
	if err := s.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "secrets is forbidden") {
		t.Errorf("err = %v", err)
	}
	missing := &Server{Bin: "/nonexistent/argocd", Home: t.TempDir()}
	if err := missing.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Errorf("err = %v", err)
	}
}

func TestRecoverRestartsOncePerGeneration(t *testing.T) {
	s := fakeServer(t, "serve")
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	addr := s.Conn()[1]
	seen := s.Generation()
	var wg sync.WaitGroup
	for range 4 { // workers lose the server together
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Recover(ctx, seen); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if g := s.Generation(); g != seen+1 {
		t.Errorf("generation = %d, want one restart from %d", g, seen)
	}
	if s.Conn()[1] != addr || !healthy(t, s) {
		t.Errorf("server must come back on the same address (%s, now %s)", addr, s.Conn()[1])
	}
}

type fakeRecovery struct {
	gen, recovered atomic.Uint64
	err            error
}

func (f *fakeRecovery) Generation() uint64 { return f.gen.Load() }
func (f *fakeRecovery) Recover(context.Context, uint64) error {
	f.recovered.Add(1)
	f.gen.Add(1)
	return f.err
}

type sequence struct {
	results []Result
	n       int
}

func (s *sequence) Run(context.Context, ...string) Result {
	r := s.results[min(s.n, len(s.results)-1)]
	s.n++
	return r
}

func TestLostConnectionRestartsTheServerAndRetries(t *testing.T) {
	lost := Result{Code: 20, Stderr: []byte(`{"level":"fatal","msg":"rpc error: code = Unavailable desc = connection error"}`)}
	r := &fakeRecovery{}
	seq := &sequence{results: []Result{lost, ok("kind: Deployment")}}
	res := Client{Runner: seq, Recovery: r}.Manifests(context.Background(), "x", nil)
	if res.Code != 0 || r.recovered.Load() != 1 || seq.n != 2 {
		t.Errorf("res=%+v recovered=%d calls=%d", res, r.recovered.Load(), seq.n)
	}

	r = &fakeRecovery{err: fmt.Errorf("argocd admin dashboard exited: no repo-server")}
	seq = &sequence{results: []Result{lost}}
	res = Client{Runner: seq, Recovery: r}.Manifests(context.Background(), "x", nil)
	if res.Code == 0 || seq.n != 1 || !strings.Contains(string(res.Stderr), "no repo-server") {
		t.Errorf("a failed restart must end the call: res=%+v calls=%d", res, seq.n)
	}
}

func TestChartErrorsNeverRestartTheServer(t *testing.T) {
	r := &fakeRecovery{}
	seq := &sequence{results: []Result{{Code: 20, Stderr: []byte("deployment.args: every item must be a string (got map)")}}}
	Client{Runner: seq, Recovery: r}.Manifests(context.Background(), "x", nil)
	if r.recovered.Load() != 0 || seq.n != 1 {
		t.Errorf("recovered=%d calls=%d", r.recovered.Load(), seq.n)
	}
}
