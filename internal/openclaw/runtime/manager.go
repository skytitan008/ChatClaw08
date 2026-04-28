package openclawruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"chatclaw/internal/openclaw"
	"chatclaw/internal/services/settings"

	"github.com/Masterminds/semver/v3"
	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// EventListener receives gateway events. Parameters are event name and raw JSON payload.
type EventListener func(event string, payload json.RawMessage)

// ToolchainServiceIF is the subset of *toolchain.ToolchainService needed by Manager.
// Implemented as an interface to avoid a cyclic import.
type ToolchainServiceIF interface {
	InstallOpenClawRuntime() error
	SetUpgradeProgressCallback(cb func(progress int, message string))
}

type Manager struct {
	app          *application.App
	store        *configStore
	toolchainSvc ToolchainServiceIF

	opMu sync.Mutex
	mu   sync.RWMutex
	// reconciling blocks pollClient from broadcasting PhaseRestarting during
	// intentional restarts (reconcile), so the UI stays stable.
	reconciling atomic.Bool

	status       RuntimeStatus
	gatewayState GatewayConnectionState
	client       *GatewayClient
	queryClient  *GatewayClient // separate connection for queries during agent runs
	readyAt      time.Time
	readyHooks   []func()
	process      *exec.Cmd
	processPID   int
	processDone  chan error
	processLog   *os.File

	expectedStopPID     int
	shuttingDown        bool
	reconnecting        atomic.Bool
	pendingPairApproval atomic.Bool   // set when NOT_PAIRED detected; cleared after approve succeeds or gives up
	consecutiveFailures int           // resets on successful connect; triggers Doctor auto-fix at threshold
	doctorTriggered     bool          // prevents repeated Doctor triggers for the same outage window
	pollingStop         chan struct{} // closed when Manager is shut down; context for polling goroutine
	pollingMu           sync.Mutex    // serialises pollClient and Shutdown to prevent stale-phase flash

	eventListenersMu sync.RWMutex
	eventListeners   map[string]EventListener // keyed by caller-chosen ID

	upgradeProgressCb func(progress int, message string)
	upgradeMu         sync.Mutex  // serialises upgradeRuntimeLocked vs reconcileLocked to prevent OSS cascade
	upgradeInProgress atomic.Bool // set during upgrade so reconcileLocked skips OSS fallback
	upgradeCancelCh   chan struct{}
	upgradeStartTime  time.Time
	upgradeOutputBuf  strings.Builder

	doctorRunSeq uint64 // atomic: correlates streamed doctor chunks with the active UI run

	configSvc   *ConfigService      // injected via SetConfigService for SyncConfig
	modelsCache *gatewayModelsCache // in-memory cache of gateway models, loaded from openclaw.json

	systemModeIsOpenClaw atomic.Bool // set by frontend to signal that the sidebar is in openclaw mode
}

func gatewayOperatorScopes() []string {
	return []string{"operator.read", "operator.write", "operator.admin"}
}

func gatewayQueryOperatorScopes() []string {
	return gatewayOperatorScopes()
}

func NewManager(app *application.App, settingsSvc *settings.SettingsService, toolchainSvc ToolchainServiceIF) *Manager {
	store := newConfigStore(settingsSvc)
	cfg := store.Get()
	m := &Manager{
		app:          app,
		store:        store,
		toolchainSvc: toolchainSvc,
		status: RuntimeStatus{
			Phase:      PhaseIdle,
			GatewayURL: gatewayURL(cfg.GatewayPort),
		},
		eventListeners: make(map[string]EventListener),
	}
	// Set up progress callback for OSS install to forward to frontend via status events
	if toolchainSvc != nil {
		toolchainSvc.SetUpgradeProgressCallback(m.broadcastUpgradeProgress)
	}
	return m
}

// SetToolchainService injects the toolchain service after construction.
// Call this before Manager.Start() so the OSS fallback is available.
func (m *Manager) SetToolchainService(svc ToolchainServiceIF) {
	m.toolchainSvc = svc
}

// SetConfigService injects the ConfigService for model/agent config sync.
func (m *Manager) SetConfigService(svc *ConfigService) {
	m.configSvc = svc
	m.modelsCache = newGatewayModelsCache()
	svc.SetModelsCache(m.modelsCache)
}

// LoadModelsCacheFromFile loads the in-memory models cache from openclaw.json.
// Call this during startup after the gateway state dir is ready.
// If the file is missing or invalid, the cache remains dirty and the next
// send will fall back to a full SyncConfig.
func (m *Manager) LoadModelsCacheFromFile(configPath string) {
	if m.modelsCache == nil {
		return
	}
	if err := m.modelsCache.LoadFromOpenClawJSON(configPath); err != nil {
		if m.app != nil && m.app.Logger != nil {
			m.app.Logger.Warn("openclaw: failed to load models cache from openclaw.json, will sync on first send",
				"path", configPath, "error", err)
		}
		return
	}
	if m.app != nil && m.app.Logger != nil {
		m.app.Logger.Info("openclaw: models cache loaded from openclaw.json",
			"path", configPath)
	}
}

// IsModelCached returns true if the given provider/model is already in the
// local models cache and the cache is considered clean (loaded from openclaw.json).
// Returns false if the cache is dirty or the model is not present.
func (m *Manager) IsModelCached(providerID, modelID string) bool {
	if m.modelsCache == nil {
		return false
	}
	return m.modelsCache.HasModel(providerID, modelID)
}

// EnsureModelOnGateway ensures the given provider/model is available on the Gateway.
// It first checks the local cache; if found, returns immediately (no Gateway call).
// If not found, it runs a full SyncConfig, then checks the cache again.
// If still missing (e.g. new ChatWiki model), it falls back to EnsureModelRegistered
// for a targeted single-model push.
func (m *Manager) EnsureModelOnGateway(ctx context.Context, providerID, modelID string) {
	if m.modelsCache != nil && m.modelsCache.HasModel(providerID, modelID) {
		return
	}

	syncCtx, syncCancel := context.WithTimeout(ctx, 15*time.Second)
	syncErr := m.SyncConfig(syncCtx)
	syncCancel()

	if syncErr != nil {
		if m.app != nil && m.app.Logger != nil {
			m.app.Logger.Warn("openclaw: config sync failed, falling back to ensure model",
				"provider", providerID, "model", modelID, "error", syncErr)
		}
		ensureCtx, ensureCancel := context.WithTimeout(ctx, 15*time.Second)
		_ = m.EnsureModelRegistered(ensureCtx, providerID, modelID)
		ensureCancel()
		return
	}

	if m.modelsCache != nil && m.modelsCache.HasModel(providerID, modelID) {
		return
	}

	ensureCtx, ensureCancel := context.WithTimeout(ctx, 15*time.Second)
	_ = m.EnsureModelRegistered(ensureCtx, providerID, modelID)
	ensureCancel()
}

// SetSystemMode is called by the frontend when the user switches between openclaw
// and chatclaw sidebar modes. When the mode changes to 'openclaw', the gateway is
// auto-started if the runtime is available and AutoStart is enabled.
func (m *Manager) SetSystemMode(isOpenClaw bool) {
	m.systemModeIsOpenClaw.Store(isOpenClaw)
}

// shouldAutoStart returns true when the gateway should be started automatically:
//   - AutoStart setting is enabled
//   - Runtime directory exists (IsOpenClawRuntimeAvailable)
//
// Unlike the previous logic that started unconditionally based on AutoStart alone,
// this respects both conditions so that ChatClaw mode does not launch the gateway
// unless the runtime is present and the user explicitly wants it.
func (m *Manager) shouldAutoStart() bool {
	if !m.store.Get().AutoStart {
		return false
	}
	// If in openclaw mode, always auto-start (runtime availability is checked by Start).
	// If not in openclaw mode, still auto-start if runtime is available — this keeps
	// the gateway warm so agents and cron tasks work without explicit "start" clicks.
	return IsOpenClawRuntimeAvailable()
}

// SyncConfig triggers an immediate config sync to push latest models/agents to the Gateway.
// This ensures new ChatWiki models are available before sending messages.
func (m *Manager) SyncConfig(ctx context.Context) error {
	if m.configSvc == nil {
		return nil
	}
	return m.configSvc.Sync(ctx)
}

// EnsureModelRegistered checks the Gateway's live config for the given provider/model
// and registers it via a targeted patch if it is missing. This guarantees that the
// model will be available when the subsequent chat message is sent, even if the
// periodic full-sync was skipped (e.g. due to a cache hit or catalog fetch failure).
func (m *Manager) EnsureModelRegistered(ctx context.Context, providerID, modelID string) error {
	if m.configSvc == nil {
		return nil
	}
	return m.configSvc.EnsureModelRegistered(ctx, providerID, modelID)
}

func (m *Manager) SetUpgradeProgressCallback(cb func(progress int, message string)) {
	m.upgradeProgressCb = cb
}

// CancelUpgrade cancels the currently running upgrade.
// If a rollback is possible (hadCurrent was true when upgrade started), it restores the .backup
// and reconnects to the previous version. The staging directory is deleted.
// Returns an error if no upgrade was in progress.
func (m *Manager) CancelUpgrade() error {
	if !m.upgradeInProgress.Load() {
		return fmt.Errorf("no upgrade in progress")
	}
	m.app.Logger.Info("openclaw: cancel requested by user")

	// Signal all upgrade goroutines to stop.
	if m.upgradeCancelCh != nil {
		close(m.upgradeCancelCh)
	}

	// The installUserRuntimeOverrideWithCancel will handle rollback via its cancel channel check.
	// After the upgrade goroutine exits, GetStatus will return the post-cancel state.
	return nil
}

