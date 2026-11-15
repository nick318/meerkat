// The shell's constructor, the tabs a connection was left with, and the
// glue GPUI's entity gave it for free: focus, events, and handing a reply
// from a goroutine back to the main thread. The types are in
// shell_types.go, with the shape decisions every shell file follows.

package main

import (
	"context"
	"strconv"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/resultsgrid"
	"github.com/nick318/meerkat/internal/sqleditor"
	"github.com/nick318/meerkat/internal/storage"
	"github.com/nick318/meerkat/internal/uikit"
)

// NewShell opens the shell and starts connecting. The window paints the
// connecting state immediately; the connection lands later.
//
// Go port: Rust's `Shell::new(target, window, cx)`. The Rust observed the
// updater so the top bar repainted when it moved; in Go the updater has
// one change callback for the whole app (`autoupdate.Init(onChange)`),
// which the boot wires to the windows, so there is nothing to observe
// here.
func NewShell(target Target, window *mygo.Window) *Shell {
	var store *storage.Store
	var storeError *string
	if opened, err := storage.OpenDefault(); err == nil {
		store = opened
	} else {
		message := err.Error()
		storeError = &message
	}
	scope := scopeOf(target)
	var env *Env
	if profile, ok := target.(TargetProfile); ok {
		var tag *string
		if store != nil {
			if found, err := store.EnvOf(profile.Profile.Id); err == nil {
				tag = found
			}
		}
		if parsed, ok := parseEnvOpt(tag); ok {
			env = &parsed
		}
	}
	// The catalog this connection had when it was last open. It paints
	// the sidebar and feeds completion straight away; the introspection
	// running behind it replaces it when it lands. A store that cannot
	// be read is a miss, not an error: the session works without it.
	var cached *introspect.Catalog
	if store != nil {
		if found, err := store.CachedCatalog(scope); err == nil {
			cached = found
		}
	}
	// What this connection was left with. It is read here, beside the
	// catalog, for the same reason: a local SQLite row, so the strip
	// is painted on the first frame rather than after a round trip.
	var saved storage.SavedTabs
	if store != nil {
		if found, err := store.SavedTabs(scope); err == nil {
			saved = found
		}
	}
	catalogFilter := uikit.NewTextField("filter schemas and tables…").Bare(filterFontSize)
	// The pane sizes the last drag left, read here beside the catalog
	// and the tabs for the same reason: local SQLite, so the first
	// frame is already the layout the user set. Clamped against this
	// window, not the one the drag happened on.
	var viewportWidth, viewportHeight float32
	if window != nil {
		width, height := window.ContentSize()
		viewportWidth, viewportHeight = float32(width), float32(height)
	}
	paneSize := func(divider Divider) float32 {
		min, max := divider.rangeIn(viewportWidth, viewportHeight)
		size := divider.defaultSize()
		if store != nil {
			if value, ok := store.UiValue(divider.key()); ok {
				if parsed, err := strconv.ParseFloat(value, 32); err == nil {
					size = float32(parsed)
				}
			}
		}
		return clampF32(size, min, max)
	}
	sidebarWidth := paneSize(DividerSidebar)
	editorHeight := paneSize(DividerEditor)
	readOnly := true
	if profile, ok := target.(TargetProfile); ok {
		readOnly = profile.Profile.ReadOnly
	}
	shell := &Shell{
		status:            StatusConnecting{To: endpoint(target)},
		connection:        nil,
		target:            target,
		connectGeneration: 0,
		connectStarted:    time.Now(),
		catalog:           nil,
		catalogLoading:    false,
		catalogError:      nil,
		catalogGroups:     nil,
		catalogRows:       nil,
		openSchemas:       map[string]struct{}{},
		closedSections:    map[string]struct{}{},
		openRelations:     map[string]struct{}{},
		catalogFilter:     catalogFilter,
		catalogSelected:   nil,
		relationTotal:     0,
		vocabulary:        &sqleditor.Vocabulary{},
		label:             nil,
		name:              connectionName(target),
		readOnly:          readOnly,
		tabs:              nil,
		active:            0,
		nextID:            1,
		store:             store,
		storeError:        storeError,
		scope:             scope,
		env:               env,
		palette:           nil,
		columnFind:        nil,
		resultFind:        nil,
		resultNeedle:      "",
		peek:              nil,
		confirm:           nil,
		sweeping:          false,
		timing:            false,
		runPressed:        false,
		copiedError:       nil,
		paneDrag:          nil,
		sidebarWidth:      sidebarWidth,
		editorHeight:      editorHeight,
		restoring:         false,
		quitting:          false,
		window:            window,
	}
	catalogFilter.Subscribe(shell.onFilterEvent)
	if cached != nil {
		shell.applyCatalog(*cached)
	}
	shell.connect()
	if len(saved.Tabs) == 0 {
		// A session opens on an empty query tab rather than on the
		// first table: it needs no catalog and no round trip, so the
		// user can type while the connection is still being made.
		shell.newQuery()
	} else {
		shell.restoreTabs(saved)
	}
	return shell
}

