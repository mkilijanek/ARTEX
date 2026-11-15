package agent

import (
	"fmt"

	"github.com/Autumn-27/norma/agentcore"
)

// Missing transcripts are fresh sessions in Norma. Any other load failure must
// stop before Run: continuing would append a new conversation to damaged history.
func resumeSession(s *agentcore.Session, id string) error {
	if err := s.Resume(id); err != nil {
		return fmt.Errorf("restore Norma session %q: %w", id, err)
	}
	return nil
}
