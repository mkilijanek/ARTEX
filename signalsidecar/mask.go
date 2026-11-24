package signalsidecar

import (
	"regexp"
	"strings"
)

// MaxGroupNameRunes is the length a group name is cut to.
const MaxGroupNameRunes = 128

var (
	// Link URIs run to the next whitespace or quote; the whole run is dropped
	// because its parameters are the secret.
	maskLinkRe = regexp.MustCompile(`(?i)(?:sgnl://|tsdevice:)[^\s"'<>` + "`" + `]*`)
	// A group id as signal-cli prints it: standard or URL-safe base64 of 32
	// bytes, padded, with or without the group. prefix ARTEX uses.
	maskGroupRe    = regexp.MustCompile(`group\.[A-Za-z0-9+/=_-]+`)
	maskRawGroupRe = regexp.MustCompile(`[A-Za-z0-9+/_-]{43}=`)
	maskUUIDRe     = regexp.MustCompile(`[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}`)
	maskNumberRe   = regexp.MustCompile(`\+?[1-9][0-9]{6,14}`)
)

// Scrub masks link URIs, group ids, account ids and phone numbers in free
// text (child output, error texts) so that a line can be logged or stored.
// Numbers keep their last four digits, ids their last four characters.
func Scrub(s string) string {
	if s == "" {
		return s
	}
	s = maskLinkRe.ReplaceAllString(s, "sgnl://…")
	s = maskGroupRe.ReplaceAllStringFunc(s, MaskGroup)
	s = maskRawGroupRe.ReplaceAllStringFunc(s, MaskGroup)
	s = maskUUIDRe.ReplaceAllStringFunc(s, maskTail)
	return maskNumberRe.ReplaceAllStringFunc(s, maskTail)
}

// MaskNumber renders a phone number as …0101.
func MaskNumber(n string) string { return maskTail(n) }

// MaskGroup renders a group id, with or without the group. prefix, as
// group.…abcd (padding ignored).
func MaskGroup(id string) string {
	id = strings.TrimPrefix(id, "group.")
	return "group." + maskTail(strings.TrimRight(id, "="))
}

func maskTail(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "…"
	}
	return "…" + string(r[len(r)-4:])
}

// IsStripped reports whether a code point is removed from group names. The
// set is a fixed list, not "every format character", so that the database
// side can use the same list in a regular expression:
// U+0001-U+001F, U+007F-U+009F, U+00AD, U+061C, U+180E, U+200B-U+200F,
// U+2028-U+202E, U+2060-U+206F, U+FEFF, U+FFF9-U+FFFB, U+E0000-U+E007F.
func IsStripped(r rune) bool {
	switch {
	case r >= 0x0001 && r <= 0x001F,
		r >= 0x007F && r <= 0x009F,
		r == 0x00AD, r == 0x061C, r == 0x180E,
		r >= 0x200B && r <= 0x200F,
		r >= 0x2028 && r <= 0x202E,
		r >= 0x2060 && r <= 0x206F,
		r == 0xFEFF,
		r >= 0xFFF9 && r <= 0xFFFB,
		r >= 0xE0000 && r <= 0xE007F:
		return true
	}
	return false
}

// CleanGroupName removes the stripped set from a group name and cuts the
// result to MaxGroupNameRunes characters. Invalid UTF-8 becomes U+FFFD.
func CleanGroupName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range name {
		if IsStripped(r) {
			continue
		}
		if n == MaxGroupNameRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
