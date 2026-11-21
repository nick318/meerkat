// The connections screen: what the app opens on when it is not pointed
// at a database.
//
// It lists the saved connections and takes a new `postgres://` URL.
// Profiles live in the local SQLite store; passwords go to the OS
// keychain and never touch that file.
//
// ⌘F narrows the list. The search reads the rows already in memory —
// name, URL and user — so it costs a substring scan, not a query, and
// it is a filter, not a jump: the list keeps the store's order.
//
// Opening the screen touches no server. A saved connection is a note of
// where a database is, not a standing session: the app must not wake a
// sleeping server, spend a connection slot, or ask the keychain for a
// password nobody asked it to use. A probe — connect, read the server
// version, disconnect on the tokio runtime — runs only when the user
// asks for it: saving a connection, the form's "test" button, or the
// "retry" action. The version it read is cached in the store, so the
// next launch paints "read-only · PG 17.2" without a socket.
//
// **"test" lives in the form, at the end of the URL it tests.** The
// question it answers is "does what I have typed reach a database", and a
// saved row has answered that already: the row paints what the last probe
// found, and ⏎ retries the ones that are down. So the button belongs where
// the answer is still unknown, and it tests the fields *as typed*, saving
// nothing — a URL that does not work must not have to be saved first.
//
// The rows come grouped by an environment tag the user gives a
// connection in the form. The set of tags is closed — prod, staging,
// dev — because the tags exist to be compared across connections, and
// free text would give every database its own spelling of "prod". The
// tag is a label on the saved row, never a connection parameter.
// Groups follow the form's own order with the untagged rows last; with
// no tag anywhere the headings disappear and the list reads flat. The
// "group by env" switch beside the search turns the grouping off, and
// the store remembers the choice across launches.
//
// The list selects before it acts, after the "select, then act" comp.
// A click selects a row and the bar over the list serves the selection:
// connect, edit, forget. ⏎ or a double click connects, ↑↓ move
// the selection, ⌘I edits. One bar for the actions keeps the rows
// dense, and the keyboard never needs the mouse.
//
// Editing reuses the new-connection form, prefilled from the profile.
// The saved password never comes back into the form; a password field
// left empty keeps the one in the keychain.
//
// Go port notes:
//
//   - The tokio tasks are goroutines; a reply comes back through
//     `window.Update`, with the form's `testGeneration` guarding the test
//     exactly as the Rust did. A screen with no window (a test) drops the
//     reply, as a dropped GPUI entity did.
//   - Names that the Rust kept private to this module and that other
//     modules of the binary also use (`KEY_CONTEXT`, `Row`, `first_line`,
//     `flatten`, `key_bindings`, the actions) carry a `connections`
//     prefix, because every module of the binary is one Go package.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/dbpostgres"
	"github.com/nick318/meerkat/internal/secrets"
	"github.com/nick318/meerkat/internal/storage"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// The reading column: the list never stretches over a wide window. The
// comp draws it at 900, which left the NAME lane a hundred pixels wide
// once the four fixed lanes had taken theirs, and a name that truncates
// on a 1,400-pixel window is a column that is too narrow rather than a
// name that is too long. 1,200 gives the name lane the room and still
// reads as a column, not a stretch.
const columnWidth float32 = 1200.
const modeWidth float32 = 130.

// The name is a word or two the user chose, so it takes a fixed lane; the
// URL is the long one, so it is the lane that flexes with the column.
const nameWidth float32 = 220.
const lastWidth float32 = 80.

// The search line sits beside the section label, so it takes a fixed
// share of that row rather than all of it.
const searchWidth float32 = 300.

// The connection form's card. It floats over the list rather than
// sitting in it, so it has a width of its own instead of the column's:
// two fields a row is what the form reads as, and 640 holds a URL
// beside its name without either one truncating.
const formWidth float32 = 640.

// The width the form's "test" button holds whatever it says. The label
// changes while a test is out — "test" is 4 characters and "testing…" is
// 8 — and a button that resized around its own word would move the URL
// field beside it on every press, which is a shift the eye reads as a
// flicker on the very element the pointer is on. The room is taken for
// the longest word once, and the label is centred in it.
const testWidth float32 = 74.

// The room the form's message line holds whether or not it has anything
// to say, so an answer never moves the blocks under it. Two lines of the
// 10px face: a test result is one short line, and a driver's refusal —
// `error returned from database: password authentication failed for user
// "postgres"` — is the message worth reading whole, so the room is taken
// for the longer of the two.
const messageHeight float32 = 28.

// One cycle of the test button's breath while a test is out, and how much
// of the button's own ink each breath mixes into the fill. It is the run
// button's gesture at a slower pace: this button sits in a form the user
// is reading rather than in the toolbar over a result they are waiting
// for, and a slow breath asks to be noticed once rather than watched.
// `pulsating_between` is a sine, so the turn at either end of the cycle is
// already smooth. Nothing travels across the button and nothing moves by a
// pixel — see the run button for why.
const testBreath = 3600 * time.Millisecond
const testBreathDepth float32 = 0.12

// How long a word coming up on the button, or a message coming up under
// it, takes to reach full ink.
//
// It is a fade and **only** a fade. The width the word sits in and the
// room the message sits in are both already taken — `TEST_WIDTH` and
// `MESSAGE_HEIGHT` — so a slide or a grow would be the very shift those
// two constants exist to prevent. Text that swapped in one frame reads as
// a flicker even when nothing has moved; over `FADE_IN` it reads as an
// answer arriving.
//
// The easing is `ease_in_out`, which leaves and arrives slowly. A quint
// ease-out — what the run button's settle uses, where the point is to be
// over quickly — spends most of a long duration almost finished, so the
// longer it is set the more it reads as a snap followed by a crawl.
const fadeIn = 480 * time.Millisecond

// How much ink the word keeps at the start of its fade. The two words are
// a **crossfade**, not an arrival: `test` is already on the button when
// `testing…` replaces it, so a fade from nothing would blink the slot
// empty for a moment. A message under the URL does start from nothing, and
// fades from nothing.
const wordFadeFloor float32 = 0.3

// The screen's own key context. ⌘F belongs to this screen, not to the
// app, so it cannot take the key from a workspace that is open.
//
// Go port: the context is the screen's root element, which dispatches
// `connectionsKeys` in `View`; the name is kept for the reader.
const connectionsKeyContext = "Connections"

// Where the "group by env" switch keeps its state across launches.
const groupByEnvKey = "connections.group_by_env"

// connectionsAction is the Rust `actions!(connections, [..])`.
type connectionsAction int

const (
	connectionsFocusSearch connectionsAction = iota
	connectionsSelectNext
	connectionsSelectPrevious
	connectionsOpenSelected
	connectionsEditSelected
)

// connectionsKeyBindings is the Rust `connections::key_bindings`.
func connectionsKeyBindings() []uikit.KeyBinding[connectionsAction] {
	return []uikit.KeyBinding[connectionsAction]{
		uikit.Bind("cmd-f", connectionsFocusSearch),
		// The walk follows the list, and ⏎ connects: the keyboard path
		// the comp promises with "↑↓ move · ⏎ connect · ⌘I edit".
		uikit.Bind("down", connectionsSelectNext),
		uikit.Bind("up", connectionsSelectPrevious),
		uikit.Bind("enter", connectionsOpenSelected),
		uikit.Bind("cmd-i", connectionsEditSelected),
	}
}

// connectionsKeys is the table, parsed once.
var connectionsKeys = connectionsKeyBindings()

// ConnectionsEvent is what the screen tells the root.
type ConnectionsEvent interface {
	isConnectionsEvent()
}

// ConnectionsEventOpen is `ConnectionsEvent::Open`: the user picked a
// connection: open the workspace on it.
type ConnectionsEventOpen struct {
	Profile dbclient.Profile
}

func (ConnectionsEventOpen) isConnectionsEvent() {}

// Connections is the screen's state.
type Connections struct {
	focusHandle ui.Handle
	// The title bar's mouse state is gone: `uikit.TitleBar` needs none
	// (see its doc).
	//
	// nil when the profiles file could not be opened; the screen then
	// says so instead of pretending there are no connections.
	store *storage.Store
	error *string
	rows  []connectionsRow
	// The search line over the saved connections. It filters the list
	// that is already in memory; it never reaches a server.
	search *uikit.TextField
	// The selected row, by profile id so it survives a reload. A click
	// selects; only ⏎, the connect button or a double click opens.
	selected *string
	// Whether the list stands in environment groups or reads flat, as
	// the "group by env" switch says. The store remembers it.
	groupByEnv bool
	form       *connectionsForm
	// Unix seconds when the screen opened, so "3d ago" stays put while
	// the user reads it.
	now int64

	// The click count of the last press on a row: what GPUI's
	// `ClickEvent::click_count` said at the click (see `row`).
	rowPressClicks int

	// The window replies come back into (GPUI's `cx.spawn` held the
	// entity's window implicitly). nil drops every reply.
	window *mygo.Window
	// `impl EventEmitter<ConnectionsEvent>`: the root's subscription.
	listeners []func(*Connections, ConnectionsEvent)
}

