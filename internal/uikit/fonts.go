package uikit

// The two bundled families. Rust loaded them in `main` with
// `cx.text_system().add_fonts(include_bytes!(…))`, from
// crates/meerkat/assets/fonts. Go's embed cannot reach outside its
// package's directory, so the same files are copied beside this one
// (fonts/, with their OFL licences) and `main` calls RegisterFonts in
// place of `add_fonts`.
//
// Each file is registered under its family name, so the three weights of
// a family make up that family and `FontWeight(500)` or `(600)` picks the
// Medium or the SemiBold face (mygo ui/window.go:37-43, RegisterFont).

import (
	"embed"
	"fmt"
	"sync"

	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/theme"
)

//go:embed fonts/*.ttf
var fontFiles embed.FS

// bundledFonts is every face and the family it belongs to, in the order
// Rust's `add_fonts` listed them.
var bundledFonts = []struct{ file, family string }{
	{"fonts/JetBrainsMono-Regular.ttf", theme.MonoFontFamily},
	{"fonts/JetBrainsMono-Medium.ttf", theme.MonoFontFamily},
	{"fonts/JetBrainsMono-SemiBold.ttf", theme.MonoFontFamily},
	{"fonts/IBMPlexSans-Regular.ttf", theme.UiFontFamily},
	{"fonts/IBMPlexSans-Medium.ttf", theme.UiFontFamily},
	{"fonts/IBMPlexSans-SemiBold.ttf", theme.UiFontFamily},
}

var (
	registerOnce sync.Once
	registerErr  error
)

// RegisterFonts adds the bundled fonts to the text system, before the
// first window. A Tester's window draws them too. Calls after the first
// return its answer and register nothing again.
func RegisterFonts() error {
	registerOnce.Do(func() { registerErr = registerFonts() })
	return registerErr
}

func registerFonts() error {
	for _, f := range bundledFonts {
		data, err := fontFiles.ReadFile(f.file)
		if err != nil {
			return fmt.Errorf("failed to load bundled fonts: %w", err)
		}
		if err := ui.RegisterFont(data, f.family); err != nil {
			return fmt.Errorf("failed to load bundled fonts: %s: %w", f.file, err)
		}
	}
	return nil
}
