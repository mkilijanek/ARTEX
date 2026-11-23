package server

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"

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

// loadUserAgentSettings reads the three settings and applies them. A stored value that no longer
// validates is an error, not a silent reset.
func loadUserAgentSettings(pg *db.DB) error {
	useragent.SetVersion(BuildVersion)
	useragent.SetHidden(pg.GetBool(settingUAHidden, true))
	custom, _, err := pg.GetSetting(settingUACustom)
	if err != nil {
		return fmt.Errorf("read %s: %w", settingUACustom, err)
	}
	custom, err = agent.ValidateUserAgent(custom)
	if err != nil {
		return fmt.Errorf("stored %s: %w", settingUACustom, err)
	}
	useragent.SetCustom(custom)
	comps := useragent.Configurable()
	raw, ok, err := pg.GetSetting(settingUAComponents)
	if err != nil {
		return fmt.Errorf("read %s: %w", settingUAComponents, err)
	}
	if ok {
		if err := json.Unmarshal([]byte(raw), &comps); err != nil {
			return fmt.Errorf("stored %s: %w", settingUAComponents, err)
		}
	}
	useragent.SetComponents(comps)
	syncToolUAEnv()
	return nil
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
	var custom string
	if p.Custom != nil {
		c, err := agent.ValidateUserAgent(*p.Custom)
		if err != nil {
			return err, nil
		}
		custom = c
	}
	var comps []string
	if p.Components != nil {
		for _, c := range *p.Components {
			if !slices.Contains(useragent.Configurable(), c) {
				return fmt.Errorf("user agent component %q is not configurable", c), nil
			}
			if !slices.Contains(comps, c) {
				comps = append(comps, c)
			}
		}
	}
	pg := s.m.pg
	if p.Hidden != nil {
		if err := pg.SetBool(settingUAHidden, *p.Hidden); err != nil {
			return nil, err
		}
	}
	if p.Custom != nil {
		if err := pg.SetSetting(settingUACustom, custom); err != nil {
			return nil, err
		}
	}
	if p.Components != nil {
		b, err := json.Marshal(comps)
		if err != nil {
			return nil, err
		}
		if err := pg.SetSetting(settingUAComponents, string(b)); err != nil {
			return nil, err
		}
	}
	if err := loadUserAgentSettings(pg); err != nil {
		return nil, err
	}
	return nil, nil
}

// userAgentPayload is the User-Agent part of the settings GET.
func userAgentPayload() map[string]any {
	return map[string]any{
		"user_agent_hidden":     useragent.Hidden(),
		"user_agent_custom":     useragent.Custom(),
		"user_agent_components": useragent.Components(),
		"user_agent_locked":     useragent.Locked(),
		"user_agent_version":    useragent.Version(),
		"user_agent_effective":  useragent.Effective(),
	}
}