type connectionsRow struct {
	saved storage.SavedConnection
	state probe
}

// What the last probe of one connection found.
type probe interface {
	isProbe()
}

// Nothing has been asked of this server in this session. The row
// paints what the store cached at the last probe, if anything.
type probeIdle struct{}

type probeProbing struct{}

type probeReady struct {
	server string
}

type probeFailed struct {
	message string
}

func (probeIdle) isProbe()    {}
func (probeProbing) isProbe() {}
func (probeReady) isProbe()   {}
func (probeFailed) isProbe()  {}

// The connection form: where the database is, and who connects. It
// serves both "+ new connection" and "edit ⌘I"; `editing` carries the
// profile id an edit writes back to. The URL may carry credentials too;
// the two fields win over it, so a pasted URL can be corrected without
// editing the string.
type connectionsForm struct {
	name     *uikit.TextField
	url      *uikit.TextField
	user     *uikit.TextField
	password *uikit.TextField
	// The environment tag, picked from the chips; nil is untagged.
	env *Env
	// Whether the session this connection opens refuses to write. A new
	// connection starts on: the careful setting is the one nobody has to
	// remember to choose.
	readOnly bool
	// What the "test" button beside the URL last found. It is state of the
	// form rather than of a row: nothing tested here is saved, so there is
	// no row to paint it in.
	test formTest
	// Counts the tests this form has sent. A reply that does not carry the
	// current count is dropped: a keystroke has changed the fields under
	// it, so it answers about a URL that is not on screen any more.
	testGeneration uint64
	// Counts the messages the form has shown. It is what the message's
	// fade is keyed by — a GPUI animation replays when its element's id
	// changes — and `0` is a form that has said nothing yet, which must
	// not fade an empty line in at a user who has asked nothing.
	messages uint64
	// The profile this form edits; nil saves a new one.
	editing *string
	error   *string
}

// Tab order, and the order the form reads on screen.
func (f *connectionsForm) fields() [4]*uikit.TextField {
	return [4]*uikit.TextField{f.name, f.url, f.user, f.password}
}

// What the form's own probe found. `Idle` is where every keystroke puts it
// back: the answer was about the URL as it was typed then.
type formTest interface {
	isFormTest()
}

type formTestIdle struct{}

type formTestTesting struct{}

// The server version, as `server_version` shortens it. It is empty
// when the server would not say.
type formTestOk struct {
	server string
}

type formTestFailed struct {
	message string
}

func (formTestIdle) isFormTest()    {}
func (formTestTesting) isFormTest() {}
func (formTestOk) isFormTest()      {}
func (formTestFailed) isFormTest()  {}

// Subscribe adds a listener for the screen's events (GPUI's
// `cx.subscribe(&screen, …)`). Listeners run on the main thread, in the
// order they were added.
func (s *Connections) Subscribe(fn func(*Connections, ConnectionsEvent)) {
	s.listeners = append(s.listeners, fn)
}

func (s *Connections) emit(e ConnectionsEvent) {
	for _, fn := range s.listeners {
		fn(s, e)
	}
}

// NewConnections is the Rust `Connections::new(window, cx)`.
//
// The Rust observed the updater so the footer repainted when it moved; in
// Go the updater has one change callback for the whole app
// (`autoupdate.Init(onChange)`), which the boot wires to the windows.
func NewConnections(window *mygo.Window) *Connections {
	store, err := storage.OpenDefault()
	var openError *string
	if err != nil {
		store, openError = nil, connectionsSome(err.Error())
	}
	return newConnections(window, store, openError)
}

// newConnections is the half of `new` after the store is opened, so a
// test can hand it a store of its own.
func newConnections(window *mygo.Window, store *storage.Store, openError *string) *Connections {
	search := uikit.NewTextField("name, host, port or database").Bare(11.)
	groupByEnv := true
	if store != nil {
		groupByEnv = store.Flag(groupByEnvKey, true)
	}
	screen := &Connections{
		store:      store,
		error:      openError,
		rows:       nil,
		search:     search,
		selected:   nil,
		groupByEnv: groupByEnv,
		form:       nil,
		now:        unixNow(),
		window:     window,
	}
	search.Subscribe(screen.onSearchEvent)
	screen.reload()
	return screen
}

// update runs fn on the main thread and repaints: `this.update(cx, …)`
// from a spawned task. With no window the reply is dropped.
func (s *Connections) update(fn func()) {
	if s.window == nil {
		return
	}
	s.window.Update(fn)
}

func (s *Connections) focusSearch() {
	s.search.FocusHandle().Focus()
}

func (s *Connections) onSearchEvent(_ *uikit.TextField, event uikit.TextFieldEvent) {
	switch event {
	// esc empties the line. A filter left behind would hide
	// connections with nothing on screen to say why.
	case uikit.TextFieldCancelled:
		s.search.Clear()
		s.focusHandle.Focus()
	// With the list down to one connection, ⏎ opens it: the
	// search line is then the fastest way into a database.
	case uikit.TextFieldSubmitted:
		var matched []string
		for _, row := range s.matching() {
			matched = append(matched, row.saved.Profile.Id)
		}
		if len(matched) == 1 {
			s.open(matched[0])
		}
	default:
	}
}

// The rows the search line leaves on screen, in the store's order.
func (s *Connections) matching() []*connectionsRow {
	query := connectionsASCIILower(s.search.Trimmed())
	var out []*connectionsRow
	for i := range s.rows {
		if matchesQuery(&s.rows[i].saved, query) {
			out = append(out, &s.rows[i])
		}
	}
	return out
}

// Read the saved connections back from the store, keeping whatever a
// probe has already found for the rows that survive.
func (s *Connections) reload() {
	if s.store == nil {
		return
	}
	saved, err := s.store.ListConnections()
	if err == nil {
		// A row that is still on screen keeps what its probe
		// found; only the rows that are new start over.
		type knownState struct {
			id    string
			state probe
		}
		var known []knownState
		for _, row := range s.rows {
			known = append(known, knownState{row.saved.Profile.Id, row.state})
		}
		s.rows = nil
		rows := make([]connectionsRow, 0, len(saved))
		for _, entry := range saved {
			var state probe = probeIdle{}
			for ix, k := range known {
				if k.id == entry.Profile.Id {
					state = k.state
					known = append(known[:ix], known[ix+1:]...)
					break
				}
			}
			rows = append(rows, connectionsRow{saved: entry, state: state})
		}
		s.rows = rows
		s.error = nil
	} else {
		s.error = connectionsSome(err.Error())
	}
	s.ensureSelection()
}

// Keep the selection on the row it was on; a selection that points
// at nothing (first load, a forgotten row) falls to the first row,
// so the action bar always serves something when there are rows.
func (s *Connections) ensureSelection() {
	stillThere := false
	if s.selected != nil {
		for _, row := range s.rows {
			if row.saved.Profile.Id == *s.selected {
				stillThere = true
				break
			}
		}
	}
	if !stillThere {
		if len(s.rows) > 0 {
			s.selected = connectionsSome(s.rows[0].saved.Profile.Id)
		} else {
			s.selected = nil
		}
	}
}

func (s *Connections) selectedRow() *connectionsRow {
	if s.selected == nil {
		return nil
	}
	return s.findRow(*s.selected)
}

// findRow is the `rows.iter().find(|row| row.saved.profile.id == id)`
// every action starts with.
func (s *Connections) findRow(id string) *connectionsRow {
	for i := range s.rows {
		if s.rows[i].saved.Profile.Id == id {
			return &s.rows[i]
		}
	}
	return nil
}

func (s *Connections) selectRow(id string) {
	s.selected = connectionsSome(id)
}

func (s *Connections) toggleGrouping() {
	s.groupByEnv = !s.groupByEnv
	if s.store != nil {
		// Losing the choice on a crash is not worth interrupting anyone.
		_ = s.store.SetFlag(groupByEnvKey, s.groupByEnv)
	}
}

// The rows in the order the screen paints them: grouped when the
// switch says so, flat otherwise. ↑↓ and the list must agree on
// this order, or the selection would jump.
func (s *Connections) visibleIds() []string {
	var ids []string
	if s.groupByEnv {
		for _, group := range groupByEnv(s.matching()) {
			for _, row := range group.rows {
				ids = append(ids, row.saved.Profile.Id)
			}
		}
	} else {
		for _, row := range s.matching() {
			ids = append(ids, row.saved.Profile.Id)
		}
	}
	return ids
}

// ↑↓ walk the rows the search leaves on screen, in the order the
// screen paints them, clamped at the ends: a short list is not a
// ring, and the edge is where the eye stops.
func (s *Connections) moveSelection(delta int) {
	ids := s.visibleIds()
	if len(ids) == 0 {
		return
	}
	here := -1
	if s.selected != nil {
		for ix, id := range ids {
			if id == *s.selected {
				here = ix
				break
			}
		}
	}
	next := 0
	if here >= 0 {
		next = min(max(here+delta, 0), len(ids)-1)
	}
	s.selected = connectionsSome(ids[next])
}

