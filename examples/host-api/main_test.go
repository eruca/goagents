package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eruca/goagents/hostkit"
)

func TestLoadHostSettingsDefaultsShutdownTimeout(t *testing.T) {
	settings, err := loadHostSettings(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadHostSettings() error = %v", err)
	}
	if settings.addr != "127.0.0.1:8080" {
		t.Fatalf("addr = %q, want loopback default", settings.addr)
	}
	if settings.shutdownTimeout != 30*time.Second {
		t.Fatalf("shutdown timeout = %v, want 30s", settings.shutdownTimeout)
	}
}

func TestLoadHostSettingsParsesShutdownTimeout(t *testing.T) {
	env := map[string]string{
		"HOST_API_ADDR":                 "127.0.0.1:9090",
		"HOST_API_SHUTDOWN_TIMEOUT":     "45s",
		"HOST_RUNTIME_HOME":             "/runtime",
		"LLMKIT_HOME":                   "/llmkit",
		agentApprovalKeychainServiceEnv: "goagents.host-api.approvals.test",
		agentApprovalKeyIDEnv:           "test-v1",
	}

	settings, err := loadHostSettings(getenvFrom(env))
	if err != nil {
		t.Fatalf("loadHostSettings() error = %v", err)
	}
	if settings.addr != env["HOST_API_ADDR"] ||
		settings.shutdownTimeout != 45*time.Second ||
		settings.runtimeHome != env["HOST_RUNTIME_HOME"] ||
		settings.llmKitHome != env["LLMKIT_HOME"] ||
		settings.agentApprovalKeychainService != env[agentApprovalKeychainServiceEnv] ||
		settings.agentApprovalKeyID != env[agentApprovalKeyIDEnv] {
		t.Fatalf("settings = %#v, want parsed environment values", settings)
	}
}

func TestLoadHostSettingsRejectsInvalidShutdownTimeout(t *testing.T) {
	oidcLoads := 0
	settings, err := loadHostSettings(getenvFrom(map[string]string{
		"HOST_API_SHUTDOWN_TIMEOUT": "not-a-duration",
	}))
	if err == nil {
		_, _ = initializeHostConfig(t.Context(), settings, func(string) string { return "" }, func(
			context.Context,
			func(string) string,
		) (*OIDCApprovalAuthenticator, error) {
			oidcLoads++
			return nil, errors.New("OIDC loader called")
		})
	}
	if err == nil {
		t.Fatal("loadHostSettings() returned nil error")
	}
	if oidcLoads != 0 {
		t.Fatalf("OIDC loader calls = %d, want 0", oidcLoads)
	}
}

func TestLoadHostSettingsRejectsNonPositiveShutdownTimeout(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			oidcLoads := 0
			settings, err := loadHostSettings(getenvFrom(map[string]string{
				"HOST_API_SHUTDOWN_TIMEOUT": value,
			}))
			if err == nil {
				_, _ = initializeHostConfig(t.Context(), settings, func(string) string { return "" }, func(
					context.Context,
					func(string) string,
				) (*OIDCApprovalAuthenticator, error) {
					oidcLoads++
					return nil, errors.New("OIDC loader called")
				})
			}
			if err == nil {
				t.Fatal("loadHostSettings() returned nil error")
			}
			if oidcLoads != 0 {
				t.Fatalf("OIDC loader calls = %d, want 0", oidcLoads)
			}
		})
	}
}

func TestLoadHostSettingsRejectsInvalidKeychainBeforeOIDC(t *testing.T) {
	tests := []struct {
		name    string
		service string
		keyID   string
	}{
		{name: "missing service", keyID: "smoke-v1"},
		{name: "missing key ID", service: "goagents.host-api.approvals.smoke.test"},
		{name: "whitespace service", service: " ", keyID: "smoke-v1"},
		{name: "both whitespace", service: " ", keyID: "\t"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]string{
				agentApprovalKeychainServiceEnv: test.service,
				agentApprovalKeyIDEnv:           test.keyID,
			}
			oidcLoads := 0

			settings, err := loadHostSettings(getenvFrom(env))
			if err == nil {
				_, _ = initializeHostConfig(t.Context(), settings, getenvFrom(env), func(
					context.Context,
					func(string) string,
				) (*OIDCApprovalAuthenticator, error) {
					oidcLoads++
					return nil, errors.New("OIDC loader called")
				})
			}

			if oidcLoads != 0 {
				t.Errorf("OIDC loader calls = %d, want 0", oidcLoads)
			}
			const wantError = "agent approval Keychain service and key ID must be configured together"
			if err == nil || err.Error() != wantError {
				t.Fatalf("loadHostSettings() error = %v, want %q", err, wantError)
			}
		})
	}
}

