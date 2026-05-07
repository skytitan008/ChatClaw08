package openclawruntime

const (
	EventStatus        = "openclaw:status"
	EventGatewayState  = "openclaw:gateway-state"
	// EventDoctorOutput streams stdout/stderr chunks while RunDoctorCommand runs (runId + stream + text).
	EventDoctorOutput = "openclaw:doctor-output"
	// EventTriggerDoctor is emitted to the frontend when consecutive WS failures exceed the threshold.
	EventTriggerDoctor = "openclaw:trigger-doctor"
)

const (
	PhaseIdle          = "idle"
	PhaseStarting      = "starting"
	PhaseConnecting    = "connecting"
	PhaseConnected     = "connected"
	PhaseRestarting    = "restarting"
	PhaseUpgrading     = "upgrading"
	PhaseError         = "error"
	PhaseNotInstalled  = "not_installed"
)

type RuntimeStatus struct {
	Phase            string `json:"phase"`
	Message          string `json:"message,omitempty"`
	Progress         int    `json:"progress,omitempty"` // 0-100, only meaningful during upgrade
	ElapsedSeconds   int    `json:"elapsedSeconds,omitempty"` // seconds since upgrade started, -1 when not upgrading
	UpgradeOutput    string `json:"upgradeOutput,omitempty"`   // accumulated command output lines
	InstalledVersion string `json:"installedVersion,omitempty"`
	RuntimeSource    string `json:"runtimeSource,omitempty"`
	RuntimePath      string `json:"runtimePath,omitempty"`
	GatewayPID       int    `json:"gatewayPid,omitempty"`
	GatewayURL       string `json:"gatewayURL,omitempty"`
}

type GatewayConnectionState struct {
	Connected     bool   `json:"connected"`
	Authenticated bool   `json:"authenticated"`
	Reconnecting  bool   `json:"reconnecting"`
	LastError     string `json:"lastError,omitempty"`
}

type RuntimeUpgradeResult struct {
	PreviousVersion    string `json:"previousVersion,omitempty"`
	CurrentVersion     string `json:"currentVersion,omitempty"`
	LatestVersion      string `json:"latestVersion,omitempty"`
	Upgraded           bool   `json:"upgraded"`
	RuntimeSource      string `json:"runtimeSource,omitempty"`
	RuntimePath        string `json:"runtimePath,omitempty"`
	HasExistingVersion bool   `json:"hasExistingVersion,omitempty"` // true if a staging dir for latestVersion already exists
	ExistingVersion    string `json:"existingVersion,omitempty"`     // version in existing staging dir (same as latestVersion)
}

// UpgradeAction signals the frontend which upgrade path to take.
type UpgradeAction string

const (
	UpgradeActionContinue UpgradeAction = "continue" // staging dir exists with all files, resume from npm install
	UpgradeActionRestart   UpgradeAction = "restart"  // delete staging dir and rebuild from scratch
)

type DoctorCommandResult struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Duration   int    `json:"duration"` // in milliseconds
	Fixed      bool   `json:"fixed,omitempty"` // whether issues were fixed
	WorkingDir string `json:"workingDir,omitempty"`
}

// GatewayStatusResult represents the parsed output of `openclaw gateway status` command.
type GatewayStatusResult struct {
	Running    bool   `json:"running"`
	PID        int    `json:"pid"`
	Version    string `json:"version,omitempty"`
	Port       int    `json:"port"`
	AuthToken  string `json:"authToken,omitempty"`
	URL        string `json:"url,omitempty"`
	RawOutput  string `json:"rawOutput,omitempty"`
}