func (s *Connections) selectNext() {
	s.moveSelection(1)
}

func (s *Connections) selectPrevious() {
	s.moveSelection(-1)
}

func (s *Connections) openSelected() {
	if s.selected != nil {
		s.connect(*s.selected)
	}
}

func (s *Connections) editSelected() {
	if s.selected != nil {
		s.openEdit(*s.selected)
	}
}

// ⏎, the connect button and a double click all land here. A row
// that is down retries rather than opening a workspace that cannot
// paint anything.
func (s *Connections) connect(id string) {
	failed := false
	if row := s.findRow(id); row != nil {
		_, failed = row.state.(probeFailed)
	}
	if failed {
		s.probe(id)
	} else {
		s.open(id)
	}
}

// Connect, read the server version, drop the connection. The row
// paints whatever comes back. A probe never introspects: the row
// says nothing about a catalog, so reading one would cost a large
// database a long query for a line nobody reads.
func (s *Connections) probe(id string) {
	row := s.findRow(id)
	if row == nil {
		return
	}
	profile := row.saved.Profile
	row.state = probeProbing{}

	if profile.Engine != dbclient.EnginePostgres {
		row.state = probeFailed{"only PostgreSQL is supported yet"}
		return
	}

	go func() {
		server, err := connectionsTask(func(ctx context.Context) (string, error) {
			connection, err := dbpostgres.ConnectProfile(ctx, &profile)
			if err != nil {
				return "", err
			}
			// A server that will not say its version is still usable.
			server, err := connection.ServerVersion(ctx)
			if err != nil {
				server = ""
			}
			// A probe is a look, not a session: give the sockets back
			// before the row paints.
			connection.Close(ctx)
			return server, nil
		})
		s.update(func() {
			var state probe
			if err == nil {
				if s.store != nil {
					// Cache what this probe saw; a failure to
					// write it is not worth interrupting anyone.
					_ = s.store.RecordProbe(id, server)
				}
				state = probeReady{server: server}
			} else {
				state = probeFailed{err.Error()}
			}
			if row := s.findRow(id); row != nil {
				if ready, ok := state.(probeReady); ok {
					row.saved.Server = connectionsSome(ready.server)
				}
				row.state = state
			}
		})
	}()
}

// Drop a saved connection, and its password with it. Nothing on the
// server is touched.
func (s *Connections) forget(id string) {
	if s.store != nil {
		if err := s.store.DeleteProfile(id); err != nil {
			s.error = connectionsSome(err.Error())
		}
	}
	_ = secrets.DeletePassword(id)
	s.reload()
}

func (s *Connections) open(id string) {
	row := s.findRow(id)
	if row == nil {
		return
	}
	profile := row.saved.Profile
	if s.store != nil {
		_ = s.store.MarkOpened(profile.Id, unixNow())
	}
	s.emit(ConnectionsEventOpen{Profile: profile})
}

// --- the connection form (new, and edit) ------------------------------

func (s *Connections) openForm() {
	if s.form != nil {
		return
	}
	s.form = s.buildForm(nil)
}

// Edit reuses the same form, prefilled from the saved row. The saved
// password stays in the keychain: it never comes back on screen, and
// an empty password field on save keeps it as it is.
func (s *Connections) openEdit(id string) {
	row := s.findRow(id)
	if row == nil {
		return
	}
	saved := row.saved
	s.form = s.buildForm(&saved)
}

func (s *Connections) buildForm(prefill *storage.SavedConnection) *connectionsForm {
	name := uikit.NewTextField("prod")
	url := uikit.NewTextField("postgres://host:5432/database")
	user := uikit.NewTextField("postgres")
	password := uikit.NewTextField("").Masked()
	if prefill != nil {
		profile := &prefill.Profile
		name.SetText(profile.Name)
		url.SetText(profileUrl(profile))
		if profile.User != nil {
			user.SetText(*profile.User)
		}
	}
	name.Subscribe(s.onFieldEvent)
	url.Subscribe(s.onFieldEvent)
	user.Subscribe(s.onFieldEvent)
	password.Subscribe(s.onFieldEvent)
	name.FocusHandle().Focus()
	form := &connectionsForm{
		name:           name,
		url:            url,
		user:           user,
		password:       password,
		env:            nil,
		readOnly:       true,
		test:           formTestIdle{},
		testGeneration: 0,
		messages:       0,
		editing:        nil,
		error:          nil,
	}
	if prefill != nil {
		form.env = connectionsEnvTag(prefill.Env)
		form.readOnly = prefill.Profile.ReadOnly
		form.editing = connectionsSome(prefill.Profile.Id)
	}
	return form
}

// A chip toggles: clicking the tag the form already has takes it
// away, so untagged needs no fourth chip.
func (s *Connections) setFormEnv(env Env) {
	if form := s.form; form != nil {
		if form.env != nil && *form.env == env {
			form.env = nil
		} else {
			form.env = &env
		}
	}
}

func (s *Connections) toggleFormReadOnly() {
	if form := s.form; form != nil {
		form.readOnly = !form.readOnly
	}
}

func (s *Connections) closeForm() {
	s.form = nil
	s.focusHandle.Focus()
}

func (s *Connections) onFieldEvent(field *uikit.TextField, event uikit.TextFieldEvent) {
	switch event {
	case uikit.TextFieldSubmitted:
		s.saveForm()
	case uikit.TextFieldCancelled:
		s.closeForm()
	case uikit.TextFieldNextFieldEvent:
		// Tab walks the fields in order and wraps at the end.
		form := s.form
		if form == nil {
			return
		}
		fields := form.fields()
		here := 0
		for ix, entry := range fields {
			if entry == field {
				here = ix
				break
			}
		}
		next := fields[(here+1)%len(fields)]
		next.FocusHandle().Focus()
	case uikit.TextFieldChanged:
		if form := s.form; form != nil {
			// The message was about the URL as it was typed then,
			// and so was whatever "test" found. The count going up
			// drops the reply of a test still in flight.
			form.error = nil
			form.test = formTestIdle{}
			form.testGeneration++
		}
	}
}

// Turn the form into a saved profile, then probe it. An edit keeps
// the profile's id, so the store row, the keychain entry, the
// history and the cached catalog all stay its. The screen stays
// where it is, so a connection that does not answer says so in its
// own row rather than in a dead workspace.
func (s *Connections) saveForm() {
	form := s.form
	if form == nil {
		return
	}
	name, url := form.name.Trimmed(), form.url.Trimmed()
	// A password may hold spaces at either end, so it is the one field
	// that is taken exactly as typed.
	user, typedPassword := form.user.Trimmed(), form.password.Text()
	env := form.env
	readOnly := form.readOnly
	var id string
	if form.editing != nil {
		id = *form.editing
	} else {
		id = newId()
	}
	if url == "" {
		s.setFormError("a connection needs a URL")
		return
	}
	store := s.store
	if store == nil {
		s.setFormError("there is nowhere to save connections")
		return
	}

	profile, urlPassword, err := dbpostgres.ProfileFromURL(id, name, url)
	if err != nil {
		s.setFormError(err.Error())
		return
	}
	// The URL says nothing about how careful to be; the switch does.
	profile.ReadOnly = readOnly
	password := mergeCredentials(&profile, user, typedPassword, urlPassword)
	var envTag *string
	if env != nil {
		envTag = connectionsSome(env.AsStr())
	}
	err = store.SaveProfile(&profile)
	if err == nil {
		err = store.SetEnv(profile.Id, envTag)
	}
	if err != nil {
		s.setFormError(err.Error())
		return
	}
	// The password leaves the URL here and goes to the keychain, so
	// the profiles file never carries it. nil writes nothing, which
	// is what lets an edit keep the password it already has.
	if password != nil {
		if err := secrets.SetPassword(profile.Id, *password); err != nil {
			s.setFormError(fmt.Sprintf("saved, but the password did not: %s", err))
		}
	}

	id = profile.Id
	s.closeForm()
	s.selected = connectionsSome(id)
	s.reload()
	s.probe(id)
}