func TestInitializeHostConfigRejectsInvalidSkillRootBeforeOIDC(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	env := map[string]string{hostAPISkillRootEnv: missing}
	settings, err := loadHostSettings(getenvFrom(env))
	if err != nil {
		t.Fatalf("loadHostSettings() error = %v", err)
	}
	oidcLoads := 0

	_, err = initializeHostConfig(t.Context(), settings, getenvFrom(env), func(
		context.Context,
		func(string) string,
	) (*OIDCApprovalAuthenticator, error) {
		oidcLoads++
		return nil, errors.New("OIDC loader called")
	})

	if oidcLoads != 0 {
		t.Fatalf("OIDC loader calls = %d, want 0", oidcLoads)
	}
	if err == nil || !strings.Contains(err.Error(), hostAPISkillRootEnv) || strings.Contains(err.Error(), missing) {
		t.Fatalf("initializeHostConfig() error = %v, want path-free Skill root error", err)
	}
}

func TestInitializeHostConfigIncludesConfiguredSkillCatalog(t *testing.T) {
	root := t.TempDir()
	writeHostAPISkill(t, root, "workflow-review", "---\nname: workflow-review\ndescription: Review a workflow safely.\n---\n# Instructions\nReview scope and evidence.\n", nil)
	expectedAuthenticator := &OIDCApprovalAuthenticator{}
	env := map[string]string{hostAPISkillRootEnv: root}
	settings, err := loadHostSettings(getenvFrom(env))
	if err != nil {
		t.Fatalf("loadHostSettings() error = %v", err)
	}

	config, err := initializeHostConfig(t.Context(), settings, getenvFrom(env), func(
		context.Context,
		func(string) string,
	) (*OIDCApprovalAuthenticator, error) {
		return expectedAuthenticator, nil
	})
	if err != nil {
		t.Fatalf("initializeHostConfig() error = %v", err)
	}
	if config.ApprovalAuthenticator != expectedAuthenticator || config.SkillCatalog == nil || config.SkillGateContext.OS == "" {
		t.Fatalf("config = %#v, want authenticator and Skill config", config)
	}
	if entries := config.SkillCatalog.List(); len(entries) != 1 || entries[0].Ref.Name != "workflow-review" {
		t.Fatalf("Skill entries = %#v, want workflow-review", entries)
	}
}

func TestInitializeHostConfigBoundsOIDCWithParentContext(t *testing.T) {
	settings, err := loadHostSettings(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadHostSettings() error = %v", err)
	}
	var remaining time.Duration

	_, err = initializeHostConfig(t.Context(), settings, func(string) string { return "" }, func(
		ctx context.Context,
		_ func(string) string,
	) (*OIDCApprovalAuthenticator, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("OIDC loader context has no deadline")
		}
		remaining = time.Until(deadline)
		return &OIDCApprovalAuthenticator{}, nil
	})
	if err != nil {
		t.Fatalf("initializeHostConfig() error = %v", err)
	}
	if remaining <= 0 || remaining > 10*time.Second {
		t.Fatalf("OIDC loader deadline remaining = %v, want within 10s", remaining)
	}
}

func TestRunHostConfigFailureWritesOneJSONLineAndReturns2(t *testing.T) {
	var stderr bytes.Buffer
	oidcLoads := 0
	serverStarts := 0
	deps := hostDependencies{
		getenv: getenvFrom(map[string]string{
			"HOST_API_SHUTDOWN_TIMEOUT": "secret-invalid-duration",
		}),
		loadApprovalAuthenticator: func(context.Context, func(string) string) (*OIDCApprovalAuthenticator, error) {
			oidcLoads++
			return nil, errors.New("OIDC loader should not run")
		},
		newServer: func(Config) (*Server, error) {
			serverStarts++
			return nil, errors.New("server should not start")
		},
		newService: func(*Server, string, io.Writer) hostkit.Service {
			t.Fatal("service factory called after configuration failure")
			return nil
		},
		stdout: io.Discard,
		stderr: &stderr,
	}

	if got := runHostWithDeps(t.Context(), deps); got != 2 {
		t.Fatalf("runHostWithDeps() = %d, want 2", got)
	}
	if oidcLoads != 0 || serverStarts != 0 {
		t.Fatalf("external initialization calls = OIDC %d, server %d; want zero", oidcLoads, serverStarts)
	}
	assertHostExitLine(t, stderr.String(), hostkit.CodeConfigFailed, "host configuration failed")
	if strings.Contains(stderr.String(), "secret-invalid-duration") {
		t.Fatalf("stderr leaked invalid environment value: %q", stderr.String())
	}
}

