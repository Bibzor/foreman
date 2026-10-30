package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Bibzor/foreman/internal/acceptance/docker"
	"github.com/Bibzor/foreman/internal/claudecode"
	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/github"
	"github.com/Bibzor/foreman/internal/orchestrator"
	"github.com/Bibzor/foreman/internal/store"
	"github.com/Bibzor/foreman/internal/version"
	"github.com/Bibzor/foreman/internal/workspace"
	"github.com/Bibzor/foreman/skills"
)

// run starts the orchestrator. Logs go to stderr; they never contain the
// private key or a token (both are github.Secret or stay in memory).
func run(args []string, stderr io.Writer) error {
	var cfgPath string
	var once bool
	err := parse("run", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&cfgPath, "config", "", "configuration file (default ~/.config/foreman/config.yml)")
		fs.BoolVar(&once, "once", false, "run a single polling cycle and exit")
	})
	if err != nil {
		return err
	}
	if cfgPath == "" {
		if cfgPath, err = config.DefaultPath(); err != nil {
			return err
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	key, err := github.LoadKey(cfg.PrivateKeyPath)
	if err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "foreman.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // every write is committed already

	gh := github.New(cfg.AppID, key)
	log := slog.New(slog.NewTextHandler(stderr, nil))
	engine := orchestrator.New(orchestrator.Deps{
		Config: cfg,
		GitHub: gh,
		Store:  db,
		Workspace: &workspace.Workspace{
			DataDir: cfg.DataDir,
			Tokens:  gh,
			Skills:  skills.FS,
			Version: skills.PackVersion,
		},
		Agent: claudecode.Qualifier{Claude: &claudecode.Claude{}},
		Developer: claudecode.Developer{
			Claude:  &claudecode.Claude{},
			GitName: "foreman", GitEmail: "foreman@users.noreply.github.com",
		},
		Deployer: &docker.Deployer{Runner: docker.Exec{}, Probe: docker.HTTPProbe},
		Log:      log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("foreman starting", "version", version.Version, "skills", skills.PackVersion, "repos", len(cfg.Repos),
		"wip_limit", cfg.WIPLimit, "once", once)
	log.Warn("project test and lint commands run code written by the agent on this host, as this user, without isolation")
	if !once {
		return engine.Run(ctx)
	}
	if err := engine.Start(ctx); err != nil {
		return err
	}
	if err := engine.Tick(ctx); err != nil {
		return fmt.Errorf("cycle: %w", err)
	}
	return nil
}