// --- the tabs a connection was left with ------------------------------

// restoreTabs opens the tabs this connection had last time, in the order
// the strip had them.
//
// A query tab comes back on its statement and waits: a restored
// session must not run a hundred statements at the user, for the same
// reason the history screen never re-runs one. A table tab is the
// paged view, so it asks for its page — and asks for it through the
// usual path, which leaves the tab loading until the connection lands.
func (s *Shell) restoreTabs(saved storage.SavedTabs) {
	s.restoring = true
	for _, tab := range saved.Tabs {
		switch tab := tab.(type) {
		case storage.SavedTabQuery:
			s.newQueryWith(tab.Statement)
			if len(s.tabs) != 0 {
				if query, ok := s.tabs[len(s.tabs)-1].(*QueryTab); ok {
					if tab.Title != "" {
						query.title = tab.Title
					}
					query.relation = tab.Relation
					// The mode the tab was left in, not the
					// connection's default: a tab switched to manual
					// was switched for a reason.
					query.txMode = tab.TxMode
				}
			}
		case storage.SavedTabTable:
			s.restoreTable(tab.Schema, tab.Table, tab.Page)
		case storage.SavedTabHistory:
			s.openHistory()
		}
	}
	s.restoring = false
	// An empty strip is not a session: a file that held nothing this
	// shell could open still opens on a query tab.
	if len(s.tabs) == 0 {
		s.newQuery()
		return
	}
	s.activate(min(saved.Active, len(s.tabs)-1))
	s.focusActiveTab()
}

// restoreTable puts back a paged table tab. This is the only way one is
// opened now: nothing in the session starts a paged view, since the
// sidebar and the palette both browse a relation in a query tab, so a
// table tab comes back only where an earlier session left one. It is
// built here by hand because the catalog names the table's kind and the
// catalog may still be on its way — a tab the user had open is opened
// whether or not the shell can describe it yet.
func (s *Shell) restoreTable(schema, table string, page int) {
	model := s.tableModel(schema, table)
	kind := introspect.TableKindTable
	var approxRows *uint64
	if model != nil {
		kind = model.Kind
		approxRows = model.ApproxRows
	}
	id := s.takeID()
	s.tabs = append(s.tabs, &TableTab{
		id:         id,
		schema:     schema,
		table:      table,
		kind:       kind,
		data:       emptyGrid(),
		page:       page,
		approxRows: approxRows,
		timing:     nil,
		loading:    false,
		error:      nil,
		selection:  resultsgrid.Selection{},
		scroll:     resultsgrid.NewGridState(),
		generation: 0,
	})
	s.activate(len(s.tabs) - 1)
	s.loadPage(id, page)
}

