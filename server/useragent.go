package server

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"

	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/useragent"
)

// User-Agent settings. The master switch defaults to hidden (on), so an install that never
// touches it keeps sending no identifying header. The values are mirrored into the useragent
// package, which every outbound request consults when it is built.
const (
	settingUAHidden     = "user_agent_hidden"
	settingUACustom     = "user_agent_custom"
	settingUAComponents = "user_agent_components" // JSON list of checked component names; unset = all configurable ones
)

// toolUAEnv is the variable API Recon scripts read. It is the one User-Agent value tool
// processes get (the allow-list in sandbox/env.go passes it through).
const toolUAEnv = "ARTEX_USER_AGENT"

// uaMu serializes everything that stores, reloads and publishes the User-Agent settings (the
// whole PATCH path and the startup load), so two concurrent updates cannot interleave.
var uaMu sync.Mutex

// readUserAgentSettings reads and validates all three settings before anything is published. A
// stored value that no longer validates is an error, not a silent reset.
func readUserAgentSettings(pg *db.DB) (hidden bool, custom string, comps []string, err error) {
	hidden = pg.GetBool(settingUAHidden, true)
	custom, _, err = pg.GetSetting(settingUACustom)
	if err != nil {
		return false, "", nil, fmt.Errorf("read %s: %w", settingUACustom, err)
	}
	custom, err = agent.ValidateUserAgent(custom)
	if err != nil {
		return false, "", nil, fmt.Errorf("stored %s: %w", settingUACustom, err)
	}
	comps = useragent.Configurable()
	raw, ok, err := pg.GetSetting(settingUAComponents)
	if err != nil {
		return false, "", nil, fmt.Errorf("read %s: %w", settingUAComponents, err)
	}
	if ok {
		if err := json.Unmarshal([]byte(raw), &comps); err != nil {
			return false, "", nil, fmt.Errorf("stored %s: %w", settingUAComponents, err)
		}
	}
	return hidden, custom, comps, nil
}

// publishUserAgentLocked reads the settings and publishes them in one atomic store. When they
// cannot be read or validated it publishes hidden=true (keeping the other parts), syncs the tool
// environment and returns the error. The caller holds uaMu.
func publishUserAgentLocked(pg *db.DB) error {
	useragent.SetVersion(BuildVersion)
	hidden, custom, comps, err := readUserAgentSettings(pg)
	if err != nil {
		snap := useragent.Snapshot()
		useragent.Apply(true, snap.Custom, snap.Components)
		syncToolUAEnv()
		return err
	}
	useragent.Apply(hidden, custom, comps)
	syncToolUAEnv()
	return nil
}

// loadUserAgentSettings applies the stored settings at startup and in tests.
func loadUserAgentSettings(pg *db.DB) error {
	uaMu.Lock()
	defer uaMu.Unlock()
	return publishUserAgentLocked(pg)
}

// syncToolUAEnv puts the API Recon User-Agent into this process's environment, or removes it.
// Tool processes copy the environment when they are spawned and the root stage reduces it to its
// allow-list, so a change reaches every process started afterwards. It is set only when the
// resolver returns a value, which also replaces anything the operator exported by hand.
func syncToolUAEnv() {
	if ua := useragent.For("API Recon"); ua != "" {
		_ = os.Setenv(toolUAEnv, ua)
		return
	}
	_ = os.Unsetenv(toolUAEnv)
}

// userAgentPatch is the User-Agent part of the settings PATCH.
type userAgentPatch struct {
	Hidden     *bool     `json:"user_agent_hidden"`
	Custom     *string   `json:"user_agent_custom"`
	Components *[]string `json:"user_agent_components"`
}

func (p userAgentPatch) any() bool { return p.Hidden != nil || p.Custom != nil || p.Components != nil }

// applyUserAgentPatch validates the whole patch first and stores nothing when any part is
// invalid; the error text is for a 400. A store failure is returned as the second value.
func (s *Server) applyUserAgentPatch(p userAgentPatch) (invalid error, failed error) {
	invalid = validateUserAgentPatch(p)
	if invalid != nil {
		return invalid, nil
	}
	uaMu.Lock()
	defer uaMu.Unlock()
	return nil, s.storeUserAgentPatchLocked(p)
}

// validateUserAgentPatch checks every part of the patch without storing anything.
func validateUserAgentPatch(p userAgentPatch) error {
	if p.Custom != nil {
		if _, err := agent.ValidateUserAgent(*p.Custom); err != nil {
			return err
		}
	}
	if p.Components != nil {
		for _, c := range *p.Components {
			if !slices.Contains(useragent.Configurable(), c) {
				return fmt.Errorf("user agent component %q is not configurable", c)
			}
		}
	}
	return nil
}

// storeUserAgentPatchLocked stores the validated patch, then reloads and publishes everything
// at once. The caller holds uaMu.
func (s *Server) storeUserAgentPatchLocked(p userAgentPatch) error {
	pg := s.m.pg
	if p.Hidden != nil {
		if err := pg.SetBool(settingUAHidden, *p.Hidden); err != nil {
			return err
		}
	}
	if p.Custom != nil {
		custom, err := agent.ValidateUserAgent(*p.Custom)
		if err != nil {
			return err
		}
		if err := pg.SetSetting(settingUACustom, custom); err != nil {
			return err
		}
	}
	if p.Components != nil {
		var comps []string
		for _, c := range *p.Components {
			if !slices.Contains(comps, c) {
				comps = append(comps, c)
			}
		}
		b, err := json.Marshal(comps)
		if err != nil {
			return err
		}
		if err := pg.SetSetting(settingUAComponents, string(b)); err != nil {
			return err
		}
	}
	return publishUserAgentLocked(pg)
}

// userAgentPayload is the User-Agent part of the settings GET.
func userAgentPayload() map[string]any {
	snap := useragent.Snapshot()
	return map[string]any{
		"user_agent_hidden":     snap.Hidden,
		"user_agent_custom":     snap.Custom,
		"user_agent_components": snap.Components,
		"user_agent_locked":     useragent.Locked(),
		"user_agent_version":    useragent.Version(),
		"user_agent_effective":  useragent.Effective(),
	}
}
