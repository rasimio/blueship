package blueship

import (
	"context"
	"time"

	"github.com/spf13/cobra"
)

// Module is the base interface every feature module implements.
type Module interface {
	Name() string
}

// ToolProvider is implemented by modules that expose LLM tools.
type ToolProvider interface {
	Module
	RegisterTools(r *ToolRegistry, d *Deps)
}

// JobProvider is implemented by modules that run background jobs.
type JobProvider interface {
	Module
	Jobs(d *Deps) []Job
}

// Job describes a periodic background task.
type Job struct {
	Name     string
	Interval time.Duration
	Run      func(ctx context.Context) error
}

// CLIProvider is implemented by modules that add CLI commands.
type CLIProvider interface {
	Module
	RegisterCLI(cmd *cobra.Command, d *Deps)
}

