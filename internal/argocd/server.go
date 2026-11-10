package argocd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultServerStartTimeout bounds how long the local API server may take to answer.
const DefaultServerStartTimeout = 60 * time.Second

// Server is the one long-lived `argocd admin dashboard` a run talks to.
//
// `argocd --core` starts a whole API server inside every CLI process: its
// informers list every Application, and it opens its own port-forwards. With
// one process per call that costs ~42 API requests and ~0.6 CPU-seconds a call
// and grows with the square of the app count. This keeps a single in-process
// API server for the run, bound to loopback in our own pod, and every call is
// a thin client of it. It has Argo CD's auth disabled, which is why it must
// never listen on anything but 127.0.0.1.
type Server struct {
	Bin          string
	Kubeconfig   string
	Home         string
	Env          []string // extra environment for the server process
	StartTimeout time.Duration
	Log          *slog.Logger // optional; restarts are logged

	mu     sync.Mutex
	port   int
	gen    uint64
	cmd    *exec.Cmd
	exited chan struct{}
	out    *tailBuffer
}

// Start launches the server and returns once it answers.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.port == 0 {
		port, err := freePort()
		if err != nil {
			return fmt.Errorf("no free local port: %w", err)
		}
		s.port = port
	}
	return s.start(ctx)
}

// Conn is the CLI flags that point a call at the server.
func (s *Server) Conn() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []string{"--server", s.addr(), "--plaintext"}
}

// Generation counts restarts; a caller passes the value it saw to Recover.
func (s *Server) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// Recover restarts the server after a call lost it, for instance when the
// repo-server pod behind its port-forward went away. Workers fail together, so
// only the first caller for a given generation restarts; the rest return at once.
func (s *Server) Recover(ctx context.Context, seen uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != seen {
		return nil
	}
	s.stop()
	s.gen++
	if s.Log != nil {
		s.Log.Warn("local argocd server lost, restarting", "restarts", s.gen)
	}
	return s.start(ctx)
}

// Stop ends the server. Safe to call more than once.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop()
}

func (s *Server) addr() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port)) }

func (s *Server) start(ctx context.Context) error {
	cmd := exec.Command(s.Bin, "admin", "dashboard", "--port", strconv.Itoa(s.port), "--address", "127.0.0.1")
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + s.Home, "KUBECONFIG=" + s.Kubeconfig}, s.Env...)
	out := &tailBuffer{max: 4096}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("argocd admin dashboard did not start: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	s.cmd, s.exited, s.out = cmd, exited, out
	if err := s.waitReady(ctx); err != nil {
		s.stop()
		return err
	}
	return nil
}

func (s *Server) waitReady(ctx context.Context) error {
	timeout := s.StartTimeout
	if timeout == 0 {
		timeout = DefaultServerStartTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := http.Client{Timeout: 2 * time.Second}
	url := "http://" + s.addr() + "/healthz"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if res, err := client.Do(req); err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-s.exited:
			return fmt.Errorf("argocd admin dashboard exited: %s", StderrReason(s.out.Bytes()))
		case <-ctx.Done():
			return fmt.Errorf("argocd admin dashboard not ready: %w: %s", ctx.Err(), StderrReason(s.out.Bytes()))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Server) stop() {
	if s.cmd == nil {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.exited:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.exited
	}
	s.cmd = nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// tailBuffer keeps the last max bytes of the server's log, for start failures.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return []byte(strings.Clone(t.buf.String()))
}