func TestRunHostNonPositiveShutdownTimeoutRemainsConfigFailure(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			var stderr bytes.Buffer
			deps := successfulHostDependencies(&stderr, newRunHostTestService(nil))
			deps.getenv = getenvFrom(map[string]string{
				"HOST_API_SHUTDOWN_TIMEOUT": value,
			})

			if got := runHostWithDeps(t.Context(), deps); got != 2 {
				t.Fatalf("runHostWithDeps() = %d, want 2", got)
			}
			assertHostExitLine(t, stderr.String(), hostkit.CodeConfigFailed, "host configuration failed")
		})
	}
}

func TestRunHostInitializationFailureWritesOneJSONLineAndReturns2(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*hostDependencies)
	}{
		{
			name: "OIDC loader",
			configure: func(deps *hostDependencies) {
				deps.loadApprovalAuthenticator = func(context.Context, func(string) string) (*OIDCApprovalAuthenticator, error) {
					return nil, errors.New("secret OIDC provider payload")
				}
			},
		},
		{
			name: "server composition",
			configure: func(deps *hostDependencies) {
				deps.newServer = func(Config) (*Server, error) {
					return nil, errors.New("secret provider path and checkpoint")
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			deps := successfulHostDependencies(&stderr, newRunHostTestService(nil))
			test.configure(&deps)

			if got := runHostWithDeps(t.Context(), deps); got != 2 {
				t.Fatalf("runHostWithDeps() = %d, want 2", got)
			}
			assertHostExitLine(t, stderr.String(), hostkit.CodeInitializationFailed, "host initialization failed")
			for _, sensitive := range []string{"secret", "provider", "path", "checkpoint"} {
				if strings.Contains(stderr.String(), sensitive) {
					t.Fatalf("stderr leaked %q: %q", sensitive, stderr.String())
				}
			}
		})
	}
}