// ContinueUpgrade resumes a previously interrupted upgrade for the given version.
// It validates that a staging directory exists with all required files, then
// re-runs npm install and proceeds to activation + gateway start.
// If the staging dir is incomplete or missing, returns an error.
func (m *Manager) ContinueUpgrade(version string) (*RuntimeUpgradeResult, error) {
	if m.isShuttingDown() {
		return nil, fmt.Errorf("runtime is shutting down")
	}
	if m.upgradeInProgress.Load() {
		return nil, fmt.Errorf("upgrade already in progress")
	}

	m.upgradeInProgress.Store(true)
	defer m.upgradeInProgress.Store(false)

	m.upgradeMu.Lock()
	defer m.upgradeMu.Unlock()

	m.upgradeOutputBuf.Reset()
	m.upgradeCancelCh = make(chan struct{})
	m.upgradeStartTime = time.Now()

	// Periodic tick to broadcast elapsed time.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ticker.C:
				m.broadcastUpgradeProgress(-1, "resuming upgrade...")
			case <-m.upgradeCancelCh:
				return
			}
		}
	}()

	target := runtime.GOOS + "-" + runtime.GOARCH
	userTargetDir, err := openclaw.UserRuntimeTargetDir(target)
	if err != nil {
		return nil, err
	}
	stagingDir := filepath.Join(userTargetDir, ".staging-"+sanitizeRuntimeVersion(version))

	// Verify staging dir exists and is complete.
	dirInfo, statErr := os.Stat(stagingDir)
	if statErr != nil || dirInfo == nil {
		return nil, fmt.Errorf("staging directory for %s not found at %s", version, stagingDir)
	}
	if isComplete, verifyErr := verifyStagingComplete(stagingDir); verifyErr != nil || !isComplete {
		return nil, fmt.Errorf("staging directory incomplete: %v", verifyErr)
	}

	m.broadcastUpgradeProgress(5, fmt.Sprintf("Resuming upgrade for openclaw@%s", version))

	activeBundle, err := resolveBundledRuntime()
	if err != nil {
		return nil, err
	}

	currentVersion, err := verifyInstalled(activeBundle)
	if err != nil {
		// Ignore — may not have a working runtime yet
		currentVersion = ""
	}

	result := &RuntimeUpgradeResult{
		PreviousVersion: currentVersion,
		CurrentVersion:  currentVersion,
		LatestVersion:   version,
	}

	m.closeClient()
	m.stopProcess()
	_ = killAllNodeProcesses()

	installResult, err := m.installUserRuntimeOverrideWithCancel(activeBundle, version, "", stagingDir)
	if err != nil {
		m.app.Logger.Error("openclaw: continue upgrade install failed", "error", err)
		m.broadcastUpgradeProgress(0, "Continue failed, attempting recovery...")
		if reconcileErr := m.reconcileLocked(false); reconcileErr != nil {
			m.app.Logger.Error("openclaw: continue recovery failed", "error", reconcileErr)
		}
		return nil, err
	}

	// Gateway start (same logic as upgradeRuntimeLocked).
	const maxStartAttempts = 5
	var startupErr error
	for attempt := 1; attempt <= maxStartAttempts; attempt++ {
		select {
		case <-m.upgradeCancelCh:
			m.app.Logger.Info("openclaw: upgrade cancelled during gateway start")
			m.broadcastUpgradeProgress(0, "Upgrade cancelled, rolling back...")
			if installResult.Restore != nil {
				_ = installResult.Restore()
			}
			_ = m.reconcileLocked(false)
			return nil, fmt.Errorf("upgrade cancelled")
		default:
		}

		m.broadcastUpgradeProgress(90, fmt.Sprintf("Starting gateway (attempt %d/%d)...", attempt, maxStartAttempts))

		port := m.store.Get().GatewayPort
		if isPortAvailable(port) {
			if reconcileErr := m.reconcileLocked(false); reconcileErr == nil {
				goto continueUpgradeSucceeded
			} else {
				startupErr = reconcileErr
				m.app.Logger.Warn("openclaw: gateway start attempt failed",
					"attempt", attempt, "maxAttempts", maxStartAttempts, "error", startupErr)
				if attempt == maxStartAttempts {
					break
				}
				if !installResult.HadCurrent {
					m.broadcastUpgradeProgress(0, fmt.Sprintf("Gateway failed (attempt %d/%d), running diagnostic...", attempt, maxStartAttempts))
					if _, fixErr := m.RunDoctorCommand("check", true); fixErr != nil {
						m.app.Logger.Warn("openclaw: doctor fix failed", "error", fixErr)
					}
				}
				time.Sleep(2 * time.Second)
			}
		} else {
			m.app.Logger.Info("openclaw: gateway already running, skipping start",
				"port", port, "attempt", attempt)
			m.broadcastUpgradeProgress(100, "Gateway already running")
			goto continueUpgradeSucceeded
		}
	}

	if installResult.HadCurrent {
		m.app.Logger.Error("openclaw: continue upgrade gateway failed after 5 attempts, rolling back",
			"error", startupErr)
		m.broadcastUpgradeProgress(0, "Gateway failed after 5 attempts, rolling back to previous version...")
		if rollbackErr := installResult.Restore(); rollbackErr != nil {
			m.app.Logger.Error("openclaw: rollback failed", "error", rollbackErr)
		}
		time.Sleep(500 * time.Millisecond)
		_ = m.reconcileLocked(false)
	} else {
		m.app.Logger.Error("openclaw: first-install gateway failed after 5 attempts",
			"error", startupErr)
		m.broadcastUpgradeProgress(0, "OpenClaw failed to start after 5 attempts, please run openclaw doctor manually.")
		_ = m.reconcileLocked(false)
	}
	return nil, fmt.Errorf("gateway failed after %d attempts: %w", maxStartAttempts, startupErr)

continueUpgradeSucceeded:
	installResult.DeleteBackup()
	installResult.Cleanup()

	status := m.GetStatus()
	result.Upgraded = true
	result.CurrentVersion = version
	result.RuntimeSource = status.RuntimeSource
	result.RuntimePath = status.RuntimePath
	return result, nil
}

func (m *Manager) Start() {
	// Always start polling so the UI stays in sync with the gateway's real state
	// (even if auto-start was disabled via "stop" button, or the port is already
	// occupied from a previous session — polling will detect the running gateway
	// and reconnect the WebSocket client without requiring an explicit "start").
	m.pollingStop = make(chan struct{})
	go m.pollGateway()

	if !m.shouldAutoStart() {
		return
	}
	go func() { _ = m.reconcile(false) }()
}

// pollGateway is an unrestricted background loop that continuously probes OpenClaw's
// real state and updates the UI — it never blocks, never stops on its own,
// and is only cancelled when Shutdown() closes pollingStop.
func (m *Manager) pollGateway() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.pollingStop:
			return
		case <-ticker.C:
			m.pollClient()
		}
	}
}

// pollClient probes the gateway port and attempts WebSocket reconnection.
// Completely passive: never restarts the process, never attempts OSS install,
// and runs regardless of internal state (shuttingDown / reconnecting lock do not block it).
// The only effect is updating Phase and GatewayState sent to the UI.
func (m *Manager) pollClient() {
	cfg := m.store.Get()
	alive := gatewayPortOccupied(cfg.GatewayPort)

	m.mu.RLock()
	connected := m.client != nil
	m.mu.RUnlock()

	if connected {
		// WS is up — if the UI still shows a degraded phase (e.g. "restarting" after
		// NotifyGatewayRestarting), push the correct state so it updates immediately
		// without waiting for the next GetStatus() poll.
		m.mu.RLock()
		phase := m.status.Phase
		m.mu.RUnlock()
		if phase != PhaseConnected {
			m.mu.RLock()
			pid := m.processPID
			version := m.status.InstalledVersion
			runtimeSource := m.status.RuntimeSource
			runtimePath := m.status.RuntimePath
			m.mu.RUnlock()
			cfg := m.store.Get()
			m.broadcastStatus(RuntimeStatus{
				Phase:            PhaseConnected,
				Message:          "OpenClaw Gateway connected",
				InstalledVersion: version,
				RuntimeSource:    runtimeSource,
				RuntimePath:      runtimePath,
				GatewayPID:       pid,
				GatewayURL:       gatewayURL(cfg.GatewayPort),
			})
		}
		return
	}

	// Not connected — determine the right response based on gateway liveness.
	// Skip if a reconcile is in progress (intentional restart), which manages its own broadcasts.
	if !alive {
		// Gateway not responding at all.
		m.mu.RLock()
		prevPhase := m.status.Phase
		m.mu.RUnlock()
		if prevPhase == PhaseConnected || prevPhase == PhaseConnecting || prevPhase == PhaseError {
			if !m.reconciling.Load() {
				m.broadcastStatus(RuntimeStatus{
					Phase:      PhaseRestarting,
					Message:    "OpenClaw Gateway not responding",
					GatewayURL: gatewayURL(cfg.GatewayPort),
				})
			}
		}
		// No process, no port — nothing to reconnect to.
		return
	}

	// Gateway is alive (port open) but WS is down — this is the recovery window.
	// Attempt WS reconnect only if not already in progress.
	if m.reconnecting.CompareAndSwap(false, true) {
		go func() {
			defer m.reconnecting.Store(false)
			time.Sleep(500 * time.Millisecond)
			m.reconnectClient()
		}()
	}
}

func (m *Manager) Shutdown() {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	// Stop the unrestricted polling loop first.
	m.mu.Lock()
	if m.pollingStop != nil {
		close(m.pollingStop)
		m.pollingStop = nil
	}
	m.mu.Unlock()

	m.mu.Lock()
	m.shuttingDown = true
	m.mu.Unlock()
	m.closeClient()
	m.stopProcess()

	cfg := m.store.Get()
	m.mu.RLock()
	prev := m.status
	m.mu.RUnlock()

	// Verify port is released after shutdown; if still occupied, force kill.
	if gatewayPortOccupied(cfg.GatewayPort) {
		m.app.Logger.Warn("openclaw: port still occupied after stop, attempting force cleanup",
			"port", cfg.GatewayPort)

		// Try openclaw gateway stop one more time
		bundle, err := resolveBundledRuntime()
		if err == nil {
			m.runGatewayStopCLI(bundle.CLIPath)
			time.Sleep(800 * time.Millisecond)
		}

		// Kill any remaining listeners
		if err := killListenersOnLocalTCPPort(cfg.GatewayPort); err != nil {
			m.app.Logger.Warn("openclaw: killListenersOnLocalTCPPort failed during shutdown", "error", err)
		}
		time.Sleep(400 * time.Millisecond)

		// Last resort: kill all node processes
		if gatewayPortOccupied(cfg.GatewayPort) {
			m.app.Logger.Warn("openclaw: port still occupied after CLI stop, killing node processes",
				"port", cfg.GatewayPort)
			if err := killAllNodeProcesses(); err != nil {
				m.app.Logger.Warn("openclaw: killAllNodeProcesses failed during shutdown", "error", err)
			}
			time.Sleep(500 * time.Millisecond)

			if err := killListenersOnLocalTCPPort(cfg.GatewayPort); err != nil {
				m.app.Logger.Warn("openclaw: final killListenersOnLocalTCPPort failed", "error", err)
			}
		}

		// Final check: report if port is still occupied
		if gatewayPortOccupied(cfg.GatewayPort) {
			occupyingPID := getOccupyingProcessPID(cfg.GatewayPort)
			m.app.Logger.Error("openclaw: port still occupied after shutdown cleanup",
				"port", cfg.GatewayPort, "occupyingPID", occupyingPID)
		} else {
			m.app.Logger.Info("openclaw: port successfully released after shutdown",
				"port", cfg.GatewayPort)
		}
	}

	// User-initiated stop: refresh persisted status so GetStatus/UI match reality.
	// Without this, phase stays "connected" while the process is gone (misleading toast + badge).
	m.broadcastStatus(RuntimeStatus{
		Phase:            PhaseIdle,
		Message:          "OpenClaw Gateway stopped",
		InstalledVersion: prev.InstalledVersion,
		RuntimeSource:    prev.RuntimeSource,
		RuntimePath:      prev.RuntimePath,
		GatewayPID:       0,
		GatewayURL:       gatewayURL(cfg.GatewayPort),
	})
	m.broadcastGatewayState(GatewayConnectionState{
		Connected:     false,
		Authenticated: false,
		Reconnecting:  false,
	})

	m.mu.Lock()
	m.shuttingDown = false
	m.mu.Unlock()
}

func (m *Manager) GetStatus() RuntimeStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

func (m *Manager) GetGatewayState() GatewayConnectionState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.gatewayState
}

func (m *Manager) RestartGateway() (RuntimeStatus, error) {
	err := m.reconcile(true)
	return m.GetStatus(), err
}

// StartGateway starts the OpenClaw gateway when it is stopped (idle state).
// Unlike RestartGateway, this does not restart an already running gateway.
func (m *Manager) StartGateway() (RuntimeStatus, error) {
	err := m.reconcile(false)
	// When there is no runtime, reconcile returns nil so the caller does not
	// show a raw error toast; the status already carries PhaseNotInstalled so
	// the UI can show a user-friendly "请前往设置安装" banner instead.
	return m.GetStatus(), err
}