// Test the fields as they are typed, and save nothing.
//
// The URL goes through the same parse a save makes, so what is tried
// is what a save would store. The password is handed to the driver
// rather than looked up, because a connection that has not been saved
// has nothing in the keychain under its id. An **edit** that left the
// field empty is the one case that does look it up — the same reading
// of an empty field the save takes.
//
// It reads the server version and gives the connection straight back,
// as `probe` does: a test is a look, not a session.
func (s *Connections) testForm() {
	form := s.form
	if form == nil {
		return
	}
	if _, testing := form.test.(formTestTesting); testing {
		return
	}
	url := form.url.Trimmed()
	user := form.user.Trimmed()
	// A password may hold spaces at either end, as it may on save.
	typedPassword := form.password.Text()
	readOnly := form.readOnly
	editing := form.editing
	if url == "" {
		s.setFormError("a connection needs a URL")
		return
	}
	// The id is only what the keychain is keyed by, and a connection
	// that has never been saved asks the keychain nothing.
	id := ""
	if editing != nil {
		id = *editing
	}
	profile, urlPassword, err := dbpostgres.ProfileFromURL(id, "", url)
	if err != nil {
		s.setFormError(err.Error())
		return
	}
	// Test the session the switch asks for, so a pooler that refuses a
	// read-only session says so here rather than on the first open.
	profile.ReadOnly = readOnly
	password := mergeCredentials(&profile, user, typedPassword, urlPassword)
	fromKeychain := password == nil && editing != nil

	form.testGeneration++
	form.test = formTestTesting{}
	form.error = nil
	generation := form.testGeneration

	go func() {
		server, err := connectionsTask(func(ctx context.Context) (string, error) {
			var credential dbpostgres.Password
			if fromKeychain {
				credential = dbpostgres.PasswordKeychain{}
			} else {
				credential = dbpostgres.PasswordGiven{Password: password}
			}
			connection, err := dbpostgres.ConnectProbe(ctx, &profile, credential)
			if err != nil {
				return "", err
			}
			// A server that will not say its version is still usable.
			server, err := connection.ServerVersion(ctx)
			if err != nil {
				server = ""
			}
			connection.Close(ctx)
			return server, nil
		})
		s.update(func() {
			form := s.form
			if form == nil {
				return
			}
			// A keystroke since the request left makes this an answer
			// about a URL that is not on screen any more.
			if form.testGeneration != generation {
				return
			}
			if err == nil {
				form.test = formTestOk{server: server}
			} else {
				form.test = formTestFailed{err.Error()}
			}
			form.messages++
		})
	}()
}

func (s *Connections) setFormError(message string) {
	if form := s.form; form != nil {
		form.error = connectionsSome(message)
		form.messages++
	}
}

// release closes the screen's handle on the local file. The Rust dropped
// it with the screen; Go has no drop, so the root calls this whenever it
// lets the screen go. A probe still in flight finds the store gone and
// records nothing, which is what a dropped screen did.
func (s *Connections) release() {
	if s.store != nil {
		_ = s.store.Close()
		s.store = nil
	}
}

// FocusHandle is the screen's focus identity (GPUI's `Focusable`).
func (s *Connections) FocusHandle() *ui.Handle {
	return &s.focusHandle
}

// View builds the screen (Rust: `Render::render`). Call it in every frame
// the screen is on screen.
func (s *Connections) View(c *ui.Context) ui.Element {
	colors := &theme.Current().Colors

	screen := ui.Column(c.Key("connections-screen")).
		Focusable().
		TabStop(false).
		Bind(&s.focusHandle).
		Fill().
		AlignItems(ui.Stretch).
		Background(colors.Window.Color()).
		Font(theme.UiFontFamily).
		FontSize(theme.SizeEmphasis).
		TextColor(colors.TextBody.Color())
	// The `Connections` key context, and its `on_action`s.
	if action, ok := uikit.DispatchKeys(screen, connectionsKeys); ok {
		switch action {
		case connectionsFocusSearch:
			s.focusSearch()
		case connectionsSelectNext:
			s.selectNext()
		case connectionsSelectPrevious:
			s.selectPrevious()
		case connectionsOpenSelected:
			s.openSelected()
		case connectionsEditSelected:
			s.editSelected()
		}
	}
	return screen.Children(func() {
		// The window has no system title bar, so this screen draws one:
		// somewhere to grab the window by, and the screen's name.
		uikit.TitleBar(c, "title-bar").
			BorderWidth(0, 0, 1, 0).
			BorderColor(colors.Border.Color()).
			Background(colors.Panel.Color()).
			Children(func() {
				ui.Text(c, "Connections").
					FontWeight(600).
					TextColor(colors.Text.Color())
			})
		ui.Scroll(c.Key("connections")).
			Grow(1).
			Basis(0).
			MinHeight(0).
			FillWidth().
			Children(func() {
				ui.Column(c).
					FillWidth().
					MaxWidth(columnWidth).
					AlignSelf(ui.Center).
					Padding(56, 32, 40, 32).
					Gap(26).
					AlignItems(ui.Stretch).
					Children(func() {
						s.header(c, colors)
						s.list(c, colors)
						if s.error != nil {
							ui.Text(c, *s.error).
								FontSize(theme.SizeMeta).
								TextColor(colors.Error.Color())
						}
						shortcuts(c, colors)
					})
			})
		// Both of these live outside the scrolling child, or they
		// would scroll away with the list — the overlay scrollbar's
		// reasoning.
		//
		// The version line is pinned in the window's bottom-right
		// corner. It was inline in the list's footer, between "+ new
		// connection" and the key hints, and that put app chrome in
		// the row that says what the *list* answers to. The corner is
		// where a version belongs, and it is the one place on this
		// screen nothing else claims.
		ui.Box(c).
			Absolute().
			Bottom(14.).
			Right(18.).
			// Its own ground, because it floats over the scrolling
			// column: on a narrow window the card below reaches
			// this far, and a version written over a list of
			// shortcuts is two lines of text in one place.
			Padding(3, 6).
			Radius(6.).
			Background(colors.Window.Color()).
			Children(func() { footSummary(c, colors) })
		// The toast sits above it, in the same corner: the line is
		// where the updater is read, and the card is the one thing it
		// ever announces.
		toast(c, colors, 40.)
		// The form is painted last, so it covers the toast: a restart
		// must not be on offer over a password field.
		s.formOverlay(c, colors)
	})
}

func (s *Connections) header(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	count := len(s.rows)
	var subtitle string
	switch count {
	case 0:
		subtitle = "No databases yet. Add the first one below."
	case 1:
		subtitle = "Meerkat is standing watch over 1 database."
	default:
		subtitle = fmt.Sprintf("Meerkat is standing watch over %d databases.", count)
	}

	return ui.Row(c).
		AlignItems(ui.Center).
		Gap(14.).
		Children(func() {
			uikit.MeerkatMark(c, 42.)
			ui.Column(c).
				Gap(5.).
				Children(func() {
					ui.Text(c, greeting()).
						FontSize(theme.SizeTitle).
						FontWeight(500).
						TextColor(colors.Text.Color())
					ui.Text(c, subtitle).
						FontSize(theme.SizeBody).
						TextColor(colors.TextMuted.Color())
				})
		})
}

func (s *Connections) list(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	searching := !s.search.IsEmpty()

	return ui.Column(c).
		Gap(9.).
		AlignItems(ui.Stretch).
		Children(func() {
			ui.Row(c).
				AlignItems(ui.Center).
				Justify(ui.SpaceBetween).
				Gap(12.).
				Children(func() {
					uikit.SectionLabel(c, "CONNECTIONS")
					ui.Row(c).
						AlignItems(ui.Center).
						Gap(10.).
						Children(func() {
							s.envSwitch(c, colors)
							// The line holds its place whatever the list
							// holds, so it does not appear and disappear
							// under the pointer.
							s.searchLine(c, colors)
						})
				})

			// One card holds it all: the action bar over the selection, the
			// column headings, the rows, and the footer with "+ new
			// connection" and the keys. The rows carry no chrome of their
			// own, which is what keeps them dense.
			ui.Column(c).
				AlignItems(ui.Stretch).
				Border(1, colors.BorderStrong.Color()).
				Radius(8.).
				Background(colors.Elevated.Color()).
				Clip().
				Children(func() {
					s.actionBar(c, colors)
					s.headerRow(c, colors)
					if s.groupByEnv {
						// With no tag anywhere the headings say nothing, so they are
						// left out and the list reads flat even with the switch on.
						groups := groupByEnv(s.matching())
						tagged := false
						for _, group := range groups {
							if group.env != nil {
								tagged = true
							}
						}
						for _, group := range groups {
							if tagged {
								s.groupHeading(c, group.env, colors)
							}
							for _, row := range group.rows {
								s.row(c, row, colors)
							}
						}
					} else {
						for _, row := range s.matching() {
							s.row(c, row, colors)
						}
					}

					// A filter that hides everything must say so, or the screen reads
					// as a store that lost its connections.
					if searching && len(s.matching()) == 0 {
						ui.Text(c, "no connection matches the search").
							Padding(12, 13).
							FontSize(theme.SizeMeta).
							TextColor(colors.TextMuted.Color())
					}
					s.footer(c, colors)
				})
		})
}

