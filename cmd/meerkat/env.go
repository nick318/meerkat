// The closed set of environment tags a connection can wear: prod,
// staging, dev. The set is closed on purpose — the tags exist to be
// compared across connections, and free text would give every database
// its own spelling of "prod".
//
// Two screens share it: the connections screen (the form's chips, the
// group headings) and the shell, where the tag colors the window frame
// so a prod session can never be mistaken for a dev one. Each tag maps
// to four `theme` tones: ring, inner, surface, text.

package main

import (
	"strings"

	"github.com/nick318/meerkat/internal/theme"
)

// Env is one environment tag.
type Env int

const (
	EnvProd Env = iota
	EnvStaging
	EnvDev
)

// EnvAll is the form's order, and the groups' order (`Env::ALL`).
var EnvAll = [3]Env{EnvProd, EnvStaging, EnvDev}

// AsStr is the tag as it is stored.
func (e Env) AsStr() string {
	switch e {
	case EnvProd:
		return "prod"
	case EnvStaging:
		return "staging"
	case EnvDev:
		return "dev"
	}
	return ""
}

// parseEnvOpt (Rust: `Env::parse`): a stored tag this build does not know
// reads as untagged, never as an error: the file may have been written by
// a newer build. A nil `tag` is `None`.
func parseEnvOpt(tag *string) (Env, bool) {
	if tag == nil {
		return 0, false
	}
	switch envASCIILower(strings.TrimSpace(*tag)) {
	case "prod":
		return EnvProd, true
	case "staging":
		return EnvStaging, true
	case "dev":
		return EnvDev, true
	}
	return 0, false
}

// envASCIILower is `str::to_ascii_lowercase`: ASCII letters only.
func envASCIILower(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if 'A' <= ch && ch <= 'Z' {
			b[i] = ch + ('a' - 'A')
		}
	}
	return string(b)
}

// Ring is the saturated tone: dots, the badge, the window frame's ring.
func (e Env) Ring(colors *theme.ThemeColors) theme.Hsla {
	switch e {
	case EnvProd:
		return colors.EnvProd
	case EnvStaging:
		return colors.EnvStaging
	default:
		return colors.EnvDev
	}
}

// Inner is the hairline just inside the frame's ring.
func (e Env) Inner(colors *theme.ThemeColors) theme.Hsla {
	switch e {
	case EnvProd:
		return colors.EnvProdInner
	case EnvStaging:
		return colors.EnvStagingInner
	default:
		return colors.EnvDevInner
	}
}

// Surface is the wash: the selected chip, and the framed shell's top bar.
func (e Env) Surface(colors *theme.ThemeColors) theme.Hsla {
	switch e {
	case EnvProd:
		return colors.EnvProdSurface
	case EnvStaging:
		return colors.EnvStagingSurface
	default:
		return colors.EnvDevSurface
	}
}

// Text is text sitting on the wash.
func (e Env) Text(colors *theme.ThemeColors) theme.Hsla {
	switch e {
	case EnvProd:
		return colors.EnvProdText
	case EnvStaging:
		return colors.EnvStagingText
	default:
		return colors.EnvDevText
	}
}

// FormNote is the line under the form's chips, from the comp. It names
// the frame, so the frame never has to explain itself.
func (e Env) FormNote() string {
	switch e {
	case EnvProd:
		return "Prod-tagged connections warn before any write and wear the clay frame."
	case EnvStaging:
		return "Staging wears the sand frame; writes go through without a prompt."
	default:
		return "Dev wears the green frame — no guardrails."
	}
}
