package uikit

// The one key dispatcher of the port: GPUI's key bindings, key contexts and
// actions, over mygo's shortcuts.
//
// GPUI binds a keystroke to an action in a *key context*, and gives a
// keystroke to the binding that matched deepest in the context stack of
// the focused element (the CLAUDE.md "Key bindings are scoped, not
// global" rule: ⌘⏎ runs a query in the shell and opens a row in the
// palette, ⌘F is bound three times, ⌘W to the root and to the shell).
//
// mygo routes a key the same way, natively: `Element.Shortcut` registers
// a key on an element, and a key pressed goes to the innermost element of
// the focus chain that registered it, then to the overlay on top, then to
// the window's `Context.Shortcut` (mygo ui/input.go:1284-1360, `routeKeys`
// and `routeKey`; `appendFocusChain` at input.go:1053 walks from the focus
// outwards). So a GPUI key context is an element, and a binding in it is a
// Shortcut on that element. What this file adds is only the shape GPUI
// code is written in — a table of keystroke strings and actions, bound
// once per context — so the Rust tables port line for line:
//
//	var fieldKeys = []uikit.KeyBinding[fieldAction]{
//		uikit.Bind("cmd-shift-left", selectToBeginningOfLine),
//		...
//	}
//	el := ui.Box(c)                       // the element that carries the context
//	if a, ok := uikit.DispatchKeys(el, fieldKeys); ok {
//		f.perform(c, a)                   // the `on_action` listeners
//	}
//
// Rules every UI agent follows (see docs/tasks/gpui-to-mygo.md):
//
//   - A binding with `Some(CONTEXT)` → `DispatchKeys` on the element that
//     wore `.key_context(CONTEXT)`. The focus must be inside it, exactly as
//     in GPUI.
//   - A binding with no context, or one on the window's root context that
//     must fire with the focus nowhere → `DispatchWindowKeys(c, …)`. It is
//     the last in line, after every element around the focus and every
//     overlay.
//   - Never handle a bindable key in `HandleInput` of an element that an
//     ancestor also binds: mygo gives `HandleInput` only the keys that no
//     Shortcut in the focus chain (or the window) claims (input.go:1118),
//     so the ancestor would win, the opposite of GPUI. Put the element's
//     own keys in its own table instead; the innermost table wins.
//   - Call `DispatchKeys` for an element in *every* frame it is built:
//     routing reads the registrations of the frame before (`regs`), so a
//     table skipped for a frame is a table that does not claim its keys.
//   - A printable key with no modifier (a bare `space`) bound around a
//     focused element that takes text both fires and types (the darwin
//     backend hands the key to the input method whatever the shortcuts
//     said, internal/darwin/surface.go:1367-1412). GPUI ate it. The Rust
//     design never binds one around a text field (the find strip sits
//     beside its owner's context, never inside it); keep it that way.
//   - mygo delivers one key per element per frame (input.go:1288); a second
//     key for the same element waits for the next frame. It is not lost.

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
)

// KeyBinding is one keystroke bound to an action (GPUI's `KeyBinding`).
// The context is where the table is dispatched, not a field.
type KeyBinding[A comparable] struct {
	// Keystroke is the binding as GPUI wrote it: "cmd-shift-left".
	Keystroke string
	Mods      ui.Modifiers
	Key       ui.Key
	Action    A
}

// Bind parses a GPUI keystroke ("cmd-shift-left", "alt-backspace",
// "ctrl-a", "enter", "cmd-.") into a binding for action. It panics on a
// keystroke it cannot read, which is a mistake in the program.
func Bind[A comparable](keystroke string, action A) KeyBinding[A] {
	mods, key, err := ParseKeystroke(keystroke)
	if err != nil {
		panic(err)
	}
	return KeyBinding[A]{Keystroke: keystroke, Mods: mods, Key: key, Action: action}
}

