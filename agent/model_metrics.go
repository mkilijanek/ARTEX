package agent

import (
	"github.com/Autumn-27/norma/agentcore"
	"github.com/mkilijanek/artex/llmmeta"
)

func attachModelMetrics(opts *agentcore.Options) {
	attachPrefixCache(opts)
	opts.Deps = llmmeta.MeterDeps(opts.Deps, opts.Provider)
	opts.Provider = llmmeta.ForPurpose(opts.Provider, "transcript_summary")
	attachNormaTrust(opts)
	attachWebFetchPolicy(opts)
}
