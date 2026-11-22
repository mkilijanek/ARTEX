package agent

import (
	"fmt"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/transcript"
)

// ReasonSessionRestore is an ARTEX integration failure, not a retryable model error.
const ReasonSessionRestore harness.TerminalReason = "session_restore_error"

// ReasonSessionStart is an ARTEX integration failure too: the session guard refused the session's
// options (a missing tool list, for example), so nothing was started and no model request was sent.
const ReasonSessionStart harness.TerminalReason = "session_start_error"

// Missing transcripts are fresh sessions in Norma. Any other load failure must
// stop before Run: continuing would append a new conversation to damaged history.
func resumeSession(s *agentcore.Session, id string, stores ...*transcript.Store) error {
	if err := s.Resume(id); err != nil {
		path := "(transcript path unavailable)"
		if len(stores) > 0 && stores[0] != nil {
			path = stores[0].MainPath(id)
		}
		return fmt.Errorf("restore Norma session %q at %q: %w; stop the session and restore a verified transcript backup before retrying (see docs/norma-sdk-audit.md)", id, path, err)
	}
	return nil
}
