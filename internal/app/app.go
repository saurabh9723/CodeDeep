package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/approval"
	"codedeep/internal/config"
	"codedeep/internal/llm"
	"codedeep/internal/mcp"
	"codedeep/internal/session"
	"codedeep/internal/tools"
)

// App holds the long-lived pieces shared by the CLI and the web server.
type App struct {
	Cfg      *config.Config
	Client   llm.Provider
	Registry *tools.Registry
	FS       *tools.FS
	Sessions *session.Store
	Logf     func(format string, args ...any)

	mcpClients []*mcp.Client
}

func New(cfg *config.Config) (*App, error) {
	logf := func(format string, args ...any) {
		if cfg.Debug {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}
	store, err := session.NewStore(cfg.SessionsDir)
	if err != nil {
		return nil, err
	}
	fsys := tools.NewFS(cfg.WorkDir, cfg.AllowOutside)
	registry := tools.NewRegistry()
	tools.RegisterCore(registry, fsys, cfg.Shell)
	client := llm.NewProvider(cfg.Provider, cfg.BaseURL, cfg.APIKey)

	a := &App{
		Cfg:      cfg,
		Client:   client,
		Registry: registry,
		FS:       fsys,
		Sessions: store,
		Logf:     logf,
	}

	if len(cfg.MCPServers) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		clients := mcp.StartAll(ctx, cfg.MCPServers, logf)
		a.mcpClients = clients
		for _, c := range clients {
			c.Register(ctx, registry, logf)
		}
	}
	return a, nil
}

func (a *App) Close() {
	for _, c := range a.mcpClients {
		c.Close()
	}
}

func (a *App) NewAgent(approver approval.Approver, emit agent.Emitter) *agent.Agent {
	return agent.New(agent.Options{
		Config:   a.Cfg,
		Client:   a.Client,
		Registry: a.Registry,
		FS:       a.FS,
		Sessions: a.Sessions,
		Approver: approver,
		Emitter:  emit,
	})
}

type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReadOnly    bool   `json:"read_only"`
}

func (a *App) ToolInfos() []ToolInfo {
	specs := a.Registry.List()
	out := make([]ToolInfo, 0, len(specs))
	for _, s := range specs {
		out = append(out, ToolInfo{
			Name:        s.Name,
			Description: s.Description,
			ReadOnly:    s.ReadOnly,
		})
	}
	return out
}

func (a *App) MCPCount() int { return len(a.mcpClients) }