// InstallAndStartRuntime downloads the OpenClaw runtime from OSS and starts the gateway.
// This is the "OSS install" equivalent of UpgradeRuntime: it installs the runtime bundle,
// stops any existing gateway, and starts a new one using the newly installed runtime.
func (m *Manager) InstallAndStartRuntime() (*RuntimeUpgradeResult, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	cfg := m.store.Get()

	// Broadcast installing state
	m.broadcastStatus(RuntimeStatus{
		Phase:      PhaseUpgrading,
		Message:    "Downloading OpenClaw runtime from OSS...",
		GatewayURL: gatewayURL(cfg.GatewayPort),
	})
	m.closeClient()
	m.stopProcess()

	// Proactively kill stray node.exe processes that might hold file locks on
	// runtime directories (e.g., leftover from a previous aborted upgrade or
	// a testers manual delete attempt). Do this before any file operations.
	_ = killAllNodeProcesses()

	// Set up progress callback so the UI can show install progress
	if m.upgradeProgressCb != nil {
		m.toolchainSvc.SetUpgradeProgressCallback(m.upgradeProgressCb)
	}

	if err := m.toolchainSvc.InstallOpenClawRuntime(); err != nil {
		_ = m.reconcileLocked(false)
		return nil, fmt.Errorf("OSS runtime install: %w", err)
	}

	bundle, err := resolveBundledRuntime()
	if err != nil {
		_ = m.reconcileLocked(false)
		return nil, fmt.Errorf("resolveBundledRuntime after OSS install: %w", err)
	}
	installedVersion, err := verifyInstalled(bundle)
	if err != nil {
		_ = m.reconcileLocked(false)
		return nil, fmt.Errorf("verifyInstalled after OSS install: %w", err)
	}

	// Activate the newly installed runtime
	if err := m.reconcileLocked(false); err != nil {
		_ = m.reconcileLocked(false)
		return nil, fmt.Errorf("reconcile after OSS install: %w", err)
	}

	status := m.GetStatus()
	return &RuntimeUpgradeResult{
		PreviousVersion: "",
		CurrentVersion:  installedVersion,
		LatestVersion:   installedVersion,
		Upgraded:        true,
		RuntimeSource:   status.RuntimeSource,
		RuntimePath:     status.RuntimePath,
	}, nil
}

func (m *Manager) UpgradeRuntime() (*RuntimeUpgradeResult, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.upgradeRuntimeLocked()
}

// reconcile is the single entry point for lifecycle management:
// resolve bundle → verify install → start process → connect WebSocket.
func (m *Manager) reconcile(restart bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.reconcileLocked(restart)
}

func (m *Manager) reconcileLocked(restart bool) error {
	if m.isShuttingDown() {
		return fmt.Errorf("runtime is shutting down")
	}

	// Block pollClient from broadcasting PhaseRestarting during this whole operation.
	m.reconciling.Store(true)
	defer m.reconciling.Store(false)

	cfg := m.store.Get()

	fail := func(phase string, err error, version string, pid int) error {
		m.app.Logger.Warn("openclaw: "+phase, "error", err)
		msg := cleanErrorMessage(err.Error())
		m.broadcastStatus(RuntimeStatus{
			Phase:            phase,
			Message:          msg,
			InstalledVersion: version,
			GatewayPID:       pid,
			GatewayURL:       gatewayURL(cfg.GatewayPort),
		})
		return err
	}

	// Fast path: WebSocket already up (covers adopted gateway where m.process is nil).
	if !restart {
		m.mu.RLock()
		ready := m.client != nil
		m.mu.RUnlock()
		if ready {
			// Status may never have received runtime metadata (e.g. AutoStart off: only poll/reconnect
			// established WS). Fill path/source/version from the resolved bundle when missing.
			if b, err := resolveBundledRuntime(); err == nil {
				m.mu.RLock()
				pid := m.processPID
				needMeta := m.status.RuntimePath == "" || m.status.RuntimeSource == ""
				m.mu.RUnlock()
				if needMeta {
					ver := strings.TrimSpace(b.Manifest.OpenClawVersion)
					if v, err := verifyInstalled(b); err == nil {
						ver = v
					}
					cfg := m.store.Get()
					m.broadcastStatus(RuntimeStatus{
						Phase:            PhaseConnected,
						Message:          "OpenClaw Gateway connected",
						InstalledVersion: ver,
						RuntimeSource:    b.Source,
						RuntimePath:      b.Root,
						GatewayPID:       pid,
						GatewayURL:       gatewayURL(cfg.GatewayPort),
					})
				}
			}
			return nil
		}
	}

	bundle, err := resolveBundledRuntime()
	if err != nil {
		// OSS install is only triggered by InstallAndStartRuntime().
		// reconcileLocked is called by Start/StartGateway/AutoStart which assume
		// a runtime is already present; OSS fallback here causes unwanted upgrades.
		if m.upgradeInProgress.Load() {
			m.app.Logger.Warn("openclaw: no bundled runtime during upgrade, skipping",
				"error", err)
		}
		// Distinguish: runtime not found vs manifest/binary invalid.
		// Emit PhaseNotInstalled so the UI can guide the user to install instead of showing an error.
		if IsOpenClawRuntimeAvailable() {
			// Some candidate exists but was invalid — treat as a real error.
			return fail("resolveBundledRuntime", err, "", 0)
		}
		// No runtime at all — emit PhaseNotInstalled so the UI prompts to install.
		// Return nil so the caller (StartGateway in the frontend) does not show an
		// error toast with a raw technical error message.
		m.app.Logger.Info("openclaw: no openclaw runtime found, prompting user to install",
			"error", err)
		m.broadcastStatus(RuntimeStatus{
			Phase:      PhaseNotInstalled,
			Message:    "OpenClaw runtime not installed",
			GatewayURL: gatewayURL(cfg.GatewayPort),
		})
		return nil
	}

	m.appendStartStep("1. resolve_runtime", bundle.Root)

	if patched, err := applyBundledRuntimeHotfixes(bundle); err != nil {
		m.app.Logger.Warn("openclaw: runtime hotfix apply failed",
			"runtimePath", bundle.Root, "error", err)
	} else if patched > 0 {
		m.app.Logger.Info("openclaw: runtime hotfix applied",
			"runtimePath", bundle.Root, "patchedFiles", patched)
		m.appendStartStep("2. apply_hotfix", fmt.Sprintf("patched %d file(s)", patched))
	}

	if restart {
		m.appendStartStep("3. stop_process", "")
		m.closeClient()
		m.stopProcess()
	}

	version, err := verifyInstalled(bundle)
	if err != nil {
		return fail("verifyInstalled", err, "", 0)
	}

	m.appendStartStep("4. verify_installed", version)

	m.broadcastStatus(RuntimeStatus{
		Phase:            PhaseStarting,
		Message:          "Preparing OpenClaw Gateway",
		InstalledVersion: version,
		RuntimeSource:    bundle.Source,
		RuntimePath:      bundle.Root,
		GatewayURL:       gatewayURL(cfg.GatewayPort),
	})

	// Ensure state dir and sandbox config are set up BEFORE the adopt/spawn branch.
	// The adopt path skips process start entirely, so these must run here — not inside
	// startProcess — to guarantee a consistent gateway state even when ChatClaw connects
	// to an already-running OpenClaw process (e.g. after a system restart).
	if err := ensureOpenClawStateDir(bundle, cfg.GatewayPort, cfg.GatewayToken); err != nil {
		return fail("ensureOpenClawStateDir", err, version, 0)
	}
	ensureSandboxConfigured(bundle)

	m.appendStartStep("5. ensure_state_dir", bundle.StateDir)

	// Start process if needed
	m.mu.RLock()
	needProcess := m.process == nil
	pid := m.processPID
	m.mu.RUnlock()

	if needProcess {
		// Port already in use: assume OpenClaw is already running (ours or recovered).
		// Do NOT spawn a second gateway — that fails to bind and leaves phase=error.
		if !restart && gatewayPortOccupied(cfg.GatewayPort) {
			pid = getOccupyingProcessPID(cfg.GatewayPort)
			m.app.Logger.Info("openclaw: adopting existing gateway on port (skip spawn)",
				"port", cfg.GatewayPort, "pid", pid)
			m.appendStartStep("6. adopt_existing", fmt.Sprintf("pid=%d", pid))
		} else {
			m.appendStartStep("6. start_process", fmt.Sprintf("port=%d", cfg.GatewayPort))
			if err := m.startProcess(cfg, bundle, version, restart); err != nil {
				return fail("startProcess", err, version, 0)
			}
			m.mu.RLock()
			pid = m.processPID
			m.mu.RUnlock()
		}
	} else {
		m.appendStartStep("6. process_exists", fmt.Sprintf("pid=%d", pid))
	}

	// Connect client if needed
	m.mu.RLock()
	needClient := m.client == nil
	m.mu.RUnlock()

	if needClient {
		m.appendStartStep("7. connect_websocket", fmt.Sprintf("port=%d", cfg.GatewayPort))
		m.broadcastStatus(RuntimeStatus{
			Phase:            PhaseConnecting,
			Message:          "Connecting to OpenClaw Gateway",
			InstalledVersion: version,
			RuntimeSource:    bundle.Source,
			RuntimePath:      bundle.Root,
			GatewayPID:       pid,
			GatewayURL:       gatewayURL(cfg.GatewayPort),
		})
		err := m.connectClient(cfg, bundle)
		if err != nil {
			var gerr *GatewayRequestError
			if errors.As(err, &gerr) && strings.EqualFold(strings.TrimSpace(gerr.Code), "NOT_PAIRED") {
				// Same as reconnectClient: HTTP approve then retry WS once (initial reconcile used to skip this).
				m.app.Logger.Info("openclaw: NOT_PAIRED on first connect, auto-approving then retrying WS")
				m.appendStartStep("8. auto_approve_pairing", "")
				m.approvePendingDevices()
				err = m.connectClient(cfg, bundle)
			}
			if err != nil {
				var gerr2 *GatewayRequestError
				if errors.As(err, &gerr2) && strings.EqualFold(strings.TrimSpace(gerr2.Code), "NOT_PAIRED") {
					m.broadcastStatus(RuntimeStatus{
						Phase:            PhaseConnecting,
						Message:          err.Error(),
						InstalledVersion: version,
						RuntimeSource:    bundle.Source,
						RuntimePath:      bundle.Root,
						GatewayPID:       pid,
						GatewayURL:       gatewayURL(cfg.GatewayPort),
					})
					m.broadcastGatewayState(GatewayConnectionState{
						Connected:     false,
						Authenticated: false,
						Reconnecting:  true,
						LastError:     err.Error(),
					})
					return err
				}
				return fail("connectClient", err, version, pid)
			}
		}
	}

	m.mu.Lock()
	m.readyAt = time.Now()
	m.mu.Unlock()

	m.appendStartStep("9. connected", version)
	m.broadcastStatus(RuntimeStatus{
		Phase:            PhaseConnected,
		Message:          "OpenClaw Gateway connected",
		InstalledVersion: version,
		RuntimeSource:    bundle.Source,
		RuntimePath:      bundle.Root,
		GatewayPID:       pid,
		GatewayURL:       gatewayURL(cfg.GatewayPort),
	})
	m.broadcastGatewayState(GatewayConnectionState{Connected: true, Authenticated: true, Reconnecting: false, LastError: ""})
	m.consecutiveFailures = 0
	m.doctorTriggered = false
	m.notifyReadyHooks()

	return nil
}

// --- Process management ---

// gatewayPortOccupied reports whether something accepts TCP connections on the gateway loopback port.
func gatewayPortOccupied(port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (m *Manager) runGatewayStopCLI(cliPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliPath, "gateway", "stop")
	setCmdHideWindow(cmd)
	if err := cmd.Run(); err != nil {
		m.app.Logger.Warn("openclaw: gateway stop CLI finished with error", "error", err)
	}
}