func TestRunHostCleanDrainWritesNoErrorAndReturns0(t *testing.T) {
	var stderr bytes.Buffer
	service := newRunHostTestService(nil)
	interrupts := make(chan struct{}, 1)
	interrupts <- struct{}{}
	deps := successfulHostDependencies(&stderr, service)
	deps.interrupts = interrupts

	if got := runHostWithDeps(t.Context(), deps); got != 0 {
		t.Fatalf("runHostWithDeps() = %d, want 0", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if service.drainCalls != 1 || service.closeCalls != 1 {
		t.Fatalf("service calls = drain %d, close %d; want one each", service.drainCalls, service.closeCalls)
	}
}

func TestRunHostInternalFailureWritesOneJSONLineAndReturns1(t *testing.T) {
	var stderr bytes.Buffer
	service := newRunHostTestService(errors.New("secret unclassified runtime failure"))
	deps := successfulHostDependencies(&stderr, service)

	if got := runHostWithDeps(t.Context(), deps); got != 1 {
		t.Fatalf("runHostWithDeps() = %d, want 1", got)
	}
	assertHostExitLine(t, stderr.String(), hostkit.CodeInternalError, "internal error")
	if strings.Contains(stderr.String(), "secret unclassified runtime failure") {
		t.Fatalf("stderr leaked runtime cause: %q", stderr.String())
	}
}

func TestRunHostIgnoresStderrWriteFailure(t *testing.T) {
	service := newRunHostTestService(errors.New("runtime failure"))
	deps := successfulHostDependencies(errorWriter{}, service)

	if got := runHostWithDeps(t.Context(), deps); got != 1 {
		t.Fatalf("runHostWithDeps() = %d, want 1", got)
	}
}

func TestSignalInterruptsDeliversTwoEventsToHostkitAndStops(t *testing.T) {
	signals := make(chan os.Signal, 2)
	stopCalls := 0
	interrupts, stop := bridgeSignalInterrupts(signals, func(got chan<- os.Signal) {
		stopCalls++
		if got != signals {
			t.Fatal("stop received a different signal channel")
		}
	})
	defer stop()
	service := newSignalHostService()
	result := make(chan hostkit.Result, 1)
	go func() {
		result <- hostkit.Run(t.Context(), service, interrupts, hostkit.Options{
			DrainTimeout:   time.Hour,
			CleanupTimeout: time.Second,
		})
	}()

	signals <- os.Interrupt
	select {
	case <-service.drainStarted:
	case <-time.After(time.Second):
		t.Fatal("first signal did not start hostkit drain")
	}
	signals <- syscall.SIGTERM

	select {
	case got := <-result:
		if got.Code() != string(hostkit.CodeShutdownTimeout) {
			t.Fatalf("hostkit result code = %q, want %q", got.Code(), hostkit.CodeShutdownTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("second signal did not force hostkit shutdown")
	}

	stop()
	stop()
	if stopCalls != 1 {
		t.Fatalf("signal stop calls = %d, want 1", stopCalls)
	}

	signals <- os.Interrupt
	select {
	case <-interrupts:
		t.Fatal("signal bridge forwarded an event after stop returned")
	default:
	}
}

func TestOSSignalInterruptsStopIsIdempotent(t *testing.T) {
	_, stop := osSignalInterrupts()
	stop()
	stop()
}

func getenvFrom(env map[string]string) func(string) string {
	return func(key string) string {
		return env[key]
	}
}

func successfulHostDependencies(stderr io.Writer, service hostkit.Service) hostDependencies {
	return hostDependencies{
		getenv: func(string) string { return "" },
		loadApprovalAuthenticator: func(context.Context, func(string) string) (*OIDCApprovalAuthenticator, error) {
			return &OIDCApprovalAuthenticator{}, nil
		},
		newServer: func(Config) (*Server, error) {
			return &Server{}, nil
		},
		newService: func(*Server, string, io.Writer) hostkit.Service {
			return service
		},
		stdout: io.Discard,
		stderr: stderr,
	}
}

func assertHostExitLine(t *testing.T, got string, code hostkit.Code, message string) {
	t.Helper()
	want := `{"level":"error","event":"host_exit","code":"` + string(code) + `","message":"` + message + `"}` + "\n"
	if got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("stderr line count = %d, want 1", strings.Count(got, "\n"))
	}

	var fields map[string]string
	if err := json.Unmarshal([]byte(got), &fields); err != nil {
		t.Fatalf("decode stderr JSON: %v", err)
	}
	if len(fields) != 4 {
		t.Fatalf("stderr JSON fields = %#v, want exactly four keys", fields)
	}
	for _, key := range []string{"level", "event", "code", "message"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("stderr JSON missing %q: %#v", key, fields)
		}
	}
}

type runHostTestService struct {
	startErr   error
	done       chan error
	drainCalls int
	closeCalls int
}

func newRunHostTestService(startErr error) *runHostTestService {
	return &runHostTestService{
		startErr: startErr,
		done:     make(chan error),
	}
}

func (s *runHostTestService) Start(context.Context) error {
	return s.startErr
}

func (s *runHostTestService) Done() <-chan error {
	return s.done
}

func (s *runHostTestService) Drain(context.Context) error {
	s.drainCalls++
	return nil
}

func (s *runHostTestService) ForceStop(context.Context) error {
	return nil
}

func (s *runHostTestService) Close(context.Context) error {
	s.closeCalls++
	return nil
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

type signalHostService struct {
	done         chan error
	drainStarted chan struct{}
}

func newSignalHostService() *signalHostService {
	return &signalHostService{
		done:         make(chan error),
		drainStarted: make(chan struct{}),
	}
}

func (*signalHostService) Start(context.Context) error {
	return nil
}

func (s *signalHostService) Done() <-chan error {
	return s.done
}

func (s *signalHostService) Drain(ctx context.Context) error {
	close(s.drainStarted)
	<-ctx.Done()
	return ctx.Err()
}

func (*signalHostService) ForceStop(context.Context) error {
	return nil
}

func (*signalHostService) Close(context.Context) error {
	return nil
}