// rememberTabs writes the strip back to the local file, so the next
// session on this connection opens on it. Every path that changes what is
// open, what a query tab holds, or which tab is in front comes through
// here.
//
// A strip that cannot be written is not worth interrupting a session
// over, so the error is dropped — as the history's is.
func (s *Shell) rememberTabs() {
	if s.restoring {
		return
	}
	if s.store == nil {
		return
	}
	tabs := make([]storage.SavedTab, 0, len(s.tabs))
	for _, tab := range s.tabs {
		switch tab := tab.(type) {
		case *TableTab:
			tabs = append(tabs, storage.SavedTabTable{
				Schema: tab.schema,
				Table:  tab.table,
				Page:   tab.page,
			})
		case *QueryTab:
			tabs = append(tabs, storage.SavedTabQuery{
				Title:     tab.title,
				Statement: tab.editor.Text(),
				Relation:  tab.relation,
				TxMode:    tab.txMode,
			})
		case *HistoryTab:
			tabs = append(tabs, storage.SavedTabHistory{})
		}
	}
	_ = s.store.SaveTabs(s.scope, tabs, s.active)
}

// --- what GPUI's entity gave the shell ---------------------------------

// FocusHandle is `impl Focusable for Shell`.
func (s *Shell) FocusHandle() *ui.Handle {
	return &s.focusHandle
}

// Subscribe adds a listener for the shell's events (`impl
// EventEmitter<ShellEvent> for Shell` with `cx.subscribe`). Listeners run
// on the main thread, in the order they were added.
func (s *Shell) Subscribe(fn func(*Shell, ShellEvent)) {
	s.listeners = append(s.listeners, fn)
}

// emit is `cx.emit(event)`. GPUI delivered it after the update that
// emitted it; a Go listener runs inside the call, so one that would
// re-enter the shell defers its work.
func (s *Shell) emit(event ShellEvent) {
	for _, fn := range s.listeners {
		fn(s, event)
	}
}

// update runs fn on the main thread and repaints: `this.update(cx, …)`
// from a spawned task. A reply that lands after the window let the shell
// go — or with no window at all, in a test — is dropped, as a
// `WeakEntity` that no longer upgrades dropped it.
func (s *Shell) update(fn func()) {
	s.updateOr(fn, nil)
}

// updateOr is update for a reply that carries something which must not be
// lost with it — a session, a connection. When the shell has been let go,
// `dropped` runs instead of fn (on the main thread, or on the calling
// goroutine when there is no window), so the reply can close what it
// holds: Rust dropped it with the reply, and its `Drop` closed the socket.
func (s *Shell) updateOr(fn func(), dropped func()) {
	if s.window == nil {
		if dropped != nil {
			dropped()
		}
		return
	}
	s.window.Update(func() {
		if s.released {
			if dropped != nil {
				dropped()
			}
			return
		}
		fn()
	})
}

// release is the window letting this shell go: GPUI dropping the entity.
// Every reply still in flight is dropped from here on (see update).
//
// Leaving a shell drops it, and with it the connection pool, so a closed
// database keeps no sockets open. Go has no `Drop`, so this is where that
// happens: every tab's session is closed and then the pool, on a
// goroutine of their own — a close must not hold the window up, and the
// pool waits for the sessions it lent out.
func (s *Shell) release() {
	if s.released {
		return
	}
	s.released = true
	var sessions []dbclient.Session
	for _, tab := range s.tabs {
		if query, ok := tab.(*QueryTab); ok && query.session != nil {
			sessions = append(sessions, query.session)
			query.session = nil
		}
	}
	connection := s.connection
	s.connection = nil
	go func() {
		ctx := context.Background()
		for _, session := range sessions {
			session.Close(ctx)
		}
		switch connection := connection.(type) {
		case interface{ Close(context.Context) }:
			connection.Close(ctx)
		case interface{ Close() error }:
			_ = connection.Close()
		}
	}()
}