// ensurePortClean frees the gateway port before starting a new process. If TCP dial
// succeeds, something is already listening — we must run "openclaw gateway stop" and,
// when that is not enough, kill listeners (netstat/taskkill on Windows) and optionally
// all node.exe processes (restart path). Returns an error if the port cannot be freed
// after all cleanup attempts.
func (m *Manager) ensurePortClean(port int, cliPath string, aggressive bool) error {
	const maxRounds = 5
	var lastErr error

	for round := 0; round < maxRounds; round++ {
		if !gatewayPortOccupied(port) {
			if round > 0 {
				m.app.Logger.Info("openclaw: gateway port is free after cleanup", "port", port)
			}
			return nil
		}

		m.app.Logger.Info("openclaw: gateway port occupied; cleanup",
			"port", port, "round", round+1, "aggressiveRestart", aggressive)

		// Step 1: Run openclaw gateway stop
		m.runGatewayStopCLI(cliPath)
		time.Sleep(800 * time.Millisecond)

		if !gatewayPortOccupied(port) {
			return nil
		}

		// Step 2: Kill listeners on the specific port
		if err := killListenersOnLocalTCPPort(port); err != nil {
			m.app.Logger.Warn("openclaw: killListenersOnLocalTCPPort failed", "error", err)
			lastErr = err
		}
		time.Sleep(400 * time.Millisecond)

		if !gatewayPortOccupied(port) {
			return nil
		}

		// Step 3: Kill all node processes (aggressive cleanup)
		if aggressive || round >= 1 {
			if err := killAllNodeProcesses(); err != nil {
				m.app.Logger.Warn("openclaw: killAllNodeProcesses failed", "error", err)
				lastErr = err
			}
			time.Sleep(500 * time.Millisecond)

			if err := killListenersOnLocalTCPPort(port); err != nil {
				m.app.Logger.Warn("openclaw: killListenersOnLocalTCPPort after node kill failed", "error", err)
				lastErr = err
			}
			time.Sleep(300 * time.Millisecond)
		}

		if !gatewayPortOccupied(port) {
			return nil
		}
	}

	// After all cleanup attempts, check once more and report the occupying process
	if gatewayPortOccupied(port) {
		occupyingPID := getOccupyingProcessPID(port)
		errMsg := fmt.Sprintf("port %d is still occupied after cleanup attempts (PID: %d)", port, occupyingPID)
		m.app.Logger.Error("openclaw: " + errMsg)
		return fmt.Errorf("port %d is still occupied after cleanup attempts (PID: %d)", port, occupyingPID)
	}

	return lastErr
}

func (m *Manager) startProcess(cfg OpenClawConfig, bundle *bundledRuntime, installedVersion string, aggressiveCleanup bool) error {
	// If the port is already occupied, assume OpenClaw itself started it.
	// Skip cleanup and reuse the existing process — only clean in extreme cases.
	if !aggressiveCleanup && gatewayPortOccupied(cfg.GatewayPort) {
		occupyingPID := getOccupyingProcessPID(cfg.GatewayPort)
		processName := getProcessNameByPID(occupyingPID)
		m.app.Logger.Info("openclaw: gateway port already occupied, skipping cleanup",
			"port", cfg.GatewayPort, "pid", occupyingPID, "process", processName)
		// Only clean if the occupying process is not a node process (likely a residual).
		if processName != "" && !strings.Contains(strings.ToLower(processName), "node") {
			m.app.Logger.Warn("openclaw: unknown process on gateway port, cleaning up",
				"port", cfg.GatewayPort, "pid", occupyingPID, "process", processName)
			if err := m.ensurePortClean(cfg.GatewayPort, bundle.CLIPath, false); err != nil {
				return fmt.Errorf("port %d cleanup failed: %w", cfg.GatewayPort, err)
			}
		}
	} else if err := m.ensurePortClean(cfg.GatewayPort, bundle.CLIPath, aggressiveCleanup); err != nil {
		m.app.Logger.Error("openclaw: port cleanup failed, cannot start gateway", "error", err)
		return fmt.Errorf("port %d cleanup failed: %w", cfg.GatewayPort, err)
	}

	logFile, err := openGatewayLogFile(bundle.LogsDir)
	if err != nil {
		return err
	}
	rawStreamPath := gatewayRawStreamLogPath(bundle.LogsDir)
	_ = os.Remove(rawStreamPath)

	// On Windows, call node.exe directly so CREATE_NO_WINDOW takes effect.
	// openclaw.cmd goes through cmd.exe which always creates a visible window
	// even when the parent has CREATE_NO_WINDOW — node.exe itself can be hidden.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		entryPath := filepath.Join(bundle.Root, "lib", "node_modules", "openclaw", "dist", "entry.js")
		cmd = exec.Command(bundle.NodeExePath, entryPath,
			"gateway", "run",
			"--allow-unconfigured",
			"--port", strconv.Itoa(cfg.GatewayPort),
			"--bind", "loopback",
			"--auth", "token",
			"--token", cfg.GatewayToken,
		)
	} else {
		cmd = exec.Command(bundle.CLIPath,
			"gateway", "run",
			"--allow-unconfigured",
			"--port", strconv.Itoa(cfg.GatewayPort),
			"--bind", "loopback",
			"--auth", "token",
			"--token", cfg.GatewayToken,
		)
		// Note: Do NOT pass --force here. The Manager already calls stopProcess()
		// (via reconcileLocked) before startProcess, so the port is guaranteed
		// clean. On Windows, --force runs "fuser" which is unavailable and causes
		// the gateway to exit with status 1, triggering an unwanted restart loop.
	}
	cmd.Env = buildGatewayEnv(cfg, bundle)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = bundle.Root
	setCmdHideWindow(cmd)

	m.app.Logger.Info("openclaw: raw stream debug enabled", "path", rawStreamPath)

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start openclaw gateway: %w", err)
	}

	done := make(chan error, 1)
	pid := cmd.Process.Pid
	m.mu.Lock()
	m.process = cmd
	m.processPID = pid
	m.processDone = done
	m.processLog = logFile
	m.mu.Unlock()

	go func() {
		waitErr := cmd.Wait()
		done <- waitErr
		_ = logFile.Close()
		m.handleProcessExit(pid, waitErr)
	}()

	m.broadcastStatus(RuntimeStatus{
		Phase:            PhaseStarting,
		Message:          "Starting OpenClaw Gateway",
		InstalledVersion: installedVersion,
		RuntimeSource:    bundle.Source,
		RuntimePath:      bundle.Root,
		GatewayPID:       pid,
		GatewayURL:       gatewayURL(cfg.GatewayPort),
	})
	return nil
}

