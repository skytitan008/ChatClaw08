package openclawruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatclaw/internal/define"
	openclawagents "chatclaw/internal/openclaw/agents"

	"github.com/wailsapp/wails/v3/pkg/application"
)

const longTermMemoryFileName = "MEMORY.md"

// AgentService handles agent CRUD via Gateway RPC (agents.*) and registers
// an "agents" config section with ConfigService so that agent list entries
// are included in the unified config.patch.
type AgentService struct {
	app       *application.App
	manager   *Manager
	agentsSvc *openclawagents.OpenClawAgentsService
	configSvc *ConfigService

	mu            sync.Mutex
	initialSynced bool
}

func NewAgentService(
	app *application.App,
	manager *Manager,
	agentsSvc *openclawagents.OpenClawAgentsService,
	configSvc *ConfigService,
) *AgentService {
	s := &AgentService{
		app:       app,
		manager:   manager,
		agentsSvc: agentsSvc,
		configSvc: configSvc,
	}
	configSvc.Register("agents", s.buildAgentsSection)
	return s
}

// OnAgentCreated is called directly after a new agent is inserted in DB.
// Calls agents.create RPC, then config.patch to sync the agent list (including model).
func (s *AgentService) OnAgentCreated(agent openclawagents.OpenClawAgent) {
	if !s.manager.IsReady() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := s.createAgent(ctx, agent); err != nil {
		s.app.Logger.Warn("openclaw: agents.create failed", "error", err)
	}
	if err := ensureLongTermMemoryFile(s.resolveAgentWorkspace(agent)); err != nil {
		s.app.Logger.Warn("openclaw: ensure MEMORY.md after create failed", "error", err)
	}
	if err := s.configSvc.Sync(ctx); err != nil {
		s.app.Logger.Warn("openclaw: config sync after create failed", "error", err)
	}
}

// OnAgentUpdated is called directly after an agent is updated in DB.
// Calls agents.update RPC (for name/workspace/model) then config.patch (for advanced settings).
func (s *AgentService) OnAgentUpdated(agent openclawagents.OpenClawAgent) {
	if !s.manager.IsReady() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := s.updateAgent(ctx, agent); err != nil {
		s.app.Logger.Warn("openclaw: agents.update failed", "error", err)
	}
	if err := s.configSvc.Sync(ctx); err != nil {
		s.app.Logger.Warn("openclaw: config sync after update failed", "error", err)
	}
}

