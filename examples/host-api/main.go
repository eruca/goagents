package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/eruca/goagents/hostkit"
)

const (
	hostShutdownTimeoutEnv     = "HOST_API_SHUTDOWN_TIMEOUT"
	defaultHostAddr            = "127.0.0.1:8080"
	defaultHostShutdownTimeout = 30 * time.Second
	hostInitializationTimeout  = 10 * time.Second
)

func main() {
	os.Exit(runHost())
}

type hostSettings struct {
	addr                         string
	shutdownTimeout              time.Duration
	runtimeHome                  string
	llmKitHome                   string
	agentApprovalKeychainService string
	agentApprovalKeyID           string
}

func loadHostSettings(getenv func(string) string) (hostSettings, error) {
	settings := hostSettings{
		addr:                         getenv("HOST_API_ADDR"),
		shutdownTimeout:              defaultHostShutdownTimeout,
		runtimeHome:                  getenv("HOST_RUNTIME_HOME"),
		llmKitHome:                   getenv("LLMKIT_HOME"),
		agentApprovalKeychainService: getenv(agentApprovalKeychainServiceEnv),
		agentApprovalKeyID:           getenv(agentApprovalKeyIDEnv),
	}
	if settings.addr == "" {
		settings.addr = defaultHostAddr
	}

	if value := getenv(hostShutdownTimeoutEnv); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return settings, fmt.Errorf("parse %s: %w", hostShutdownTimeoutEnv, err)
		}
		settings.shutdownTimeout = timeout
	}
	if settings.shutdownTimeout <= 0 {
		return settings, fmt.Errorf("%s must be positive", hostShutdownTimeoutEnv)
	}
	if _, err := resolveAgentApprovalKeychainConfig(
		settings.agentApprovalKeychainService,
		settings.agentApprovalKeyID,
	); err != nil {
		return settings, err
	}
	return settings, nil
}

func initializeHostConfig(
	ctx context.Context,
	settings hostSettings,
	getenv func(string) string,
	loadApprovalAuthenticator func(
		context.Context,
		func(string) string,
	) (*OIDCApprovalAuthenticator, error),
) (Config, error) {
	catalog, skillGate, err := loadHostSkillConfig(getenv)
	if err != nil {
		return Config{}, err
	}

	startupCtx, cancel := context.WithTimeout(ctx, hostInitializationTimeout)
	defer cancel()
	approvalAuthenticator, err := loadApprovalAuthenticator(startupCtx, getenv)
	if err != nil {
		return Config{}, err
	}
	return Config{
		RuntimeHome:                  settings.runtimeHome,
		LLMKitHome:                   settings.llmKitHome,
		ApprovalAuthenticator:        approvalAuthenticator,
		AgentApprovalKeychainService: settings.agentApprovalKeychainService,
		AgentApprovalKeyID:           settings.agentApprovalKeyID,
		SkillCatalog:                 catalog,
		SkillGateContext:             skillGate,
	}, nil
}

type hostDependencies struct {
	getenv                    func(string) string
	loadApprovalAuthenticator func(
		context.Context,
		func(string) string,
	) (*OIDCApprovalAuthenticator, error)
	newServer  func(Config) (*Server, error)
	newService func(*Server, string, io.Writer) hostkit.Service
	stdout     io.Writer
	stderr     io.Writer
	interrupts <-chan struct{}
}

func runHost() int {
	interrupts, stopSignals := osSignalInterrupts()
	defer stopSignals()

	return runHostWithDeps(context.Background(), hostDependencies{
		getenv:                    os.Getenv,
		loadApprovalAuthenticator: loadOIDCApprovalAuthenticator,
		newServer:                 NewServer,
		newService: func(server *Server, addr string, stdout io.Writer) hostkit.Service {
			return newHostAPIService(server, addr, stdout)
		},
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		interrupts: interrupts,
	})
}

func runHostWithDeps(ctx context.Context, deps hostDependencies) int {
	settings, err := loadHostSettings(deps.getenv)
	if err != nil {
		return runStartupFailure(
			ctx,
			deps,
			defaultHostShutdownTimeout,
			hostkit.CodeConfigFailed,
			"host configuration failed",
			err,
		)
	}

	config, err := initializeHostConfig(
		ctx,
		settings,
		deps.getenv,
		deps.loadApprovalAuthenticator,
	)
	if err != nil {
		return runStartupFailure(
			ctx,
			deps,
			settings.shutdownTimeout,
			hostkit.CodeInitializationFailed,
			"host initialization failed",
			err,
		)
	}
	server, err := deps.newServer(config)
	if err != nil {
		return runStartupFailure(
			ctx,
			deps,
			settings.shutdownTimeout,
			hostkit.CodeInitializationFailed,
			"host initialization failed",
			err,
		)
	}

	service := deps.newService(server, settings.addr, deps.stdout)
	return runHostService(ctx, service, deps, settings.shutdownTimeout)
}

func runStartupFailure(
	ctx context.Context,
	deps hostDependencies,
	shutdownTimeout time.Duration,
	code hostkit.Code,
	safeMessage string,
	cause error,
) int {
	service := &startupFailureService{
		err: hostkit.Fail(code, safeMessage, cause),
	}
	return runHostService(ctx, service, deps, shutdownTimeout)
}

func runHostService(
	ctx context.Context,
	service hostkit.Service,
	deps hostDependencies,
	shutdownTimeout time.Duration,
) int {
	result := hostkit.Run(ctx, service, deps.interrupts, hostkit.Options{
		DrainTimeout:   shutdownTimeout,
		CleanupTimeout: hostCleanupTimeout,
	})
	if result.ExitCode() != 0 {
		stderr := deps.stderr
		if stderr == nil {
			stderr = io.Discard
		}
		_ = hostkit.WriteError(stderr, result)
	}
	return result.ExitCode()
}

type startupFailureService struct {
	err error
}

func (s *startupFailureService) Start(context.Context) error {
	return s.err
}

func (*startupFailureService) Done() <-chan error {
	return nil
}

func (*startupFailureService) Drain(context.Context) error {
	return nil
}

func (*startupFailureService) ForceStop(context.Context) error {
	return nil
}

func (*startupFailureService) Close(context.Context) error {
	return nil
}

var _ hostkit.Service = (*startupFailureService)(nil)

func osSignalInterrupts() (<-chan struct{}, func()) {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	return bridgeSignalInterrupts(signals, signal.Stop)
}

func bridgeSignalInterrupts(
	signals chan os.Signal,
	stopSignals func(chan<- os.Signal),
) (<-chan struct{}, func()) {
	interrupts := make(chan struct{}, 2)
	stopBridge := make(chan struct{})
	bridgeStopped := make(chan struct{})

	go func() {
		defer close(bridgeStopped)
		for {
			select {
			case <-stopBridge:
				return
			case <-signals:
				// A stoppable send prevents the bridge from leaking when two
				// pending events already fill the hostkit interrupt buffer.
				select {
				case interrupts <- struct{}{}:
				case <-stopBridge:
					return
				}
			}
		}
	}()

	var stopOnce sync.Once
	return interrupts, func() {
		stopOnce.Do(func() {
			stopSignals(signals)
			close(stopBridge)
			<-bridgeStopped
		})
	}
}