func (m *Manager) stopProcess() {
	m.mu.Lock()
	if m.process == nil {
		m.mu.Unlock()
		return
	}
	proc := m.process
	done := m.processDone
	m.expectedStopPID = m.processPID
	m.process = nil
	m.processPID = 0
	m.processDone = nil
	m.processLog = nil
	m.mu.Unlock()

	if proc.Process != nil {
		if runtime.GOOS == "windows" {
			_ = proc.Process.Kill()
		} else {
			_ = proc.Process.Signal(os.Interrupt)
		}
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if proc.Process != nil {
				_ = proc.Process.Kill()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (m *Manager) handleProcessExit(pid int, exitErr error) {
	m.app.Logger.Info("openclaw: process exited", "pid", pid, "error", exitErr)
	m.mu.Lock()
	intentional := pid == m.expectedStopPID
	if intentional {
		m.expectedStopPID = 0
	}
	if m.processPID == pid {
		m.process = nil
		m.processPID = 0
		m.processDone = nil
		m.processLog = nil
		m.client = nil
		if m.queryClient != nil {
			_ = m.queryClient.Close()
			m.queryClient = nil
		}
		m.readyAt = time.Time{}
	}
	shuttingDown := m.shuttingDown
	m.mu.Unlock()

	if shuttingDown || intentional {
		return
	}

	cfg := m.store.Get()

	// Wait 4 seconds before checking the port to avoid a race with the restarting
	// gateway: the old process may still be releasing the socket while OpenClaw's
	// supervisor is already spawning the new one.
	time.Sleep(4 * time.Second)

	// Port still occupied after process exit → OpenClaw detected a config change
	// and is self-restarting; gateway is already back up. No action needed —
	// reconnectClient will naturally succeed on its next polling attempt.
	if gatewayPortOccupied(cfg.GatewayPort) {
		m.app.Logger.Info("openclaw: config change detected, gateway already recovered",
			"port", cfg.GatewayPort)
		return
	}

	// Port is free → gateway is truly gone. OpenClaw's supervisor will handle
	// the actual restart; we just wait for the port to come back.
	m.broadcastStatus(RuntimeStatus{
		Phase:      PhaseRestarting,
		Message:    "OpenClaw Gateway exited, waiting for auto-recovery",
		GatewayPID: 0,
		GatewayURL: gatewayURL(cfg.GatewayPort),
	})
	m.broadcastGatewayState(GatewayConnectionState{
		Connected:     false,
		Authenticated: false,
		Reconnecting:  true,
		LastError:     errStr(exitErr),
	})
}

// --- WebSocket client management ---

func (m *Manager) connectClient(cfg OpenClawConfig, bundle *bundledRuntime) error {
	identity, err := loadOrCreateDeviceIdentity(bundle.StateDir)
	if err != nil {
		return err
	}
	storedTok, _ := loadStoredDeviceToken(bundle.StateDir, clientRole)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var lastErr error
	for {
		if ctx.Err() != nil {
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		}

		m.mu.RLock()
		managed := m.process != nil
		m.mu.RUnlock()
		// Allow WS connect when we did not spawn the process but something listens on the port.
		alive := managed || gatewayPortOccupied(cfg.GatewayPort)
		if !alive {
			if lastErr != nil {
				return fmt.Errorf("gateway process exited: %w", lastErr)
			}
			return fmt.Errorf("gateway process exited before connection established")
		}

		client := NewGatewayClient(gatewayClientOptions{
			URL:             gatewayWebSocketURL(cfg.GatewayPort),
			Token:           cfg.GatewayToken,
			DeviceIdentity:  identity,
			StoredDeviceTok: storedTok,
			Scopes:          gatewayOperatorScopes(),
			OnEvent:         m.dispatchEvent,
			OnDisconnect:    m.handleGatewayDisconnect,
			OnLateError:     m.handleLateErrorResponse,
		})
		hello, err := client.Connect(ctx)
		if err == nil {
			if hello.Auth != nil && hello.Auth.DeviceToken != "" {
				_ = storeDeviceToken(bundle.StateDir, hello.Auth.Role, hello.Auth.DeviceToken, hello.Auth.Scopes)
			}

			// Create a second connection for queries (sessions.get etc.)
			// so they don't block on long-running agent RPCs.
			qClient := NewGatewayClient(gatewayClientOptions{
				URL:             gatewayWebSocketURL(cfg.GatewayPort),
				Token:           cfg.GatewayToken,
				DeviceIdentity:  identity,
				StoredDeviceTok: storedTok,
				Scopes:          gatewayQueryOperatorScopes(),
			})
			if _, qErr := qClient.Connect(ctx); qErr != nil {
				m.app.Logger.Warn("openclaw: query client connect failed, will use main client", "err", qErr)
				qClient = nil
			}

			m.mu.Lock()
			m.client = client
			m.queryClient = qClient
			m.mu.Unlock()
			return nil
		}
		lastErr = err
		if !shouldRetryConnect(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func (m *Manager) closeClient() {
	m.mu.Lock()
	client := m.client
	qClient := m.queryClient
	m.client = nil
	m.queryClient = nil
	m.readyAt = time.Time{}
	m.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
	if qClient != nil {
		_ = qClient.Close()
	}
}

// approvePendingDevices runs the OpenClaw CLI to list pending pairing requests
// and auto-approves them. This avoids the HTTP API issue where the gateway's
// REST endpoint (/) returns an HTML redirect instead of JSON.
//
// Actual JSON structure observed from openclaw CLI --json:
//
//	{ "pending": [{ "requestId": "...", "deviceId": "...", "displayName": "ChatClaw", ... }], "paired": [...] }
func (m *Manager) approvePendingDevices() {
	if !m.pendingPairApproval.CompareAndSwap(false, true) {
		return
	}
	defer m.pendingPairApproval.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Step 1: devices list --json
	listOut, err := m.ExecCLI(ctx, "devices", "list", "--json")
	if err != nil {
		m.app.Logger.Warn("openclaw: approve: devices list failed", "error", err, "output", string(listOut))
		return
	}

	// Step 2: parse the confirmed structure { pending: [{ requestId, deviceId, displayName, ... }], paired: [...] }
	var listResult struct {
		Pending []struct {
			RequestID   string `json:"requestId"`
			DeviceID    string `json:"deviceId"`
			DisplayName string `json:"displayName"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(listOut, &listResult); err != nil {
		m.app.Logger.Warn("openclaw: approve: decode devices list failed", "error", err, "output", strings.TrimSpace(string(listOut)))
		return
	}

	if len(listResult.Pending) == 0 {
		m.app.Logger.Info("openclaw: approve: no pending devices found, skipping")
		return
	}

	for _, r := range listResult.Pending {
		m.app.Logger.Info("openclaw: approve: found pending request",
			"requestId", r.RequestID, "deviceId", r.DeviceID, "displayName", r.DisplayName)
	}

	// Step 3: approve --latest (gateway auto-selects the most recent pending request)
	m.app.Logger.Info("openclaw: approve: auto-approving latest pending device via CLI")
	approveOut, err := m.ExecCLI(ctx, "devices", "approve", "--latest", "--json")
	if err != nil {
		m.app.Logger.Warn("openclaw: approve: CLI approve failed", "error", err, "output", strings.TrimSpace(string(approveOut)))
		return
	}

	// Verify approve succeeded by checking for approvedAtMs in response.
	var approveResult struct {
		RequestID string `json:"requestId"`
		Device    struct {
			DeviceID     string `json:"deviceId"`
			ApprovedAtMs int64  `json:"approvedAtMs"`
		} `json:"device"`
	}
	if err := json.Unmarshal(approveOut, &approveResult); err == nil && approveResult.Device.ApprovedAtMs > 0 {
		m.app.Logger.Info("openclaw: approve: device approved successfully",
			"requestId", approveResult.RequestID,
			"deviceId", approveResult.Device.DeviceID,
			"approvedAtMs", approveResult.Device.ApprovedAtMs)
	} else {
		m.app.Logger.Info("openclaw: approve: device approved via CLI", "output", strings.TrimSpace(string(approveOut)))
	}
}

// reconnectClient only reconnects WebSocket, does not touch the process.
// WS reconnect failures are surfaced to the user as an error message;
// heartbeat polling will retry until OpenClaw recovers.
func (m *Manager) reconnectClient() {
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	cfg := m.store.Get()
	if m.process == nil && !gatewayPortOccupied(cfg.GatewayPort) {
		m.app.Logger.Info("openclaw: skipping WS reconnect, no gateway process and port not listening")
		m.broadcastGatewayState(GatewayConnectionState{
			Connected:     false,
			Authenticated: false,
			Reconnecting:  true,
			LastError:     "Gateway process not running",
		})
		return
	}
	bundle, err := resolveBundledRuntime()
	if err != nil {
		m.app.Logger.Warn("openclaw: reconnect: no bundled runtime", "error", err)
		m.broadcastGatewayState(GatewayConnectionState{
			Connected:     false,
			Authenticated: false,
			Reconnecting:  true,
			LastError:     "No runtime available",
		})
		return
	}

	if err := m.connectClient(cfg, bundle); err != nil {
		// NOT_PAIRED: auto-approve pending device, then retry WS once.
		var gerr *GatewayRequestError
		if errors.As(err, &gerr) && strings.EqualFold(strings.TrimSpace(gerr.Code), "NOT_PAIRED") {
			m.app.Logger.Info("openclaw: NOT_PAIRED, auto-approving device then retrying WS")
			m.approvePendingDevices()

			// Retry WS connection after approve.
			if retryErr := m.connectClient(cfg, bundle); retryErr == nil {
				goto connected
			}
		}

		// Always broadcast the error to frontend so the user sees what's happening.
		// Don't silently trigger restarts — let the user know and give them a chance to act.
		errMsg := err.Error()
		m.app.Logger.Warn("openclaw: WS reconnect failed", "error", err)
		m.broadcastGatewayState(GatewayConnectionState{
			Connected:     false,
			Authenticated: false,
			Reconnecting:  true,
			LastError:     errMsg,
		})
		return
	}

connected:
	m.consecutiveFailures = 0
	m.doctorTriggered = false

	version := strings.TrimSpace(bundle.Manifest.OpenClawVersion)
	if v, err := verifyInstalled(bundle); err == nil {
		version = v
	}

	m.mu.RLock()
	pid := m.processPID
	m.mu.RUnlock()
	m.broadcastStatus(RuntimeStatus{
		Phase:            PhaseConnected,
		Message:          "OpenClaw Gateway connected",
		InstalledVersion: version,
		RuntimeSource:    bundle.Source,
		RuntimePath:      bundle.Root,
		GatewayPID:       pid,
		GatewayURL:       gatewayURL(cfg.GatewayPort),
	})
	m.broadcastGatewayState(GatewayConnectionState{
		Connected:     true,
		Authenticated: true,
		Reconnecting:  false,
		LastError:     "",
	})
	m.app.Logger.Info("openclaw: WS reconnect succeeded", "pid", pid)
}

func (m *Manager) handleGatewayDisconnect(err error) {
	m.app.Logger.Info("openclaw: gateway disconnected", "error", err)
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return
	}
	// No managed process but port may still be serving an adopted gateway — try WS reconnect.
	cfg := m.store.Get()
	if m.process == nil && !gatewayPortOccupied(cfg.GatewayPort) {
		m.client = nil
		if m.queryClient != nil {
			_ = m.queryClient.Close()
			m.queryClient = nil
		}
		m.readyAt = time.Time{}
		m.mu.Unlock()
		m.broadcastGatewayState(GatewayConnectionState{
			Connected:     false,
			Authenticated: false,
			Reconnecting:  true,
			LastError:     errStr(err),
		})
		return
	}
	m.client = nil
	if m.queryClient != nil {
		_ = m.queryClient.Close()
		m.queryClient = nil
	}
	m.readyAt = time.Time{}
	m.mu.Unlock()

	m.broadcastGatewayState(GatewayConnectionState{
		Connected:     false,
		Authenticated: false,
		Reconnecting:  true,
		LastError:     errStr(err),
	})

	// Only reconnect WebSocket; do NOT restart the OpenClaw process.
	// The reconnect lock prevents multiple concurrent reconnect attempts.
	if !m.reconnecting.CompareAndSwap(false, true) {
		m.app.Logger.Info("openclaw: skipping disconnect reconnect, already in progress")
		return
	}

	go func() {
		defer m.reconnecting.Store(false)
		time.Sleep(500 * time.Millisecond)
		m.reconnectClient()
	}()
}

// --- Status broadcasting ---

func (m *Manager) broadcastStatus(s RuntimeStatus) {
	m.mu.Lock()
	// Capture connected state while holding the lock
	connected := m.client != nil
	// Intermediate broadcasts often omit runtime metadata; keep last known values so
	// UI state stays stable during reconnects and errors.
	if s.InstalledVersion == "" && m.status.InstalledVersion != "" {
		switch s.Phase {
		case PhaseStarting, PhaseConnecting, PhaseRestarting, PhaseConnected, PhaseError, PhaseUpgrading:
			s.InstalledVersion = m.status.InstalledVersion
		}
	}
	if s.RuntimeSource == "" && m.status.RuntimeSource != "" {
		switch s.Phase {
		case PhaseStarting, PhaseConnecting, PhaseRestarting, PhaseConnected, PhaseError, PhaseUpgrading:
			s.RuntimeSource = m.status.RuntimeSource
		}
	}
	if s.RuntimePath == "" && m.status.RuntimePath != "" {
		switch s.Phase {
		case PhaseStarting, PhaseConnecting, PhaseRestarting, PhaseConnected, PhaseError, PhaseUpgrading:
			s.RuntimePath = m.status.RuntimePath
		}
	}
	if s.GatewayURL == "" && m.status.GatewayURL != "" {
		s.GatewayURL = m.status.GatewayURL
	}
	m.status = s
	m.mu.Unlock()
	if m.app != nil {
		m.app.Event.Emit(EventStatus, s)
		m.app.Logger.Debug("openclaw: broadcast status",
			"phase", s.Phase,
			"message", s.Message,
			"version", s.InstalledVersion,
			"connected", connected,
		)
	}
}

// runtimeStatusRestarting builds a restarting status while preserving the last known CLI version label.
func (m *Manager) runtimeStatusRestarting() RuntimeStatus {
	m.mu.RLock()
	prev := m.status
	m.mu.RUnlock()
	cfg := m.store.Get()
	return RuntimeStatus{
		Phase:            PhaseRestarting,
		Message:          "OpenClaw Gateway exited, restarting",
		InstalledVersion: prev.InstalledVersion,
		RuntimeSource:    prev.RuntimeSource,
		RuntimePath:      prev.RuntimePath,
		GatewayURL:       gatewayURL(cfg.GatewayPort),
	}
}

// NotifyGatewayRestarting broadcasts PhaseRestarting to the frontend before a gateway
// restart is initiated via CLI. This allows the UI to show "重启中" immediately,
// rather than waiting for the passive polling loop to detect the port/WS change.
// Skips the broadcast if the gateway is still connected — in that case the backend
// naturally reports the real connected state on the next poll.
func (m *Manager) NotifyGatewayRestarting() {
	m.mu.RLock()
	prev := m.status
	stillConnected := m.client != nil
	m.mu.RUnlock()
	// Only override the status if the gateway is actually down.
	// If it's still connected, the backend will correctly report "running" on the next poll.
	if stillConnected {
		return
	}
	cfg := m.store.Get()
	m.broadcastStatus(RuntimeStatus{
		Phase:            PhaseRestarting,
		Message:          "OpenClaw Gateway restarting",
		InstalledVersion: prev.InstalledVersion,
		RuntimeSource:    prev.RuntimeSource,
		RuntimePath:      prev.RuntimePath,
		GatewayURL:       gatewayURL(cfg.GatewayPort),
	})
}

func (m *Manager) broadcastGatewayState(gs GatewayConnectionState) {
	m.mu.Lock()
	m.gatewayState = gs
	m.mu.Unlock()
	if m.app != nil {
		m.app.Event.Emit(EventGatewayState, gs)
	}
}

func (m *Manager) isShuttingDown() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.shuttingDown
}

func (m *Manager) IsReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status.Phase == PhaseConnected
}

// GatewayURL returns the HTTP base URL of the running OpenClaw Gateway.
func (m *Manager) GatewayURL() string {
	return gatewayURL(m.store.Get().GatewayPort)
}

// GatewayToken returns the auth token for the running OpenClaw Gateway.
func (m *Manager) GatewayToken() string {
	return m.store.Get().GatewayToken
}

// CLICommand returns the bundled OpenClaw CLI path and the isolated environment used by ChatClaw.
func (m *Manager) CLICommand() (string, []string, error) {
	bundle, err := resolveBundledRuntime()
	if err != nil {
		return "", nil, err
	}
	return bundle.CLIPath, buildGatewayEnv(m.store.Get(), bundle), nil
}

func (m *Manager) Request(ctx context.Context, method string, params any, out any) error {
	m.mu.RLock()
	client := m.client
	m.mu.RUnlock()
	if client == nil {
		return errors.New("gateway websocket is not connected")
	}
	return client.Request(ctx, method, params, out)
}

// QueryRequest sends a request over the dedicated query connection,
// which is not blocked by long-running agent RPCs on the main connection.
// Falls back to the main client if the query client is unavailable.
func (m *Manager) QueryRequest(ctx context.Context, method string, params any, out any) error {
	m.mu.RLock()
	qc := m.queryClient
	mc := m.client
	m.mu.RUnlock()

	if qc != nil {
		return qc.Request(ctx, method, params, out)
	}
	if mc != nil {
		return mc.Request(ctx, method, params, out)
	}
	return errors.New("gateway websocket is not connected")
}

// SkillsStatus calls the OpenClaw Gateway RPC "skills.status" (protocol schema: SkillsStatusParams).
// Pass empty agentID for the default scope; pass an OpenClaw agent id for that agent's workspace view.
func (m *Manager) SkillsStatus(ctx context.Context, agentID string) (json.RawMessage, error) {
	params := map[string]any{}
	if strings.TrimSpace(agentID) != "" {
		params["agentId"] = strings.TrimSpace(agentID)
	}
	var raw json.RawMessage
	if err := m.QueryRequest(ctx, "skills.status", params, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ExecCLI runs an openclaw CLI subcommand (e.g. "channels", "add", "--channel", "feishu")
// and returns its combined stdout+stderr output. The command inherits the same
// environment as the gateway process so config paths, node path, etc. are correct.
// The gateway does NOT need to restart — channel config changes hot-apply via
// file watcher (see docs/gateway/configuration: "Channels → No restart needed").
// openClawCommand builds an exec.Cmd that runs the OpenClaw CLI.
// On Windows it bypasses openclaw.cmd and calls node.exe directly so that
// CREATE_NO_WINDOW takes effect on the node.exe process itself; on other
// platforms it uses the regular openclaw shell script.
func (m *Manager) openClawCommand(ctx context.Context, bundle *bundledRuntime, args ...string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		entryPath := filepath.Join(bundle.Root, "lib", "node_modules", "openclaw", "dist", "entry.js")
		allArgs := append([]string{entryPath}, args...)
		return exec.CommandContext(ctx, bundle.NodeExePath, allArgs...)
	}
	return exec.CommandContext(ctx, bundle.CLIPath, args...)
}

func (m *Manager) ExecCLI(ctx context.Context, args ...string) ([]byte, error) {
	bundle, err := resolveBundledRuntime()
	if err != nil {
		return nil, fmt.Errorf("resolve openclaw runtime for CLI exec: %w", err)
	}
	cmd := m.openClawCommand(ctx, bundle, args...)
	cmd.Env = buildGatewayEnv(m.store.Get(), bundle)
	cmd.Dir = bundle.Root
	setCmdHideWindow(cmd)
	m.app.Logger.Info("openclaw: exec CLI", "cmd", cmd.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("openclaw CLI %v: %w\n%s", args, err, string(out))
	}
	return out, nil
}

// PrepareCLICommand builds an *exec.Cmd for the bundled openclaw CLI with the same
// environment and working directory as ExecCLI, without starting it.
// Callers may attach StdoutPipe / Stderr and use Start + Wait for interactive flows
// (e.g. WhatsApp QR login).
func (m *Manager) PrepareCLICommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if m == nil {
		return nil, errors.New("openclaw manager is nil")
	}
	bundle, err := resolveBundledRuntime()
	if err != nil {
		return nil, fmt.Errorf("resolve openclaw runtime for CLI exec: %w", err)
	}
	cmd := m.openClawCommand(ctx, bundle, args...)
	cmd.Env = buildGatewayEnv(m.store.Get(), bundle)
	cmd.Dir = bundle.Root
	setCmdHideWindow(cmd)
	return cmd, nil
}

// ExecNpx runs an npx command using the bundled Node.js runtime with the same
// isolated environment as the OpenClaw gateway process.
// On Windows it calls node.exe directly to bypass npx.cmd so CREATE_NO_WINDOW works.
func (m *Manager) ExecNpx(ctx context.Context, args ...string) ([]byte, error) {
	bundle, err := resolveBundledRuntime()
	if err != nil {
		return nil, fmt.Errorf("resolve openclaw runtime for npx exec: %w", err)
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		npxJSPath := filepath.Join(bundle.Root, "tools", "node", "npx.js")
		allArgs := append([]string{npxJSPath}, args...)
		cmd = exec.CommandContext(ctx, bundle.NodeExePath, allArgs...)
	} else {
		npxPath := filepath.Join(bundle.Root, "tools", "node", "bin", "npx")
		cmd = exec.CommandContext(ctx, npxPath, args...)
	}
	cmd.Env = buildGatewayEnv(m.store.Get(), bundle)
	cmd.Dir = bundle.Root
	setCmdHideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Return the raw output to the caller so it can log or display it;
		// keep the error message concise for structured logging.
		return out, fmt.Errorf("npx %v: %w", args, err)
	}
	return out, nil
}

// BundleStateDir returns the state directory (OPENCLAW_STATE_DIR) used by the bundled OpenClaw runtime.
// This is the root for the openclaw.json config, extensions directory, etc.
func (m *Manager) BundleStateDir() (string, error) {
	bundle, err := resolveBundledRuntime()
	if err != nil {
		return "", fmt.Errorf("resolve openclaw runtime: %w", err)
	}
	return bundle.StateDir, nil
}

// AddEventListener registers a listener for gateway events with the given key.
// The caller is responsible for removing it when done via RemoveEventListener.
func (m *Manager) AddEventListener(key string, fn func(event string, payload json.RawMessage)) {
	m.eventListenersMu.Lock()
	defer m.eventListenersMu.Unlock()
	m.eventListeners[key] = fn
}

// RemoveEventListener removes the listener registered under key.
func (m *Manager) RemoveEventListener(key string) {
	m.eventListenersMu.Lock()
	defer m.eventListenersMu.Unlock()
	delete(m.eventListeners, key)
}

// handleLateErrorResponse is called when a second (error) response arrives for
// a request whose initial OK response was already consumed. This happens when
// the Gateway sends an early ack followed by an async error (e.g. sandbox failure).
// We re-dispatch it as a synthetic "agent_late_error" event so chat listeners
// can detect and report the failure.
func (m *Manager) handleLateErrorResponse(resp gatewayResponseFrame) {
	errMsg := ""
	errCode := ""
	if resp.Error != nil {
		errMsg = resp.Error.Message
		errCode = resp.Error.Code
	}
	m.app.Logger.Warn("openclaw: late error response for completed request",
		"id", resp.ID, "code", errCode, "error", errMsg)

	synth := map[string]any{"error": errMsg, "code": errCode}
	if len(resp.Payload) > 0 {
		var extra map[string]any
		if json.Unmarshal(resp.Payload, &extra) == nil {
			for k, v := range extra {
				synth[k] = v
			}
		}
	}
	payload, _ := json.Marshal(synth)
	m.dispatchEvent(GatewayEventFrame{
		Event:   "agent_late_error",
		Payload: payload,
	})
}

func (m *Manager) dispatchEvent(ev GatewayEventFrame) {
	m.eventListenersMu.RLock()
	listeners := make([]EventListener, 0, len(m.eventListeners))
	for _, fn := range m.eventListeners {
		listeners = append(listeners, fn)
	}
	m.eventListenersMu.RUnlock()

	for _, fn := range listeners {
		fn(ev.Event, ev.Payload)
	}
}

func (m *Manager) RegisterReadyHook(fn func()) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readyHooks = append(m.readyHooks, fn)
}

// runDoctorAutoFix triggers "openclaw doctor --fix" and emits the trigger event
// to the frontend so it can show the doctor console. It runs asynchronously
// and does not block the reconnect loop.
func (m *Manager) runDoctorAutoFix() {
	if m.app != nil {
		m.app.Event.Emit(EventTriggerDoctor, map[string]any{
			"reason": "consecutive_ws_failures",
		})
	}

	bundle, err := resolveBundledRuntime()
	if err != nil {
		m.app.Logger.Warn("openclaw: doctor auto-fix: no bundled runtime", "error", err)
		return
	}

	m.app.Logger.Info("openclaw: running doctor auto-fix",
		"runtime", bundle.Root)

	// Run doctor --fix; output streams via EventDoctorOutput as normal.
	_, runErr := m.RunDoctorCommand("doctor --fix --yes --non-interactive", true)
	if runErr != nil {
		m.app.Logger.Warn("openclaw: doctor auto-fix failed", "error", runErr)
	}
	// Reset trigger flag after doctor completes so future failure windows can re-trigger.
	m.mu.Lock()
	m.doctorTriggered = false
	m.mu.Unlock()
}

// RunDoctorCommand executes an openclaw doctor command, streams stdout/stderr via EventDoctorOutput, and returns the final result.
func (m *Manager) RunDoctorCommand(command string, fix bool) (*DoctorCommandResult, error) {
	bundle, err := resolveBundledRuntime()
	if err != nil {
		return nil, fmt.Errorf("resolve openclaw runtime for doctor: %w", err)
	}

	args := []string{"doctor"}
	if fix {
		args = append(args, "--fix", "--yes", "--non-interactive")
	}

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, bundle.CLIPath, args...)
	cmd.Env = buildGatewayEnv(m.store.Get(), bundle)
	cmd.Dir = bundle.Root
	setCmdHideWindow(cmd)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("openclaw doctor stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("openclaw doctor stderr: %w", err)
	}

	runID := atomic.AddUint64(&m.doctorRunSeq, 1)

	var fullStdout strings.Builder
	var fullStderr strings.Builder
	var wg sync.WaitGroup

	emitChunk := func(stream, text string) {
		if m.app == nil || text == "" {
			return
		}
		m.app.Event.Emit(EventDoctorOutput, map[string]any{
			"runId":  runID,
			"stream": stream,
			"text":   text,
		})
	}

	pump := func(r io.Reader, stream string, acc *strings.Builder) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, readErr := r.Read(buf)
			if n > 0 {
				chunk := decodeWindowsConsoleOutput(buf[:n])
				_, _ = acc.WriteString(chunk)
				emitChunk(stream, chunk)
			}
			if readErr == io.EOF {
				return
			}
			if readErr != nil {
				return
			}
		}
	}

	startTime := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("openclaw doctor start: %w", err)
	}

	wg.Add(2)
	go pump(stdoutPipe, "stdout", &fullStdout)
	go pump(stderrPipe, "stderr", &fullStderr)

	waitErr := cmd.Wait()
	wg.Wait()
	duration := time.Since(startTime)

	result := &DoctorCommandResult{
		Command:    command,
		Stdout:     fullStdout.String(),
		Stderr:     fullStderr.String(),
		Duration:   int(duration.Milliseconds()),
		WorkingDir: bundle.Root,
	}

	if waitErr != nil {
		if result.Stderr == "" {
			result.Stderr = waitErr.Error()
		} else {
			result.Stderr = result.Stderr + "\n" + waitErr.Error()
		}
		result.ExitCode = 1
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		}
		result.Fixed = false
	} else {
		result.ExitCode = 0
		result.Fixed = fix
	}

	return result, nil
}

func (m *Manager) notifyReadyHooks() {
	m.mu.RLock()
	hooks := append([]func(){}, m.readyHooks...)
	m.mu.RUnlock()
	for _, fn := range hooks {
		go fn()
	}
}

// --- Helpers ---

// decodeWindowsConsoleOutput converts subprocess bytes to UTF-8 for JSON and the web UI.
// On Chinese Windows, tools like schtasks emit GBK (CP936); Go's string(out) treats bytes as UTF-8
// and invalid sequences become U+FFFD in clients.
func decodeWindowsConsoleOutput(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	if runtime.GOOS != "windows" {
		return string(b)
	}
	r := transform.NewReader(bytes.NewReader(b), simplifiedchinese.GBK.NewDecoder())
	decoded, decErr := io.ReadAll(r)
	if decErr != nil || len(decoded) == 0 {
		return string(b)
	}
	return string(decoded)
}

func verifyInstalled(bundle *bundledRuntime) (string, error) {
	if _, err := os.Stat(bundle.CLIPath); err != nil {
		return "", fmt.Errorf("verify bundled OpenClaw runtime: %w", err)
	}
	verCmd := exec.Command(bundle.CLIPath, "--version")
	setCmdHideWindow(verCmd)
	out, err := verCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("check openclaw version: %w", err)
	}
	version, err := parseVersionOutput(string(out))
	if err != nil {
		return "", err
	}
	if bundle.Manifest.OpenClawVersion != "" && bundle.Manifest.OpenClawVersion != version {
		return "", fmt.Errorf("bundled OpenClaw version mismatch: manifest=%s cli=%s",
			bundle.Manifest.OpenClawVersion, version)
	}
	return version, nil
}

// ensureOpenClawStateDir creates OPENCLAW_STATE_DIR. We intentionally do not run
// `openclaw config set` before gateway start — that pre-writes openclaw.json and races with
// the gateway's own persistence of --auth/--token, causing repeated reload restarts; see
// ResponsesEndpointSection + ConfigService.Sync instead.
func ensureOpenClawStateDir(bundle *bundledRuntime, defaultPort int, gatewayToken string) error {
	if err := os.MkdirAll(bundle.StateDir, 0o700); err != nil {
		return fmt.Errorf("create openclaw state dir: %w", err)
	}
	log := slog.Default()
	// Resolve extraSkills dir once so it can be included in both the new-file baseline
	// and the existing-file patch below.
	extraSkillsDir := ""
	if rtRoot, err := resolveRuntimeRootLocal(); err == nil {
		extraSkillsDir = filepath.Join(rtRoot, "extraSkills")
	}
	// If openclaw.json does not exist, write a minimal baseline config so the gateway
	// can start with a valid port, local mode, token auth, and skills.load.extraDirs.
	// This prevents the "no gateway url / no token" error loop for new users.
	if err := ensureOpenClawDefaultConfig(bundle.ConfigPath, defaultPort, gatewayToken, extraSkillsDir, log); err != nil {
		log.Warn("openclaw: ensure default config failed", "error", err, "config", bundle.ConfigPath)
	}
	// Fix config version downgrade: if openclaw.json was written by a newer version,
	// gateway will refuse to start ("Config was last written by a newer OpenClaw").
	// Remove _config_version field to allow the older runtime to start.
	if err := fixOpenClawConfigVersionIfNeeded(bundle.ConfigPath, bundle.Manifest.OpenClawVersion, log); err != nil {
		// Log but don't fail — gateway startup may still work if config is OK.
		log.Warn("openclaw: fix config version failed", "error", err, "config", bundle.ConfigPath)
	}
	// Clean up corrupted numeric-id model entries from chatwiki provider in openclaw.json.
	// These can accumulate from old catalog sync bugs and must not persist across restarts.
	if err := cleanCorruptedChatWikiModels(bundle.ConfigPath, log); err != nil {
		log.Warn("openclaw: clean corrupted chatwiki models failed", "error", err, "config", bundle.ConfigPath)
	}
	// Clean up deprecated identity fields from agent entries in openclaw.json.
	// OpenClaw 4.26+ no longer supports identity via RPC.
	if err := cleanAgentsIdentity(bundle.ConfigPath, log); err != nil {
		log.Warn("openclaw: clean agents identity failed", "error", err, "config", bundle.ConfigPath)
	}
	// Ensure gateway auth config is present before boot so token auth takes effect.
	if err := ensureGatewayAuthConfig(bundle.ConfigPath, defaultPort, gatewayToken, log); err != nil {
		log.Warn("openclaw: ensure gateway auth config failed", "error", err, "config", bundle.ConfigPath)
	}
	// Ensure skills.load.extraDirs is present in openclaw.json.
	if err := ensureSkillsExtraDirs(bundle.ConfigPath, extraSkillsDir, log); err != nil {
		log.Warn("openclaw: ensure skills extra dirs failed", "error", err, "config", bundle.ConfigPath)
	}
	return nil
}

// ensureOpenClawDefaultConfig writes a minimal openclaw.json if the file does not exist yet.
// This gives new users a working gateway config (port + local mode + token auth) on first start,
// avoiding the "no gateway url / no token" error loop.
func ensureOpenClawDefaultConfig(configPath string, defaultPort int, gatewayToken string, extraSkillsDir string, log *slog.Logger) error {
	if _, statErr := os.Stat(configPath); statErr == nil {
		// File already exists — nothing to do.
		return nil
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("stat config: %w", statErr)
	}

	cfg := map[string]any{
		"gateway": map[string]any{
			"port": defaultPort,
			"mode": "local",
			"auth": map[string]any{
				"mode":   "token",
				"token":  gatewayToken,
			},
		},
	}

	// Include extraSkills dir in the baseline config so new users get the full config.
	if extraSkillsDir != "" {
		cfg["skills"] = map[string]any{
			"load": map[string]any{
				"extraDirs": []any{extraSkillsDir},
			},
		}
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal default config: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		return fmt.Errorf("write default config: %w", err)
	}
	log.Info("openclaw: wrote default openclaw.json",
		"port", defaultPort, "config", configPath)
	return nil
}

// fixOpenClawConfigVersionIfNeeded reads openclaw.json and removes _config_version
// if it is newer than the runtime version. This prevents gateway startup failure
// when rolling back to an older bundled runtime after a failed upgrade.
func fixOpenClawConfigVersionIfNeeded(configPath, runtimeVersion string, log *slog.Logger) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	configVersion, _ := cfg["_config_version"].(string)
	if configVersion == "" {
		return nil
	}
	if isVersionNewerOrEqual(configVersion, runtimeVersion) {
		// Config is from same or newer version — no action needed.
		return nil
	}
	// Config is from a newer version but we're running an older runtime.
	// Remove _config_version to allow gateway to start.
	delete(cfg, "_config_version")
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	log.Info("openclaw: config version downgraded",
		"from", configVersion, "to", runtimeVersion, "config", configPath)
	return nil
}

// cleanCorruptedChatWikiModels removes model entries from the chatwiki provider
// in openclaw.json where both id and name are purely numeric (e.g. {"id":"1","name":"1"}).
// These are remnants of corrupted catalog data that must not persist across restarts.
func cleanCorruptedChatWikiModels(configPath string, log *slog.Logger) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config: %w", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	models, ok := cfg["models"].(map[string]any)
	if !ok {
		return nil
	}
	provs, ok := models["providers"].(map[string]any)
	if !ok {
		return nil
	}
	chatwiki, ok := provs["chatwiki"].(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := chatwiki["models"].([]any)
	if !ok {
		return nil
	}

	original := len(arr)
	cleaned := make([]any, 0, original)
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			cleaned = append(cleaned, item)
			continue
		}
		id, _ := m["id"].(string)
		name, _ := m["name"].(string)
		if isAllDigits(id) && isAllDigits(name) {
			continue // skip corrupted entry
		}
		cleaned = append(cleaned, item)
	}

	if len(cleaned) == original {
		return nil
	}

	chatwiki["models"] = cleaned
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	log.Info("openclaw: cleaned corrupted chatwiki models",
		"removed", original-len(cleaned), "remaining", len(cleaned), "config", configPath)
	return nil
}

// cleanAgentsIdentity removes the deprecated 'identity' field from agent entries
// in openclaw.json. OpenClaw 4.26+ no longer supports identity via RPC,
// and having stale identity config can cause sync failures.
func cleanAgentsIdentity(configPath string, log *slog.Logger) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config: %w", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	agents, ok := cfg["agents"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := agents["list"].([]any)
	if !ok {
		return nil
	}

	cleaned := false
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, hasIdentity := entry["identity"]; hasIdentity {
			delete(entry, "identity")
			cleaned = true
		}
	}

	if !cleaned {
		return nil
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	log.Info("openclaw: cleaned deprecated identity fields from agents config",
		"config", configPath)
	return nil
}

// ensureGatewayAuthConfig ensures openclaw.json has all required gateway fields set
// before boot, so the gateway starts on a consistent port and uses token auth.
//
// Priority for port:
//  1. gateway.port  (top-level, newer OpenClaw 2026.4+)
//  2. gateway.http.port  (legacy location)
//  3. defaultPort (passed in; ChatClaw's stored settings value)
//
// The chosen port is always written back to openclaw.json so the CLI commands
// (devices list/approve) and the gateway agree on the same port after restart.
func ensureGatewayAuthConfig(configPath string, defaultPort int, gatewayToken string, log *slog.Logger) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config for auth patch: %w", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config for auth patch: %w", err)
	}

	gw, _ := cfg["gateway"].(map[string]any)
	if gw == nil {
		gw = map[string]any{}
		cfg["gateway"] = gw
	}

	// Resolve port: check existing config first, then fall back to ChatClaw default.
	port := 0
	if v, ok := gw["port"]; ok {
		if f, ok := v.(float64); ok {
			port = int(f)
		}
	}
	if port == 0 {
		if http, ok := gw["http"].(map[string]any); ok {
			if v, ok := http["port"].(float64); ok {
				port = int(v)
			}
		}
	}
	if port == 0 {
		port = defaultPort
	}
	// Write resolved port back so CLI and gateway always agree.
	gw["port"] = port

	// OpenClaw 2026.4+ requires explicit gateway.mode.
	if _, exists := gw["mode"]; !exists {
		gw["mode"] = "local"
	}

	auth, _ := gw["auth"].(map[string]any)
	if auth == nil {
		auth = map[string]any{}
		gw["auth"] = auth
	}

	// "token" is the only reliable mode in OpenClaw 2026.4+.
	auth["mode"] = "token"
	// Preserve an existing token so previously paired devices stay connected.
	// Only write the ChatClaw token if the config has no token at all.
	if _, exists := auth["token"]; !exists && gatewayToken != "" {
		auth["token"] = gatewayToken
	}

	// Remove stale keys from older config versions so they don't cause validation errors.
	delete(auth, "autoApprove")
	delete(auth, "localAutoApprove")
	delete(auth, "pairingTimeout")

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config after auth patch: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		return fmt.Errorf("write auth to config: %w", err)
	}
	log.Info("openclaw: gateway config patched",
		"config", configPath,
		"port", port,
		"auth_mode", auth["mode"])
	return nil
}

// ensureSkillsExtraDirs ensures openclaw.json has skills.load.extraDirs configured.
// If skills.load.extraDirs is missing or empty, it is set to [extraSkillsDir].
// If it exists but does not contain extraSkillsDir, that path is appended.
// extraSkillsDir should be the resolved absolute path (e.g. C:\soft\ChatClaw\extraSkills).
func ensureSkillsExtraDirs(configPath string, extraSkillsDir string, log *slog.Logger) error {
	if extraSkillsDir == "" {
		return nil
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config: %w", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	// Navigate to skills.load.extraDirs, creating intermediate maps as needed.
	if cfg["skills"] == nil {
		cfg["skills"] = make(map[string]any)
	}
	skillsRaw, _ := cfg["skills"].(map[string]any)
	if skillsRaw == nil {
		skillsRaw = make(map[string]any)
		cfg["skills"] = skillsRaw
	}
	if skillsRaw["load"] == nil {
		skillsRaw["load"] = make(map[string]any)
	}
	load, _ := skillsRaw["load"].(map[string]any)
	if load == nil {
		load = make(map[string]any)
		skillsRaw["load"] = load
	}

	extraDirsVal, ok := load["extraDirs"].([]any)
	if !ok || len(extraDirsVal) == 0 {
		// Empty or absent: set to the default single entry.
		load["extraDirs"] = []any{extraSkillsDir}
		out, _ := json.MarshalIndent(cfg, "", "  ")
		os.WriteFile(configPath, out, 0o644)
		log.Info("openclaw: set skills.load.extraDirs",
			"extraDirs", []string{extraSkillsDir}, "config", configPath)
		return nil
	}

	// Check if the expected directory is already in the list.
	for _, v := range extraDirsVal {
		if s, ok := v.(string); ok && s == extraSkillsDir {
			return nil // already present
		}
	}

	// Not present: append it.
	extraDirsVal = append(extraDirsVal, extraSkillsDir)
	load["extraDirs"] = extraDirsVal
	out, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(configPath, out, 0o644)
	log.Info("openclaw: appended extraSkills to skills.load.extraDirs",
		"extraDirs", extraDirsVal, "config", configPath)
	return nil
}

// resolveRuntimeRootLocal returns the directory containing the running executable.
func resolveRuntimeRootLocal() (string, error) {
	execPath, err := os.Executable()
	if err != nil || strings.TrimSpace(execPath) == "" {
		return "", fmt.Errorf("cannot resolve executable path")
	}
	return filepath.Dir(execPath), nil
}

// isVersionNewerOrEqual returns true if v1 >= v2 (semver comparison).
func isVersionNewerOrEqual(v1, v2 string) bool {
	a1, err1 := semver.NewVersion(strings.TrimSpace(v1))
	a2, err2 := semver.NewVersion(strings.TrimSpace(v2))
	if err1 != nil || err2 != nil {
		// Non-semver: fallback to string comparison
		return strings.TrimSpace(v1) >= strings.TrimSpace(v2)
	}
	return a1.GreaterThan(a2) || a1.Equal(a2)
}

func parseVersionOutput(output string) (string, error) {
	for _, field := range strings.Fields(strings.TrimSpace(output)) {
		candidate := strings.TrimPrefix(strings.Trim(field, "(),"), "v")
		if strings.Count(candidate, ".") >= 2 && isVersionChars(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not parse openclaw version from %q", strings.TrimSpace(output))
}

func isVersionChars(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

func gatewayURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func gatewayWebSocketURL(port int) string {
	return fmt.Sprintf("ws://127.0.0.1:%d/ws", port)
}

func buildGatewayEnv(cfg OpenClawConfig, bundle *bundledRuntime) []string {
	envMap := map[string]string{}
	for _, entry := range os.Environ() {
		if k, v, ok := strings.Cut(entry, "="); ok {
			envMap[k] = v
		}
	}
	rawStreamPath := gatewayRawStreamLogPath(bundle.LogsDir)
	envMap["OPENCLAW_STATE_DIR"] = bundle.StateDir
	envMap["OPENCLAW_CONFIG_PATH"] = bundle.ConfigPath
	envMap["OPENCLAW_SKIP_CANVAS_HOST"] = "1"
	envMap["OPENCLAW_EMBEDDED_IN"] = "ChatClaw"
	envMap["OPENCLAW_RAW_STREAM"] = "1"
	envMap["OPENCLAW_RAW_STREAM_PATH"] = rawStreamPath
	_ = os.Setenv("OPENCLAW_RAW_STREAM", "1")
	_ = os.Setenv("OPENCLAW_RAW_STREAM_PATH", rawStreamPath)

	var pathKey, nodeBin string
	if runtime.GOOS == "windows" {
		pathKey, nodeBin = "Path", filepath.Join(bundle.Root, "tools", "node")
	} else {
		pathKey, nodeBin = "PATH", filepath.Join(bundle.Root, "tools", "node", "bin")
	}
	// Also expose the bundled openclaw CLI itself so that plugin installers
	// (e.g. npx @tencent-weixin/openclaw-weixin-cli install) can invoke `openclaw`.
	cliBin := filepath.Join(bundle.Root, "bin")
	if cur := envMap[pathKey]; cur != "" {
		envMap[pathKey] = cliBin + string(os.PathListSeparator) + nodeBin + string(os.PathListSeparator) + cur
	} else {
		envMap[pathKey] = cliBin + string(os.PathListSeparator) + nodeBin
	}

	result := make([]string, 0, len(envMap))
	for k, v := range envMap {
		result = append(result, k+"="+v)
	}
	return result
}

func gatewayRawStreamLogPath(logsDir string) string {
	return filepath.Join(logsDir, "openclaw-raw-stream.jsonl")
}

func openGatewayLogFile(logsDir string) (*os.File, error) {
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create logs dir: %w", err)
	}
	return os.OpenFile(filepath.Join(logsDir, "openclaw-gateway.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

// ensureSandboxConfigured checks whether Docker is available.
// If Docker is not running, any agent with sandbox.mode="all" is switched to
// "none" so the agent can operate without a container runtime.
func ensureSandboxConfigured(bundle *bundledRuntime) {
	if isDockerAvailable() {
		return
	}

	raw, err := os.ReadFile(bundle.ConfigPath)
	if err != nil {
		return
	}
	var cfg map[string]any
	if json.Unmarshal(raw, &cfg) != nil {
		return
	}

	agents, _ := cfg["agents"].(map[string]any)
	if agents == nil {
		return
	}
	list, _ := agents["list"].([]any)
	if len(list) == 0 {
		return
	}

	modified := false
	for _, item := range list {
		agent, _ := item.(map[string]any)
		if agent == nil {
			continue
		}
		sandbox, _ := agent["sandbox"].(map[string]any)
		if sandbox == nil {
			continue
		}
		if mode, _ := sandbox["mode"].(string); mode == "all" {
			sandbox["mode"] = "off"
			modified = true
		}
	}

	if !modified {
		return
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(bundle.ConfigPath, out, 0o644)
}

func isDockerAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info")
	setCmdHideWindow(cmd)
	return cmd.Run() == nil
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func shouldRetryConnect(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"connection refused", "connection reset", "broken pipe",
		"unexpected eof", "read connect challenge", "dial gateway websocket"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// broadcastUpgradeProgress sends a PhaseUpgrading status with a progress percentage (0-100),
// elapsed time in seconds, and a descriptive message, preserving last known runtime metadata.
func (m *Manager) broadcastUpgradeProgress(progress int, message string) {
	elapsed := -1
	if !m.upgradeStartTime.IsZero() {
		elapsed = int(time.Since(m.upgradeStartTime).Seconds())
	}
	s := RuntimeStatus{
		Phase:          PhaseUpgrading,
		Progress:       progress,
		Message:        message,
		ElapsedSeconds: elapsed,
		UpgradeOutput:  m.upgradeOutputBuf.String(),
	}
	// Preserve last known runtime metadata when phase is upgrading.
	if s.InstalledVersion == "" && m.status.InstalledVersion != "" {
		s.InstalledVersion = m.status.InstalledVersion
	}
	if s.RuntimeSource == "" && m.status.RuntimeSource != "" {
		s.RuntimeSource = m.status.RuntimeSource
	}
	if s.RuntimePath == "" && m.status.RuntimePath != "" {
		s.RuntimePath = m.status.RuntimePath
	}
	if s.GatewayURL == "" && m.status.GatewayURL != "" {
		s.GatewayURL = m.status.GatewayURL
	}
	m.broadcastStatus(s)
}

// appendUpgradeOutput appends a line to the upgrade output buffer and broadcasts it.
// Lines beyond the last 200 are dropped to prevent unbounded growth.
func (m *Manager) appendUpgradeOutput(line string) {
	if line == "" {
		return
	}
	m.upgradeOutputBuf.WriteString(line)
	m.upgradeOutputBuf.WriteString("\n")
	// Keep only the last 200 lines (roughly 20KB at 100 chars/line).
	const maxLines = 200
	lines := strings.Split(m.upgradeOutputBuf.String(), "\n")
	if len(lines) > maxLines+1 {
		m.upgradeOutputBuf.Reset()
		m.upgradeOutputBuf.WriteString(strings.Join(lines[len(lines)-(maxLines+1):], "\n"))
	}
}

// appendStartStep appends a step entry to the startup output buffer.
// The buffer is shared with upgrade output; it is cleared on the first start step
// so that start and upgrade outputs do not mix. Broadcasts the updated output.
func (m *Manager) appendStartStep(stepKey string, detail string) {
	m.mu.Lock()
	// Clear buffer on first step of a new start sequence.
	if m.upgradeOutputBuf.Len() == 0 {
		m.upgradeOutputBuf.Reset()
	}
	if detail != "" {
		m.upgradeOutputBuf.WriteString(fmt.Sprintf("[%s] %s", stepKey, detail))
	} else {
		m.upgradeOutputBuf.WriteString(stepKey)
	}
	m.upgradeOutputBuf.WriteString("\n")
	// Keep only the last 200 lines.
	const maxLines = 200
	content := m.upgradeOutputBuf.String()
	lines := strings.Split(content, "\n")
	if len(lines) > maxLines+1 {
		m.upgradeOutputBuf.Reset()
		m.upgradeOutputBuf.WriteString(strings.Join(lines[len(lines)-(maxLines+1):], "\n"))
	}
	bufLen := m.upgradeOutputBuf.Len()
	m.mu.Unlock()

	cfg := m.store.Get()
	m.broadcastStatus(RuntimeStatus{
		Phase:         PhaseStarting,
		Message:       "Starting OpenClaw Gateway",
		UpgradeOutput: content[:bufLen],
		GatewayURL:    gatewayURL(cfg.GatewayPort),
	})
}

// cleanErrorMessage strips Go stack traces and redundant path noise from error strings
// so the UI shows a concise, user-friendly message instead of a raw technical dump.
func cleanErrorMessage(raw string) string {
	lines := strings.Split(raw, "\n")
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Stop at the first line that looks like a Go stack frame.
		if strings.HasPrefix(trimmed, "chatclaw/") ||
			strings.HasPrefix(trimmed, "github.com/") ||
			strings.HasPrefix(trimmed, "goroutine ") ||
			strings.HasPrefix(trimmed, "created by ") {
			break
		}
		out = append(out, trimmed)
	}
	result := strings.Join(out, " ")
	result = strings.TrimSpace(result)
	if result == "" {
		return "An error occurred while starting the gateway"
	}
	// Truncate very long messages (e.g. long path listings) to a reasonable length.
	const maxLen = 120
	if len(result) > maxLen {
		result = result[:maxLen] + "…"
	}
	return result
}