// The bar over the list. It serves the selected row — connect, edit,
// test, forget — so the rows themselves stay free of buttons, and
// there is one place to look for what ⏎ will do.
func (s *Connections) actionBar(c *ui.Context, colors *theme.ThemeColors) {
	row := s.selectedRow()
	if row == nil {
		return
	}
	id := row.saved.Profile.Id
	_, failed := row.state.(probeFailed)

	var meta string
	switch state := row.state.(type) {
	case probeFailed:
		var seen string
		if row.saved.LastOpened != nil {
			seen = fmt.Sprintf("last seen %s", ago(s.now-*row.saved.LastOpened))
		} else {
			seen = "never opened"
		}
		meta = fmt.Sprintf("%s · %s", connectionsFirstLine(state.message), seen)
	case probeProbing:
		meta = fmt.Sprintf("%s · connecting…", profileUrl(&row.saved.Profile))
	default:
		server := ""
		if row.saved.Server != nil {
			server = fmt.Sprintf(" · %s", *row.saved.Server)
		}
		meta = fmt.Sprintf("%s · %s%s", profileUrl(&row.saved.Profile), modeWord(&row.saved.Profile), server)
	}

	var connectLabel string
	switch row.state.(type) {
	case probeProbing:
		connectLabel = "…"
	case probeFailed:
		connectLabel = "retry ⏎"
	default:
		connectLabel = "connect ⏎"
	}

	connectID, editID, forgetID := id, id, id
	ui.Row(c).
		AlignItems(ui.Center).
		Gap(8.).
		Padding(8, 12).
		Background(colors.Panel.Color()).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Border.Color()).
		Children(func() {
			nameColor := colors.Text
			if failed {
				nameColor = colors.Error
			}
			ui.Text(c, row.saved.Profile.Name).
				FontSize(theme.SizeBody).
				FontWeight(500).
				TextColor(nameColor.Color()).
				Shrink(0)
			if env, ok := parseEnvOpt(row.saved.Env); ok {
				// The selection's tag, as a small chip: the heading
				// that says it may be off screen.
				ui.Row(c).
					Shrink(0).
					AlignItems(ui.Center).
					Gap(4.).
					Padding(2, 5).
					Border(1, colors.BorderStrong.Color()).
					Radius(4.).
					FontSize(theme.SizeCaption).
					FontWeight(600).
					TextColor(colors.TextMuted.Color()).
					Children(func() {
						uikit.StatusDot(c, env.Ring(colors)).Size(5, 5).Radius(2.5)
						ui.Text(c, connectionsASCIIUpper(env.AsStr()))
					})
			}
			metaColor := colors.TextMuted
			if failed {
				metaColor = colors.ErrorSecondary
			}
			ui.Text(c, meta).
				Grow(1).
				Basis(0).
				MinWidth(0).
				FontSize(theme.SizeMeta).
				TextColor(metaColor.Color()).
				SingleLine()
			uikit.AccentButton(c.Key("connect-selected"), connectLabel).
				OnClick(func() { s.connect(connectID) })
			uikit.ToolbarButton(c.Key("edit-selected"), "edit ⌘I").
				OnClick(func() { s.openEdit(editID) })
			// Forgetting a connection drops the profile and its
			// keychain password. The database itself is untouched.
			uikit.ToolbarButton(c.Key("forget-selected"), "forget").
				OnClick(func() { s.forget(forgetID) })
		})
}

// One environment's heading over its rows, in the section-label
// voice the sidebar uses for its groups. The dot carries the
// environment's own color; the untagged group has none.
func (s *Connections) groupHeading(c *ui.Context, env *Env, colors *theme.ThemeColors) ui.Element {
	var text string
	if env != nil {
		text = connectionsASCIIUpper(env.AsStr())
	} else {
		text = "UNTAGGED"
	}
	return ui.Row(c).
		AlignItems(ui.Center).
		Gap(6.).
		Padding(10, 13, 5, 13).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Hairline.Color()).
		FontSize(theme.SizeCaption).
		FontWeight(600).
		TextColor(colors.TextFaint.Color()).
		Children(func() {
			if env != nil {
				uikit.StatusDot(c, env.Ring(colors)).Size(5, 5).Radius(2.5)
			}
			ui.Text(c, text)
		})
}

// The "group by env" switch: a small pill whose knob sits at the
// end the state is. It flips the list between environment groups
// and the flat store order.
func (s *Connections) envSwitch(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	on := s.groupByEnv
	pill := ui.Row(c.Key("group-by-env")).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(7.).
		Padding(5, 9).
		Border(1, colors.Border.Color()).
		Radius(6.).
		Background(colors.Panel.Color()).
		Cursor(ui.CursorPointer)
	if pill.Hovered() {
		pill.BorderColor(colors.BorderStrong.Color())
	}
	pill.OnClick(func() { s.toggleGrouping() })
	return pill.Children(func() {
		fill := colors.BorderStrong
		if on {
			fill = colors.Accent
		}
		knob := ui.Row(c).
			Width(20.).
			Height(11.).
			Radius(11./2).
			Padding(0, 2).
			AlignItems(ui.Center).
			Shrink(0).
			Background(fill.Color())
		if on {
			knob.Justify(ui.End)
		}
		knob.Children(func() {
			ui.Box(c).Size(7, 7).Radius(7. / 2).Background(colors.Window.Color())
		})
		textColor := colors.TextMuted
		if on {
			textColor = colors.TextSecondary
		}
		ui.Text(c, "group by env").
			FontSize(theme.SizeMeta).
			TextColor(textColor.Color())
	})
}

// The column headings, on the rows' own grid so they line up.
func (s *Connections) headerRow(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	heading := func(text string) ui.Element {
		return ui.Text(c, text).
			FontSize(theme.SizeCaption).
			FontWeight(600).
			TextColor(colors.TextFaint.Color())
	}
	return ui.Row(c).
		AlignItems(ui.Center).
		Gap(12.).
		Padding(0, 13).
		Height(26.).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.BorderStrong.Color()).
		Children(func() {
			ui.Box(c).Width(9.).Shrink(0)
			ui.Column(c).Width(nameWidth).Shrink(0).Children(func() { heading("NAME") })
			ui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() { heading("HOST") })
			ui.Column(c).Width(modeWidth).Shrink(0).Children(func() { heading("MODE") })
			ui.Column(c).
				Width(lastWidth).
				Shrink(0).
				AlignItems(ui.End).
				Children(func() { heading("LAST USED").TextAlign(ui.End) })
		})
}

// The card's last line: the way to a new connection, and the keys
// the list answers to. **The updater's line is not among them** — it
// is app chrome rather than anything the list answers to, so it sits
// pinned in the window's own corner. See `View`.
func (s *Connections) footer(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	key := func(text string) ui.Element {
		return ui.Text(c, text).
			FontSize(theme.SizeMeta).
			TextColor(colors.TextFaint.Color())
	}
	return ui.Row(c).
		AlignItems(ui.Center).
		Gap(12.).
		Padding(8, 13).
		Background(colors.Panel.Color()).
		BorderWidth(1, 0, 0, 0).
		BorderColor(colors.Border.Color()).
		Children(func() {
			link := ui.Text(c.Key("new-connection"), "+ new connection").
				FontSize(theme.SizeMeta).
				TextColor(colors.Accent.Color()).
				Cursor(ui.CursorPointer)
			if link.Hovered() {
				link.TextColor(colors.AccentDeep.Color())
			}
			link.OnClick(func() { s.openForm() })
			ui.Box(c).Grow(1).Basis(0)
			key("↑↓ move")
			key("⏎ connect")
			key("⌘I edit")
		})
}

// The search line: a bare field inside a surface of the screen's own,
// the way the palette's line is built.
func (s *Connections) searchLine(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	focused := s.search.FocusHandle().Focused(c)
	border := colors.Border
	if focused {
		border = colors.Accent
	}
	line := ui.Row(c.Key("search-connections")).
		Width(searchWidth).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(8.).
		Padding(6, 10).
		Border(1, border.Color()).
		Radius(6.).
		Background(colors.Panel.Color()).
		Cursor(ui.CursorPointer)
	// Clicking anywhere on the surface puts the caret in the line.
	line.OnClick(func() { s.search.FocusHandle().Focus() })
	return line.Children(func() {
		ui.Text(c, "⌕").
			FontSize(theme.SizeMeta).
			TextColor(colors.TextFaint.Color())
		ui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() { s.search.View(c) })
		ui.Text(c, "⌘F").
			FontSize(theme.SizeCaption).
			TextColor(colors.TextFaint.Color())
	})
}

