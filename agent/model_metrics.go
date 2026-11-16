package agent

import (
	"github.com/Autumn-27/artex/llmmeta"
	"github.com/Autumn-27/norma/agentcore"
)

func attachModelMetrics(opts *agentcore.Options) {
	opts.Deps = llmmeta.MeterDeps(opts.Deps, opts.Provider)
	opts.Provider = llmmeta.ForPurpose(opts.Provider, "transcript_summary")
}
