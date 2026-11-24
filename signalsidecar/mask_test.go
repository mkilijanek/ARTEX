package signalsidecar

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	probeURI    = "sgnl://linkdevice?uuid=ZmFrZS11dWlkLWZvci10ZXN0cw&pub_key=BQabcdefghijklmnopqrstuvwxyz0123456789%2BABCDEF&capabilities=a,b"
	probeNumber = "+48600100101"
	probeGroup  = "group.dGhpcy1pcy1hLXRlc3QtZ3JvdXAtaWQtMzItYnl0ZXM="
	probeRawID  = "dGhpcy1pcy1hLXRlc3QtZ3JvdXAtaWQtMzItYnl0ZXM="
	probeUUID   = "11111111-2222-4333-8444-555555555555"
)

func TestScrubMasksSecrets(t *testing.T) {
	cases := []struct {
		name, in string
		gone     []string
		want     []string
	}{
		{"link uri", "link is " + probeURI + " now", []string{"ZmFrZS11dWlk", "pub_key=", "BQabcdef"}, []string{"sgnl://"}},
		{"link uri in quotes", `uri="` + probeURI + `"`, []string{"pub_key", "ZmFrZS11dWlk"}, nil},
		{"link uri uppercase scheme", "SGNL://linkdevice?uuid=abc&pub_key=def", []string{"pub_key", "abc"}, nil},
		{"legacy device uri", "tsdevice:/?uuid=abc&pub_key=def end", []string{"pub_key", "abc"}, []string{"end"}},
		{"number with plus", "account " + probeNumber + " failed", []string{probeNumber, "4860010"}, []string{"…0101", "failed"}},
		{"number without plus", "account 48600100101 failed", []string{"48600100101"}, []string{"…0101"}},
		{"group prefixed", "group " + probeGroup + " unknown", []string{probeRawID}, []string{"group.…0ZXM", "unknown"}},
		{"group bare padded id", "id " + probeRawID + " x", []string{probeRawID}, nil},
		{"uuid", "aci " + probeUUID + " ok", []string{probeUUID, "11111111"}, []string{"…5555"}},
		{"several on one line", probeNumber + " " + probeGroup + " " + probeURI, []string{probeNumber, probeRawID, "pub_key"}, nil},
		{"unicode text kept", "zażółć gęślą jaźń", nil, []string{"zażółć gęślą jaźń"}},
		{"empty", "", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			for _, g := range tc.gone {
				if strings.Contains(got, g) {
					t.Errorf("output %q still contains %q", got, g)
				}
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("output %q lacks %q", got, w)
				}
			}
			if !utf8.ValidString(got) {
				t.Errorf("output is not valid UTF-8: %q", got)
			}
			if again := Scrub(got); again != got {
				t.Errorf("not idempotent: %q then %q", got, again)
			}
		})
	}
}

func TestMaskHelpers(t *testing.T) {
	if got := MaskNumber(probeNumber); got != "…0101" {
		t.Errorf("MaskNumber = %q", got)
	}
	if got := MaskNumber("123"); got != "…" {
		t.Errorf("short number: %q", got)
	}
	if got := MaskGroup(probeGroup); got != "group.…0ZXM" {
		t.Errorf("MaskGroup = %q", got)
	}
	if got := MaskGroup("group.abcdefgh=="); got != "group.…efgh" {
		t.Errorf("padding must be ignored: %q", got)
	}
	if got := MaskGroup("abcdefgh"); got != "group.…efgh" {
		t.Errorf("bare id: %q", got)
	}
}

// strippedRanges is the stripped set written out independently of the code.
var strippedRanges = [][2]rune{
	{0x0001, 0x001F}, {0x007F, 0x009F}, {0x00AD, 0x00AD}, {0x061C, 0x061C}, {0x180E, 0x180E},
	{0x200B, 0x200F}, {0x2028, 0x202E}, {0x2060, 0x206F}, {0xFEFF, 0xFEFF}, {0xFFF9, 0xFFFB},
	{0xE0000, 0xE007F},
}

func TestIsStrippedMatchesTheContractForEveryCodePoint(t *testing.T) {
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		want := false
		for _, rg := range strippedRanges {
			if r >= rg[0] && r <= rg[1] {
				want = true
				break
			}
		}
		if got := IsStripped(r); got != want {
			t.Fatalf("U+%04X: IsStripped = %v, want %v", r, got, want)
		}
	}
}

func TestCleanGroupName(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "Ops team", "Ops team"},
		{"controls and bidi removed", "Ops\u202Eteam\u200B\x01\x1f\u2066x\uFEFF\U000E0041", "Opsteamx"},
		{"newline removed", "a\nb\tc", "abc"},
		{"nbsp and normal unicode kept", "a\u00A0b za\u017c\u00f3\u0142\u0107 \u65e5\u672c", "a\u00A0b za\u017c\u00f3\u0142\u0107 \u65e5\u672c"},
		{"empty", "", ""},
		{"cut at 128 characters", strings.Repeat("ż", 200), strings.Repeat("ż", 128)},
		{"cut counts after removal", strings.Repeat("\u200B", 300) + strings.Repeat("a", 130), strings.Repeat("a", 128)},
		{"invalid utf-8 replaced", "a\xffb", "a\uFFFDb"},
	}
	for _, tc := range cases {
		if got := CleanGroupName(tc.in); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