// DispatchKeys registers every binding of the table as a Shortcut of el
// and reports the action whose key was delivered to el this frame. mygo
// gives an element one key a frame, so there is at most one.
func DispatchKeys[A comparable](el ui.Element, bindings []KeyBinding[A]) (A, bool) {
	var hit A
	found := false
	for _, b := range bindings {
		// Every binding registers, hit or not: an unregistered key is
		// not claimed in the next frame.
		if el.Shortcut(b.Mods, b.Key) && !found {
			hit, found = b.Action, true
		}
	}
	return hit, found
}

// DispatchWindowKeys is DispatchKeys for the window: bindings that fire
// wherever the focus is, or with no focus at all, after every element
// around the focus and every overlay has had its chance.
func DispatchWindowKeys[A comparable](c *ui.Context, bindings []KeyBinding[A]) (A, bool) {
	var hit A
	found := false
	for _, b := range bindings {
		if c.Shortcut(b.Mods, b.Key) && !found {
			hit, found = b.Action, true
		}
	}
	return hit, found
}

// ParseKeystroke reads a GPUI keystroke: modifiers and a key joined by
// '-'. "cmd" is the platform's command modifier (Command on macOS).
func ParseKeystroke(s string) (ui.Modifiers, ui.Key, error) {
	parts := strings.Split(s, "-")
	// "cmd--" names the minus key: the empty part after the last '-'.
	if strings.HasSuffix(s, "--") {
		parts = append(parts[:len(parts)-2], "-")
	}
	var mods ui.Modifiers
	for _, p := range parts[:len(parts)-1] {
		switch p {
		case "cmd":
			mods |= ui.Cmd
		case "ctrl":
			mods |= ui.Ctrl
		case "alt":
			mods |= ui.Alt
		case "shift":
			mods |= ui.Shift
		default:
			return 0, 0, fmt.Errorf("uikit: unknown modifier %q in keystroke %q", p, s)
		}
	}
	name := parts[len(parts)-1]
	if key, ok := namedKeys[name]; ok {
		return mods, key, nil
	}
	if len(name) == 1 {
		switch ch := name[0]; {
		case ch >= 'a' && ch <= 'z':
			return mods, ui.KeyA + ui.Key(ch-'a'), nil
		case ch >= '0' && ch <= '9':
			return mods, ui.Key0 + ui.Key(ch-'0'), nil
		}
	}
	return 0, 0, fmt.Errorf("uikit: unknown key %q in keystroke %q", name, s)
}

var namedKeys = map[string]ui.Key{
	"enter":     ui.KeyEnter,
	"escape":    ui.KeyEscape,
	"backspace": ui.KeyBackspace,
	"tab":       ui.KeyTab,
	"space":     ui.KeySpace,
	"delete":    ui.KeyDelete,
	"insert":    ui.KeyInsert,
	"home":      ui.KeyHome,
	"end":       ui.KeyEnd,
	"pageup":    ui.KeyPageUp,
	"pagedown":  ui.KeyPageDown,
	"left":      ui.KeyLeft,
	"right":     ui.KeyRight,
	"up":        ui.KeyUp,
	"down":      ui.KeyDown,
	"f1":        ui.KeyF1,
	"f2":        ui.KeyF2,
	"f3":        ui.KeyF3,
	"f4":        ui.KeyF4,
	"f5":        ui.KeyF5,
	"f6":        ui.KeyF6,
	"f7":        ui.KeyF7,
	"f8":        ui.KeyF8,
	"f9":        ui.KeyF9,
	"f10":       ui.KeyF10,
	"f11":       ui.KeyF11,
	"f12":       ui.KeyF12,
	"-":         ui.KeyMinus,
	"=":         ui.KeyEqual,
	",":         ui.KeyComma,
	".":         ui.KeyPeriod,
	"/":         ui.KeySlash,
	";":         ui.KeySemicolon,
	"'":         ui.KeyQuote,
	"[":         ui.KeyBracketLeft,
	"]":         ui.KeyBracketRight,
	"\\":        ui.KeyBackslash,
	"`":         ui.KeyBackquote,
}