func (s *Connections) row(c *ui.Context, row *connectionsRow, colors *theme.ThemeColors) ui.Element {
	_, failed := row.state.(probeFailed)
	id := row.saved.Profile.Id
	selected := s.selected != nil && *s.selected == id

	// Failure recolours the row's text, the way the comp does: warm
	// ink over the shared surface, no surface of its own.
	var nameColor, urlColor, dot theme.Hsla
	switch row.state.(type) {
	case probeReady:
		nameColor, urlColor, dot = colors.Text, colors.TextMuted, colors.Ok
	case probeIdle, probeProbing:
		nameColor, urlColor, dot = colors.Text, colors.TextMuted, colors.Idle
	case probeFailed:
		nameColor, urlColor, dot = colors.Error, colors.ErrorSecondary, colors.ErrorMark
	}

	// There is no status column: a row that is up has nothing to say
	// beyond its name and its URL, and a 200-pixel lane that is empty
	// on every healthy row is room taken from the name. So the two
	// states that do have something to say borrow a lane. A failure
	// takes the HOST lane — the URL is the thing that failed, the row
	// is already in the error's ink, and the bar over the list repeats
	// the URL once the row is selected — and "connecting…" takes the
	// MODE lane for the moment the probe is out.
	var host string
	switch state := row.state.(type) {
	case probeFailed:
		host = connectionsFirstLine(state.message)
	default:
		host = profileUrl(&row.saved.Profile)
	}

	word := modeWord(&row.saved.Profile)
	var mode string
	switch state := row.state.(type) {
	case probeReady:
		if state.server != "" {
			mode = fmt.Sprintf("%s · %s", word, state.server)
		} else {
			mode = word
		}
	case probeProbing:
		mode = "connecting…"
	case probeIdle:
		if row.saved.Server != nil {
			mode = fmt.Sprintf("%s · %s", word, *row.saved.Server)
		} else {
			mode = word
		}
	case probeFailed:
		// The failure is in the HOST lane; the mode of a row that is
		// down is not worth a word.
		mode = ""
	}

	var last string
	if row.saved.LastOpened != nil {
		last = ago(s.now - *row.saved.LastOpened)
	} else {
		last = "—"
	}

	card := ui.Row(c.Key("connection-"+id)).
		AlignItems(ui.Center).
		Gap(12.).
		Padding(0, 13).
		Height(34.).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Hairline.Color()).
		Cursor(ui.CursorPointer)
	// A click selects; only a double click opens. The bar over
	// the list is where a single click's actions live.
	// GPUI's `ClickEvent::click_count` is the count of the press the
	// click came from; mygo reports a double click on the press and the
	// click on the release, so the press's count is kept until then.
	card.ObserveInput(func(ev ui.InputEvent) {
		if ev.Kind == ui.InputPointerDown && ev.Button == 0 {
			s.rowPressClicks = ev.Clicks
		}
	})
	card.OnClick(func() {
		if s.rowPressClicks >= 2 {
			s.connect(id)
		} else {
			s.selectRow(id)
		}
	})

	if selected {
		card.Background(colors.Selection.Color())
	} else if card.Hovered() {
		card.Background(colors.Panel.Color())
	}

	return card.Children(func() {
		if selected {
			// The comp's accent rail: painted over the row's left
			// edge, so the columns keep their alignment.
			ui.Box(c).
				Absolute().
				Left(0).
				Top(0).
				Bottom(0).
				Width(2.).
				Background(colors.Accent.Color())
		}
		ui.Row(c).Width(9.).Shrink(0).Children(func() { statusDotOf(c, dot) })
		weight := 400
		if selected {
			weight = 500
		}
		ui.Text(c, row.saved.Profile.Name).
			Width(nameWidth).
			Shrink(0).
			FontSize(theme.SizeBody).
			FontWeight(weight).
			TextColor(nameColor.Color()).
			SingleLine()
		ui.Text(c, host).
			Grow(1).
			Basis(0).
			MinWidth(0).
			FontSize(theme.SizeMeta).
			TextColor(urlColor.Color()).
			SingleLine()
		// The comp marks a guarded connection in the accent and
		// leaves an open one in plain ink, so the column can be
		// read down for the rows that are held back.
		var modeColor theme.Hsla
		switch {
		case failed:
			modeColor = colors.ErrorFaint
		case row.saved.Profile.ReadOnly:
			modeColor = colors.AccentDeep
		default:
			modeColor = colors.TextSecondary
		}
		ui.Text(c, mode).
			Width(modeWidth).
			Shrink(0).
			FontSize(theme.SizeMeta).
			TextColor(modeColor.Color()).
			SingleLine()
		ui.Text(c, last).
			Width(lastWidth).
			Shrink(0).
			FontSize(theme.SizeCaption).
			TextColor(colors.TextFaint.Color()).
			TextAlign(ui.End).
			SingleLine()
	})
}

// One environment chip in the form: a dot in the environment's own
// color and its name. The selected chip wears the environment's
// wash; clicking it again takes the tag away.
func (s *Connections) envChip(c *ui.Context, env Env, selected bool, colors *theme.ThemeColors) ui.Element {
	chip := ui.Row(c.Key("env-" + env.AsStr())).
		Grow(1).
		Basis(0).
		AlignItems(ui.Center).
		Justify(ui.Center).
		Gap(7.).
		PaddingY(7.).
		BorderWidth(1).
		Radius(7.).
		Cursor(ui.CursorPointer).
		FontSize(theme.SizeMeta)
	chip.OnClick(func() { s.setFormEnv(env) })
	chip.Children(func() {
		uikit.StatusDot(c, env.Ring(colors))
		ui.Text(c, env.AsStr())
	})
	if selected {
		chip.BorderColor(env.Ring(colors).Color()).
			Background(env.Surface(colors).Color()).
			FontWeight(500).
			TextColor(env.Text(colors).Color())
	} else {
		chip.BorderColor(colors.Border.Color()).
			Background(colors.Elevated.Color()).
			TextColor(colors.TextMuted.Color())
		if chip.Hovered() {
			chip.BorderColor(colors.TextFaint.Color())
		}
	}
	return chip
}

// The comp's "safety & limits" block: the read-only switch.
//
// The switch is not a label on the row — it is a connection parameter,
// and the driver asks the server for a read-only session, so
// `DROP TABLE` comes back as an error from Postgres rather than from a
// guess about what the SQL meant.
//
// The note under it says what the flag does *not* cover, because a
// guardrail that is trusted further than it reaches is worse than
// none: the setting is the session's default, and a statement is free
// to turn it off for itself. Only a role without write rights closes
// that door.
//
// Who ends a transaction is **not** asked here. A transaction lives on
// one connection and a tab is one connection, so the mode is the tab's
// and the query toolbar is where it is switched: a connection-wide
// setting would open every tab of that database holding a transaction,
// for a user who wanted one.
func (s *Connections) safetySection(c *ui.Context, form *connectionsForm, colors *theme.ThemeColors) ui.Element {
	on := form.readOnly
	return ui.Column(c).
		AlignItems(ui.Stretch).
		Gap(9.).
		Padding(12, 0, 0, 0).
		BorderWidth(1, 0, 0, 0).
		BorderColor(colors.Border.Color()).
		Children(func() {
			uikit.SectionLabel(c, "SAFETY")
			toggle := ui.Row(c.Key("read-only-session")).
				AlignItems(ui.Center).
				Gap(10.).
				Cursor(ui.CursorPointer)
			toggle.OnClick(func() { s.toggleFormReadOnly() })
			toggle.Children(func() {
				uikit.Switch(c, on)
				ui.Column(c).
					Grow(1).
					Basis(0).
					MinWidth(0).
					Gap(3.).
					Children(func() {
						ui.Text(c, "read-only session").
							FontSize(theme.SizeBody).
							TextColor(colors.Text.Color())
						note := "Every statement you type can change the database. Nothing asks twice."
						if on {
							note = "Blocks insert, update, delete and DDL. The server refuses them, not the app."
						}
						ui.Text(c, note).
							FontSize(theme.SizeMeta).
							TextColor(colors.TextFaint.Color())
					})
			})
		})
}

// The form's own probe button, at the end of the URL field. It says
// what it is doing on its own face: a button whose only answer is
// three lines down is a button nobody watches.
//
// **Two things move, and neither of them is a pixel.** The word fades
// in when it changes, because a label that swaps in one frame reads as
// a flicker on the very element the pointer is on — the width is
// already held, so the fade is all that is left to say the word
// changed. And while the test is out the fill breathes, which is the
// run button's gesture for "working", at the run button's own pace.
//
// The word is a child of its own so it can be animated apart from the
// box: GPUI gives one animation to one element, and the box is busy
// breathing.
func (s *Connections) testButton(c *ui.Context, form *connectionsForm, colors *theme.ThemeColors) ui.Element {
	_, testing := form.test.(formTestTesting)
	ink := colors.TextSecondary
	resting := colors.Elevated
	button := uikit.ToolbarButtonBare(c.Key("test-connection")).
		Shrink(0).
		// Fixed room for the longer word: see `TEST_WIDTH`. The
		// horizontal padding the button carries is inside it, so the
		// word is centred rather than pushed off one end.
		Width(testWidth).
		Justify(ui.Center).
		FontSize(theme.SizeCaption)
	button.Children(func() {
		label := "test"
		if testing {
			label = "testing…"
		}
		word := ui.Text(c.Key("test-word"), label).Shrink(0)
		// The id carries which word it is, so the fade replays on
		// the change and holds still through every other rebuild.
		delta := uikit.AnimationDelta(c, word, connectionsAnimationID{"test-word", connectionsU64(testing)},
			uikit.NewAnimation(fadeIn).WithEasing(uikit.EaseInOut))
		alpha := wordFadeFloor + (1.-wordFadeFloor)*delta
		word.TextColor(ink.Opacity(alpha).Color())
	})
	button.OnClick(func() { s.testForm() })
	if !testing {
		return button
	}
	// Phase-locked to the app's clock, so a keystroke elsewhere
	// in the form does not start the breath over.
	delta := uikit.AnimationDelta(c, button, "test-breath",
		uikit.NewAnimation(testBreath).
			RepeatSynced().
			WithEasing(uikit.PulsatingBetween(0., 1.)))
	// A GPUI hover style is laid over the animated fill, so the hovered
	// button keeps the hover's fill.
	if !button.Hovered() {
		button.Background(resting.Blend(ink.Opacity(testBreathDepth * delta)).Color())
	}
	return button
}