// EnsureAgentSynced creates the agent on the gateway if agents.list does not include it.
func (s *AgentService) EnsureAgentSynced(agent openclawagents.OpenClawAgent) error {
	if s.manager == nil || !s.manager.IsReady() {
		return nil
	}
	agentID := strings.TrimSpace(agent.OpenClawAgentID)
	if agentID == "" {
		return fmt.Errorf("openclaw agent local id %d: empty openclaw_agent_id", agent.ID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var gatewayList agentsListResult
	if err := s.manager.Request(ctx, "agents.list", map[string]any{}, &gatewayList); err != nil {
		return fmt.Errorf("agents.list: %w", err)
	}
	normalized := strings.ToLower(agentID)
	for _, entry := range gatewayList.Agents {
		if strings.ToLower(entry.ID) == normalized {
			wsDir := s.resolveAgentWorkspace(agent)
			return ensureLongTermMemoryFile(wsDir)
		}
	}

	if err := s.createAgent(ctx, agent); err != nil {
		return fmt.Errorf("agents.create %s: %w", agentID, err)
	}
	if err := s.configSvc.Sync(ctx); err != nil {
		return fmt.Errorf("config sync after agents.create: %w", err)
	}
	wsDir := s.resolveAgentWorkspace(agent)
	return ensureLongTermMemoryFile(wsDir)
}

// OnAgentDeleted is called directly after an agent is deleted from DB.
// It calls agents.delete on Gateway, then config.patch so agents.list matches DB
// (same as OnAgentUpdated which syncs after RPC).
func (s *AgentService) OnAgentDeleted(openclawAgentID string) {
	if !s.manager.IsReady() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := s.deleteAgent(ctx, openclawAgentID); err != nil {
		s.app.Logger.Warn("openclaw: agents.delete failed", "error", err)
	}
	if err := s.configSvc.Sync(ctx); err != nil {
		s.app.Logger.Warn("openclaw: config sync after delete failed", "error", err)
	}
}

// Sync does a full reconciliation (used on initial Gateway connect).
func (s *AgentService) Sync() {
	if s.manager == nil || s.agentsSvc == nil {
		return
	}
	if !s.manager.IsReady() {
		return
	}
	if err := s.syncOnce(); err != nil {
		s.app.Logger.Warn("openclaw: agent sync failed", "error", err)
	}
}

// OnGatewayReady is meant to be registered as a ready hook on Manager.
// Only the first connection triggers a sync to push initial DB state.
func (s *AgentService) OnGatewayReady() {
	s.mu.Lock()
	if s.initialSynced {
		s.mu.Unlock()
		return
	}
	s.initialSynced = true
	s.mu.Unlock()

	s.Sync()
}

// syncOnce ensures agents exist in Gateway, reconciles creates/deletes,
// then asks ConfigService to push the merged config (models + agents).
func (s *AgentService) syncOnce() error {
	if err := s.agentsSvc.EnsureMainAgent(); err != nil {
		return err
	}

	desiredAgents, err := s.agentsSvc.ListAgentsForOpenClawSync()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var gatewayList agentsListResult
	if err := s.manager.Request(ctx, "agents.list", map[string]any{}, &gatewayList); err != nil {
		return fmt.Errorf("agents.list: %w", err)
	}

	gatewayMap := make(map[string]agentsListEntry, len(gatewayList.Agents))
	for _, entry := range gatewayList.Agents {
		gatewayMap[strings.ToLower(entry.ID)] = entry
	}

	desiredMap := make(map[string]struct{}, len(desiredAgents))
	for _, a := range desiredAgents {
		desiredMap[strings.ToLower(a.OpenClawAgentID)] = struct{}{}
	}

	for _, agent := range desiredAgents {
		agentID := agent.OpenClawAgentID
		normalizedID := strings.ToLower(agentID)
		_, existsInGateway := gatewayMap[normalizedID]

		wsDir := s.resolveAgentWorkspace(agent)
		wsMissing := false
		if _, statErr := os.Stat(wsDir); os.IsNotExist(statErr) {
			wsMissing = true
		}

		if wsMissing && agentID == define.OpenClawMainAgentID {
			if err := os.MkdirAll(wsDir, 0o755); err != nil {
				return fmt.Errorf("mkdir workspace %s: %w", wsDir, err)
			}
		} else if wsMissing || !existsInGateway {
			if err := s.createAgent(ctx, agent); err != nil {
				return fmt.Errorf("agents.create %s: %w", agentID, err)
			}
		}
		if err := ensureLongTermMemoryFile(wsDir); err != nil {
			return fmt.Errorf("ensure %s for %s: %w", longTermMemoryFileName, agentID, err)
		}
	}

	for _, entry := range gatewayList.Agents {
		normalizedEntryID := strings.ToLower(entry.ID)
		if !isManagedAgentID(normalizedEntryID) {
			continue
		}
		if _, wanted := desiredMap[normalizedEntryID]; wanted {
			continue
		}
		if entry.ID == define.OpenClawMainAgentID {
			continue
		}
		if err := s.deleteAgent(ctx, entry.ID); err != nil {
			return fmt.Errorf("agents.delete %s: %w", entry.ID, err)
		}
	}

	if err := s.configSvc.Sync(ctx); err != nil {
		return fmt.Errorf("config sync: %w", err)
	}

	return nil
}

// buildAgentsSection is the SectionBuilder registered with ConfigService.
// It produces {"agents": {"list": [...]}} from the DB state.
func (s *AgentService) buildAgentsSection(ctx context.Context) (map[string]any, error) {
	desiredAgents, err := s.agentsSvc.ListAgentsForOpenClawSync()
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}

	list := make([]any, 0, len(desiredAgents))
	for _, agent := range desiredAgents {
		list = append(list, s.buildAgentEntry(agent))
	}

	return map[string]any{
		"agents": map[string]any{"list": list},
	}, nil
}

func (s *AgentService) buildAgentEntry(agent openclawagents.OpenClawAgent) map[string]any {
	entry := map[string]any{
		"id":        agent.OpenClawAgentID,
		"name":      agent.Name,
		"workspace": s.resolveAgentWorkspace(agent),
		"agentDir":  s.resolveAgentDir(agent),
	}
	if agent.OpenClawAgentID == define.OpenClawMainAgentID {
		entry["default"] = true
	}

	if agent.DefaultLLMProviderID != "" && agent.DefaultLLMModelID != "" {
		entry["model"] = agent.DefaultLLMProviderID + "/" + agent.DefaultLLMModelID
	}

	if agent.SandboxMode != "" {
		entry["sandbox"] = map[string]any{"mode": agent.SandboxMode}
	}

	if agent.GroupChatMentionPatterns != "" && agent.GroupChatMentionPatterns != "[]" {
		var patterns []string
		if json.Unmarshal([]byte(agent.GroupChatMentionPatterns), &patterns) == nil && len(patterns) > 0 {
			entry["groupChat"] = map[string]any{"mentionPatterns": patterns}
		}
	}

	if agent.ToolsProfile != "" || (agent.ToolsAllow != "" && agent.ToolsAllow != "[]") || (agent.ToolsDeny != "" && agent.ToolsDeny != "[]") {
		tools := map[string]any{}
		if agent.ToolsProfile != "" {
			tools["profile"] = agent.ToolsProfile
		}
		if agent.ToolsAllow != "" && agent.ToolsAllow != "[]" {
			var allow []string
			if json.Unmarshal([]byte(agent.ToolsAllow), &allow) == nil && len(allow) > 0 {
				tools["allow"] = allow
			}
		}
		if agent.ToolsDeny != "" && agent.ToolsDeny != "[]" {
			var deny []string
			if json.Unmarshal([]byte(agent.ToolsDeny), &deny) == nil && len(deny) > 0 {
				tools["deny"] = deny
			}
		}
		if len(tools) > 0 {
			entry["tools"] = tools
		}
	}

	if agent.HeartbeatEvery != "" {
		entry["heartbeat"] = map[string]any{"every": agent.HeartbeatEvery}
	}

	if agent.ParamsTemperature != "" || agent.ParamsMaxTokens != "" {
		params := map[string]any{}
		if agent.ParamsTemperature != "" {
			if v, err := strconv.ParseFloat(agent.ParamsTemperature, 64); err == nil {
				params["temperature"] = v
			}
		}
		if agent.ParamsMaxTokens != "" {
			if v, err := strconv.Atoi(agent.ParamsMaxTokens); err == nil {
				params["maxTokens"] = v
			}
		}
		if len(params) > 0 {
			entry["params"] = params
		}
	}

	return entry
}

// --- Agent directory/CRUD helpers ---

func (s *AgentService) resolveAgentWorkspace(agent openclawagents.OpenClawAgent) string {
	return filepath.Join(s.stateDir(), "workspace-"+agent.OpenClawAgentID)
}

func (s *AgentService) resolveAgentDir(agent openclawagents.OpenClawAgent) string {
	return filepath.Join(s.stateDir(), "agents", agent.OpenClawAgentID, "agent")
}

func (s *AgentService) stateDir() string {
	dir := s.agentsSvc.GetDefaultWorkDir()
	if dir == "" {
		return "."
	}
	return dir
}

func (s *AgentService) createAgent(ctx context.Context, agent openclawagents.OpenClawAgent) error {
	workspace := s.resolveAgentWorkspace(agent)
	if err := ensureAgentWorkspaceStateDir(workspace); err != nil {
		return fmt.Errorf("prepare agent workspace %s: %w", workspace, err)
	}

	params := map[string]any{
		"name":      agent.OpenClawAgentID,
		"workspace": workspace,
	}
	if agent.DefaultLLMProviderID != "" && agent.DefaultLLMModelID != "" {
		params["model"] = agent.DefaultLLMProviderID + "/" + agent.DefaultLLMModelID
	}

	// Note: OpenClaw 4.26+ no longer supports identity via RPC.
	// identity emoji/theme are stored locally but not synced to Gateway.

	var resp map[string]any
	sendCreate := func() error {
		resp = nil
		return s.manager.Request(ctx, "agents.create", params, &resp)
	}
	if err := sendCreate(); err != nil {
		if isAgentAlreadyExistsError(err) {
			// Another concurrent flow may have created it already.
			s.app.Logger.Info("openclaw: agents.create skipped because agent already exists",
				"agentId", agent.OpenClawAgentID,
				"workspace", workspace)
		} else if isWorkspaceStateRenameENOENT(err) {
			// OpenClaw occasionally fails the first create with:
			// ENOENT ... rename workspace-state.json.tmp -> workspace-state.json.
			// Ensure workspace/.openclaw and retry once to avoid surfacing a transient bind error.
			s.app.Logger.Warn("openclaw: agents.create hit transient workspace-state ENOENT, retrying once",
				"agentId", agent.OpenClawAgentID,
				"workspace", workspace,
				"error", err)
			if ensureErr := ensureAgentWorkspaceStateDir(workspace); ensureErr != nil {
				return fmt.Errorf("re-prepare agent workspace %s: %w", workspace, ensureErr)
			}
			time.Sleep(250 * time.Millisecond)
			if retryErr := sendCreate(); retryErr != nil {
				if !isAgentAlreadyExistsError(retryErr) {
					return retryErr
				}
				s.app.Logger.Info("openclaw: agents.create retry resolved as already-exists",
					"agentId", agent.OpenClawAgentID,
					"workspace", workspace)
			}
		} else {
			return err
		}
	}
	// agents.create derives the agent ID from the name param, so we pass
	// OpenClawAgentID above. Now set the human-readable display name via update.
	if agent.Name != agent.OpenClawAgentID {
		return s.updateAgent(ctx, agent)
	}
	return nil
}

func (s *AgentService) updateAgent(ctx context.Context, agent openclawagents.OpenClawAgent) error {
	params := map[string]any{
		"agentId": agent.OpenClawAgentID,
	}
	params["name"] = agent.Name
	params["workspace"] = s.resolveAgentWorkspace(agent)

	// Note: OpenClaw 4.26+ no longer supports identity via RPC.
	// identity emoji/theme are stored locally but not synced to Gateway.

	if agent.DefaultLLMProviderID != "" && agent.DefaultLLMModelID != "" {
		params["model"] = agent.DefaultLLMProviderID + "/" + agent.DefaultLLMModelID
	}
	var resp map[string]any
	return s.manager.Request(ctx, "agents.update", params, &resp)
}

func (s *AgentService) deleteAgent(ctx context.Context, agentID string) error {
	return s.manager.Request(ctx, "agents.delete", map[string]any{
		"agentId":     agentID,
		"deleteFiles": false,
	}, nil)
}

// --- Shared types (used by agent syncer previously, now here) ---

type agentsListResult struct {
	DefaultID string            `json:"defaultId"`
	Agents    []agentsListEntry `json:"agents"`
}

type agentsListEntry struct {
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Identity *agentsListIdentity `json:"identity,omitempty"`
}

type agentsListIdentity struct {
	Name string `json:"name,omitempty"`
}

func isManagedAgentID(id string) bool {
	if define.OpenClawManagedAgentIDPrefix == "" {
		return true
	}
	return id == define.OpenClawMainAgentID || strings.HasPrefix(id, define.OpenClawManagedAgentIDPrefix)
}

func ensureLongTermMemoryFile(workspaceDir string) error {
	if strings.TrimSpace(workspaceDir) == "" {
		return fmt.Errorf("workspace path is empty")
	}
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return err
	}

	path := filepath.Join(workspaceDir, longTermMemoryFileName)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	return os.WriteFile(path, nil, 0o644)
}

func ensureAgentWorkspaceStateDir(workspaceDir string) error {
	workspaceDir = strings.TrimSpace(workspaceDir)
	if workspaceDir == "" {
		return fmt.Errorf("workspace path is empty")
	}
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(workspaceDir, ".openclaw"), 0o755)
}

func isWorkspaceStateRenameENOENT(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "enoent") {
		return false
	}
	if !strings.Contains(msg, "workspace-state.json") {
		return false
	}
	return strings.Contains(msg, "rename")
}

func isAgentAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "already exists") {
		return false
	}
	return strings.Contains(msg, "agent")
}