// connectionsAnimationID is a GPUI `ElementId` built from a name and a number, the
// shape `("test-word", testing as u64)` had.
type connectionsAnimationID struct {
	name string
	n    uint64
}

func connectionsU64(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// The form's message, under the URL and the button that tests it:
// what the last test found, or what went wrong on a save.
//
// The room is **always taken**, whether or not there is anything to
// say. A message that appeared with its answer would push every block
// under it down at the moment the user is reading it, which is the
// same shift `TEST_WIDTH` takes out of the button — and here it moves
// half the card rather than one field. It is two lines high, so a
// server's own refusal reads whole rather than cut at the pane.
//
// One place serves both, because only one of them is ever worth
// reading: an error is set by the very press that would have tested or
// saved, and a keystroke clears them together.
//
// The message **fades in** rather than appearing. The room under the
// URL is held whether or not anything is in it, so the fade is the
// whole of what says something arrived — and it arrives a round trip
// after the press that asked for it, which is exactly when a change
// nobody saw happen is a change nobody reads.
func (s *Connections) formMessage(c *ui.Context, form *connectionsForm, colors *theme.ThemeColors) ui.Element {
	var color theme.Hsla
	var text string
	if form.error != nil {
		color, text = colors.Error, connectionsFirstLine(*form.error)
	} else {
		switch test := form.test.(type) {
		case formTestFailed:
			color, text = colors.Error, connectionsFirstLine(test.message)
		case formTestOk:
			if test.server == "" {
				// A server that would not say its version still answered.
				color, text = colors.Ok, "connected"
			} else {
				color, text = colors.Ok, fmt.Sprintf("connected · %s", test.server)
			}
		default:
			// Nothing while a test is out: the button already says so.
			color, text = colors.TextFaint, ""
		}
	}
	line := ui.Column(c.Key("form-message")).
		Height(messageHeight).
		FontSize(theme.SizeCaption).
		TextColor(color.Color()).
		Children(func() { ui.Text(c, text) })
	// A form that has said nothing has nothing to fade in, and the
	// first paint of every form is exactly that.
	if form.messages == 0 {
		return line
	}
	delta := uikit.AnimationDelta(c, line, connectionsAnimationID{"form-message", form.messages},
		uikit.NewAnimation(fadeIn).WithEasing(uikit.EaseInOut))
	return line.TextColor(color.Opacity(delta).Color())
}

// The connection form, as a modal over the list. It was a card at the
// foot of the list, and that put it wherever the list ended — below
// the fold on a long list, and beside the rows it was about to change
// on a short one. A form is a question with one answer, and a modal
// is what says nothing else on the screen answers until it is done.
//
// It is the shell's dialog pattern: an absolutely positioned child of
// the screen, never a window of its own, so closing it hands the focus
// straight back to the list. The scrim does **not** dismiss on a
// click, as the close dialog's does not: four fields of typed text and
// a password are what a stray click would throw away. ⎋ and "cancel"
// are the ways out.
func (s *Connections) formOverlay(c *ui.Context, colors *theme.ThemeColors) {
	form := s.form
	if form == nil {
		return
	}
	scrim := ui.Row(c.Key("connection-form-scrim")).
		Absolute().
		Top(0).
		Left(0).
		Fill().
		Justify(ui.Center).
		AlignItems(ui.Center).
		Padding(24.).
		Background(colors.Overlay.Color())
	// `.occlude()`: the scrim takes the press so nothing under it hears
	// it, and does nothing with it.
	scrim.OnClick(func() {})
	scrim.Children(func() { s.formCard(c, form, colors) })
}

func (s *Connections) formCard(c *ui.Context, form *connectionsForm, colors *theme.ThemeColors) ui.Element {
	label := func(text string) ui.Element {
		return ui.Text(c, text).
			FontSize(theme.SizeMeta).
			TextColor(colors.TextMuted.Color())
	}
	editing := form.editing != nil

	card := ui.Scroll(c.Key("connection-form")).
		Width(formWidth).
		MaxWidthPercent(100).
		// A short window scrolls the card rather than clipping it.
		MaxHeightPercent(100).
		Border(1, colors.BorderStrong.Color()).
		Radius(10.).
		Background(colors.Elevated.Color())
	uikit.WithShadows(card, []uikit.BoxShadow{
		{OffsetX: 0, OffsetY: 24, BlurRadius: 60, SpreadRadius: -20, Color: colors.Shadow},
	})
	return card.Children(func() {
		ui.Column(c).
			AlignItems(ui.Stretch).
			Gap(10.).
			Padding(18.).
			Children(func() {
				// A dialog's title, as the close confirmation's is:
				// the card is a question being asked, not a section
				// of a page.
				title := "New connection"
				if editing {
					title = "Edit connection"
				}
				ui.Text(c, title).
					FontSize(theme.SizeEmphasis).
					FontWeight(600).
					TextColor(colors.Text.Color())
				ui.Row(c).
					AlignItems(ui.Stretch).
					Gap(12.).
					Children(func() {
						ui.Column(c).
							Width(200.).
							Shrink(0).
							AlignItems(ui.Stretch).
							Gap(5.).
							Children(func() {
								label("name")
								form.name.View(c)
							})
						ui.Column(c).
							Grow(1).
							Basis(0).
							MinWidth(0).
							AlignItems(ui.Stretch).
							Gap(5.).
							Children(func() {
								label("url")
								// "test" sits at the end of the URL it
								// tests, so the question and the button
								// that answers it read as one line.
								ui.Row(c).
									AlignItems(ui.Center).
									Gap(8.).
									Children(func() {
										ui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() { form.url.View(c) })
										s.testButton(c, form, colors)
									})
							})
					})
				s.formMessage(c, form, colors)
				ui.Row(c).
					AlignItems(ui.Stretch).
					Gap(12.).
					Children(func() {
						ui.Column(c).
							Width(200.).
							Shrink(0).
							AlignItems(ui.Stretch).
							Gap(5.).
							Children(func() {
								label("user")
								form.user.View(c)
							})
						ui.Column(c).
							Grow(1).
							Basis(0).
							MinWidth(0).
							AlignItems(ui.Stretch).
							Gap(5.).
							Children(func() {
								label("password")
								form.password.View(c)
							})
					})
				ui.Column(c).
					AlignItems(ui.Stretch).
					Gap(5.).
					Children(func() {
						label("environment tag")
						ui.Row(c).AlignItems(ui.Stretch).Gap(8.).Children(func() {
							for _, env := range EnvAll {
								s.envChip(c, env, form.env != nil && *form.env == env, colors)
							}
						})
						// The note names the frame the tag brings, so
						// the frame never has to explain itself.
						note := "An untagged connection wears no frame."
						if form.env != nil {
							note = form.env.FormNote()
						}
						ui.Text(c, note).FontSize(theme.SizeMeta).TextColor(colors.TextFaint.Color())
					})
				s.safetySection(c, form, colors)
				ui.Row(c).
					AlignItems(ui.Center).
					Gap(10.).
					Children(func() {
						// Say where the password goes before it is typed,
						// and on an edit, what leaving it empty means.
						hint := "the password goes to the OS keychain, not to the profiles file"
						if editing {
							hint = "an empty password keeps the saved one; a typed one replaces it in the OS keychain"
						}
						ui.Text(c, hint).
							Grow(1).
							Basis(0).
							// A flex child's floor is its content's width,
							// and this line is longer than the card leaves
							// it, so without the floor taken away it pushes
							// "save" out of the card. It wraps instead.
							MinWidth(0).
							FontSize(theme.SizeMeta).
							TextColor(colors.TextFaint.Color())
						cancel := ui.Text(c.Key("cancel-connection"), "cancel").
							Shrink(0).
							Padding(5, 10).
							FontSize(theme.SizeBody).
							TextColor(colors.TextMuted.Color()).
							Cursor(ui.CursorPointer)
						if cancel.Hovered() {
							cancel.TextColor(colors.Text.Color())
						}
						cancel.OnClick(func() { s.closeForm() })
						uikit.AccentButton(c.Key("save-connection"), "save ⏎").
							Shrink(0).
							OnClick(func() { s.saveForm() })
					})
			})
	})
}

// The keyboard the workspace answers to, as the comp lists it.
func shortcuts(c *ui.Context, colors *theme.ThemeColors) ui.Element {
	line := func(what, keys string) {
		ui.Row(c).
			Justify(ui.SpaceBetween).
			AlignItems(ui.Stretch).
			Children(func() {
				ui.Text(c, what)
				ui.Text(c, keys).TextColor(colors.TextMuted.Color())
			})
	}

	return ui.Column(c).
		AlignItems(ui.Stretch).
		Gap(9.).
		Children(func() {
			uikit.SectionLabel(c, "SHORTCUTS")
			ui.Column(c).
				AlignItems(ui.Stretch).
				Gap(8.).
				Padding(11, 13).
				Border(1, colors.Border.Color()).
				Radius(7.).
				Background(colors.Panel.Color()).
				FontSize(theme.SizeMeta).
				TextColor(colors.TextSecondary.Color()).
				Children(func() {
					line("new query", "⌘T")
					line("close tab", "⌘W")
					line("run query", "⌘⏎")
					line("stop / terminate a run", "⌘.")
					line("refresh", "⌘R")
					line("page back / forward", "⌘[ ⌘]")
				})
		})
}

// The comp's dot, one size up: on its own row it needs the extra pixel.
func statusDotOf(c *ui.Context, color theme.Hsla) ui.Element {
	return uikit.StatusDot(c, color).Size(9, 9).Radius(4.5)
}

// What the row and the action bar call this connection's mode. The word
// is the same one the shell's mark carries, so the two screens agree.
func modeWord(profile *dbclient.Profile) string {
	if profile.ReadOnly {
		return "read-only"
	}
	return "read-write"
}

// `postgres://host:port/database`, with no credentials in it.
func profileUrl(profile *dbclient.Profile) string {
	var scheme string
	switch profile.Engine {
	case dbclient.EnginePostgres:
		scheme = "postgres"
	case dbclient.EngineMysql:
		scheme = "mysql"
	case dbclient.EngineSqlite:
		scheme = "sqlite"
	}
	host := "localhost"
	if profile.Host != nil {
		host = *profile.Host
	}
	switch {
	// A socket directory is a path, which a URL's host part can only
	// carry escaped.
	case strings.HasPrefix(host, "/"):
		host = url.PathEscape(host)
	// An IPv6 address is bracketed, or its colons read as a port. A
	// list (`h1:5433,h2`) is written as it was saved.
	case strings.Count(host, ":") > 1 && !strings.Contains(host, ","):
		host = "[" + host + "]"
	}
	address := host
	if profile.Port != nil {
		address = fmt.Sprintf("%s:%d", host, *profile.Port)
	}
	text := fmt.Sprintf("%s://%s/%s", scheme, address, profile.Database)
	if profile.Params != "" {
		text += "?" + profile.Params
	}
	return text
}

// Put the form's user and password over whatever the URL carried, and
// hand back the password that goes to the keychain. A field left empty
// keeps what the URL said, so a URL with credentials in it still works
// on its own.
func mergeCredentials(profile *dbclient.Profile, user, typedPassword string, urlPassword *string) *string {
	if user != "" {
		profile.User = connectionsSome(user)
	}
	if typedPassword == "" {
		return urlPassword
	}
	return connectionsSome(typedPassword)
}

// connectionsEnvGroup is one `(Option<Env>, Vec<&Row>)` of `group_by_env`.
type connectionsEnvGroup struct {
	env  *Env
	rows []*connectionsRow
}

// Group the rows the search left by their environment tag, in the
// form's own order — prod, staging, dev — with the untagged rows
// closing the list: a tag was given to stand out, so an untagged row
// must not stand first.
func groupByEnv(rows []*connectionsRow) []connectionsEnvGroup {
	var groups []connectionsEnvGroup
	for _, row := range rows {
		env := connectionsEnvTag(row.saved.Env)
		found := false
		for i := range groups {
			if connectionsSameEnv(groups[i].env, env) {
				groups[i].rows = append(groups[i].rows, row)
				found = true
				break
			}
		}
		if !found {
			groups = append(groups, connectionsEnvGroup{env: env, rows: []*connectionsRow{row}})
		}
	}
	rank := func(key *Env) int {
		if key == nil {
			return 3
		}
		switch *key {
		case EnvProd:
			return 0
		case EnvStaging:
			return 1
		case EnvDev:
			return 2
		}
		return 3
	}
	// `sort_by_key` is stable, and so is `sort.SliceStable`.
	sort.SliceStable(groups, func(i, j int) bool { return rank(groups[i].env) < rank(groups[j].env) })
	return groups
}

// connectionsSameEnv is `Option<Env> == Option<Env>`.
func connectionsSameEnv(a, b *Env) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// connectionsEnvTag is `Env::parse(tag.as_deref())`, as an `Option<Env>`.
func connectionsEnvTag(tag *string) *Env {
	env, ok := parseEnvOpt(tag)
	if !ok {
		return nil
	}
	return &env
}

// Does this connection answer the search line? `query` must already be
// lowercased, as `matching` hands it over.
//
// The haystack is the name, the URL the row paints, the user and the
// environment tag, so a port ("5433"), a host, a database name, the
// name the user gave the connection or "prod" all find it. Every word
// must hit, so a second word narrows the list rather than widening it.
func matchesQuery(saved *storage.SavedConnection, query string) bool {
	if query == "" {
		return true
	}
	profile := &saved.Profile
	user, env := "", ""
	if profile.User != nil {
		user = *profile.User
	}
	if saved.Env != nil {
		env = *saved.Env
	}
	haystack := connectionsASCIILower(fmt.Sprintf("%s %s %s %s",
		profile.Name,
		profileUrl(profile),
		user,
		env,
	))
	for _, word := range strings.Fields(query) {
		if !strings.Contains(haystack, word) {
			return false
		}
	}
	return true
}

// A driver error can run to several lines; a row has space for one.
func connectionsFirstLine(error string) string {
	// `str::lines`: split at `\n`, and a `\r` before it goes too.
	line, _, found := strings.Cut(error, "\n")
	if found {
		line = strings.TrimSuffix(line, "\r")
	}
	return line
}

func greeting() string {
	switch hour := time.Now().Hour(); {
	case hour >= 5 && hour <= 11:
		return "Good morning."
	case hour >= 12 && hour <= 17:
		return "Good afternoon."
	default:
		return "Good evening."
	}
}

// How long ago, in the comp's shorthand: `3d ago`, `5h ago`, `just now`.
func ago(seconds int64) string {
	switch {
	case seconds <= 59:
		return "just now"
	case seconds <= 3_599:
		return fmt.Sprintf("%dm ago", seconds/60)
	case seconds <= 86_399:
		return fmt.Sprintf("%dh ago", seconds/3_600)
	default:
		return fmt.Sprintf("%dd ago", seconds/86_400)
	}
}

// Wall clock in unix seconds: what the store keeps its times in, both
// for "last opened" and for the query history.
func unixNow() int64 {
	now := time.Now()
	// `duration_since(UNIX_EPOCH)` fails before the epoch, which reads as 0.
	if now.Before(time.Unix(0, 0)) {
		return 0
	}
	return now.Unix()
}

// A profile id that no other profile will take. The clock is enough:
// two connections are never saved in the same nanosecond.
func newId() string {
	now := time.Now()
	var nanos uint64
	if !now.Before(time.Unix(0, 0)) {
		nanos = uint64(now.UnixNano())
	}
	return fmt.Sprintf("%x", nanos)
}

// connectionsTask runs a probe the way a tokio task ran it: a panic in
// it is the task dying, which `flatten` reports as an interruption.
func connectionsTask[T any](task func(ctx context.Context) (T, error)) (value T, err error) {
	var join any
	func() {
		defer func() { join = recover() }()
		value, err = task(context.Background())
	}()
	return connectionsFlatten(value, err, join)
}

// Collapse "the task died" and "the probe failed" into one message.
func connectionsFlatten[T any](value T, err error, join any) (T, error) {
	var zero T
	switch {
	case join != nil:
		return zero, fmt.Errorf("the probe was interrupted: %v", join)
	case err != nil:
		return zero, errors.New(err.Error())
	default:
		return value, nil
	}
}

// connectionsASCIILower is `str::to_ascii_lowercase`: ASCII letters
// only, everything else left as it is.
func connectionsASCIILower(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if ch >= 'A' && ch <= 'Z' {
			b[i] = ch + ('a' - 'A')
		}
	}
	return string(b)
}

// connectionsASCIIUpper is `str::to_ascii_uppercase`.
func connectionsASCIIUpper(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if ch >= 'a' && ch <= 'z' {
			b[i] = ch - ('a' - 'A')
		}
	}
	return string(b)
}

// connectionsSome is `Some(value)` for a field that holds an `Option`.
func connectionsSome[T any](value T) *T {
	return &value
}
