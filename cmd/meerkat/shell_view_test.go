package main

// Not a port of a Rust test: the shell's views rendered headless, over a
// shell built by hand — no window, no store, no connection — so every
// strip, overlay and pane can be looked at. The frames are written to
// SHELL_PNG_DIR when it is set.

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/dbpostgres"
	"github.com/nick318/meerkat/internal/introspect"
	"github.com/nick318/meerkat/internal/query"
	"github.com/nick318/meerkat/internal/resultsgrid"
	"github.com/nick318/meerkat/internal/sqleditor"
	"github.com/nick318/meerkat/internal/uikit"
)

func viewTestCatalog() introspect.Catalog {
	rows := uint64(18_400)
	column := func(name, kind string, nullable bool) introspect.Column {
		return introspect.Column{Name: name, DataType: kind, Nullable: nullable}
	}
	return introspect.Catalog{Schemas: []introspect.Schema{
		{
			Name: "public",
			Tables: []introspect.Table{
				{
					Name: "orders", Kind: introspect.TableKindTable, ApproxRows: &rows,
					Columns: []introspect.Column{
						column("id", "bigint", false),
						column("user_id", "bigint", false),
						column("total", "numeric(12,2)", true),
						column("placed_at", "timestamptz", false),
					},
					PrimaryKey: []string{"id"},
				},
				{
					Name: "users", Kind: introspect.TableKindTable,
					Columns: []introspect.Column{
						column("id", "bigint", false),
						column("email", "text", false),
						column("name", "text", true),
					},
					PrimaryKey: []string{"id"},
				},
				{
					Name: "active_users", Kind: introspect.TableKindView,
					Columns: []introspect.Column{column("id", "bigint", true)},
				},
			},
		},
		{
			Name:   "billing",
			Tables: []introspect.Table{{Name: "invoices", Kind: introspect.TableKindTable}},
		},
	}}
}

// newViewTestShell is a shell as NewShell leaves it, less everything that
// would reach outside the test: the store, the connect, the window.
func newViewTestShell(env *Env) *Shell {
	name := "billing"
	filter := uikit.NewTextField("filter schemas and tables…").Bare(filterFontSize)
	s := &Shell{
		status:         StatusConnected{},
		target:         TargetUrl{Url: "postgres://localhost/shop"},
		connectStarted: time.Now(),
		openSchemas:    map[string]struct{}{},
		closedSections: map[string]struct{}{},
		openRelations:  map[string]struct{}{},
		catalogFilter:  filter,
		label:          &dbpostgres.Label{Database: "shop", Host: "db.internal", Port: 5432},
		name:           &name,
		readOnly:       true,
		nextID:         1,
		env:            env,
		sidebarWidth:   sidebarDefault,
		editorHeight:   editorDefault,
	}
	filter.Subscribe(s.onFilterEvent)
	s.applyCatalog(viewTestCatalog())
	s.toggleSchema("public")
	s.toggleRelation(relationKey("public", "orders"))
	return s
}

// withResult fills the active query tab as a run that landed would.
func withResult(s *Shell) *QueryTab {
	tab := s.tabs[s.active].(*QueryTab)
	tab.editor.SetText("select id, email, name\n  from users\n where name is not null;")
	tab.relation = &[2]string{"public", "users"}
	tab.title = "users"
	tab.data = resultsgrid.NewGridData(
		[]string{"id", "email", "name"},
		[][]dbclient.Value{
			{dbclient.ValueInt(1), dbclient.ValueText("ada@example.com"), dbclient.ValueText("Ada Lovelace")},
			{dbclient.ValueInt(2), dbclient.ValueText("grace@example.com"), dbclient.ValueText("Grace Hopper")},
			{dbclient.ValueInt(3), dbclient.ValueText("edsger@example.com"), dbclient.ValueNull{}},
		},
	)
	tab.hasResult = true
	tab.statementsRun = 1
	tab.landed = 1
	firstRow, link := uint64(12), uint64(9)
	timing := newTiming(41, dbclient.Wire{FirstRowMs: &firstRow, LinkMs: &link})
	tab.timing = &timing
	return tab
}

func writeShellPNG(t *testing.T, tt *ui.Tester, name string) {
	t.Helper()
	dir := os.Getenv("SHELL_PNG_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, tt.Image()); err != nil {
		t.Fatal(err)
	}
}

func mustFind(t *testing.T, tt *ui.Tester, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if _, ok := tt.Find(text); !ok {
			t.Errorf("%q is not on screen; texts: %q", text, tt.Texts())
		}
	}
}

func TestTheShellRendersEveryPaneAndOverlay(t *testing.T) {
	if err := uikit.RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	prod := EnvProd
	s := newViewTestShell(&prod)
	s.newQuery()
	tab := withResult(s)
	s.newQueryWith("delete from orders where placed_at < now() - interval '1 year';")
	s.activate(0)
	tt := ui.NewTester(func(c *ui.Context) { s.View(c) }, 1360, 880)
	tt.Frame()
	mustFind(t, tt, "billing", "Search tables and history", "Read-only", "Result",
		"Query history", "On lookout", "public", "orders", "Column")
	writeShellPNG(t, tt, "01-query-result")

	// A transaction open, a schema report, a syntax mark and a stop.
	tab.inTransaction = true
	tab.txMode = dbclient.TxModeManual
	tab.txTally = TxTally{statements: 2, affected: 14}
	tab.schemaReport = &SchemaReport{
		delta: introspect.Delta{Relations: []introspect.RelationDelta{{
			Schema: "public", Name: "users", Kind: introspect.TableKindTable,
			Change: introspect.RelationAltered{Columns: []introspect.ColumnDelta{
				introspect.ColumnAdded{Column: introspect.Column{Name: "nickname", DataType: "text", Nullable: true}},
				introspect.ColumnDropped{Column: introspect.Column{Name: "legacy_id", DataType: "integer"}},
				introspect.ColumnChanged{
					Before: introspect.Column{Name: "age", DataType: "text"},
					After:  introspect.Column{Name: "age", DataType: "integer"},
				},
			}},
		}}},
		statements: []ddlAt{{ix: 0, verb: query.DdlVerbAlter}},
		ms:         12,
		held:       true,
	}
	tab.editor.SetDiagnostics([]sqleditor.Diagnostic{{Range: sqleditor.Range{Start: 7, End: 9}, Message: `column "idd" does not exist`}})
	tab.run = Run{kind: RunRunning, flight: Live{started: time.Now().Add(-2 * time.Second)}}
	tt.Frame()
	mustFind(t, tt, "SCHEMA CHANGED", "Stop", "Manual", "commit", "rollback")
	writeShellPNG(t, tt, "02-transaction-schema-running")
	tab.run = Run{}

	// The column find, the palette, the peek and the confirmation.
	s.openColumnFind()
	tt.Frame()
	mustFind(t, tt, "3/3", "↑↓ browse")
	writeShellPNG(t, tt, "03-column-find")
	s.closeColumnFind()

	s.openPalette()
	tt.Frame()
	mustFind(t, tt, "type to search tables and history")
	writeShellPNG(t, tt, "04-palette")
	s.closePalette()

	s.openPeek(resultsgrid.NewCell(1, 1))
	s.peek.selection = query.Range{Start: 0, End: 5}
	tt.Frame()
	mustFind(t, tt, "⌘C copies what is marked")
	writeShellPNG(t, tt, "05-peek")
	s.closePeek()

	s.confirm = &Confirm{
		what:    Close{Kind: CloseKindWindow},
		running: []string{"users"},
		open:    []string{"query 2"},
	}
	tt.Frame()
	writeShellPNG(t, tt, "06-confirm")
	s.confirm = nil

	// A history tab, a failed connect, an untagged session.
	s.tabs = append(s.tabs, &HistoryTab{id: s.takeID()})
	s.activate(len(s.tabs) - 1)
	tt.Frame()
	mustFind(t, tt, "nothing run yet on this connection")
	writeShellPNG(t, tt, "07-history")

	plain := newViewTestShell(nil)
	plain.newQuery()
	plain.status = StatusFailed{Error: dbclient.ConnectError{
		Kind:   dbclient.ConnectFailureRefused,
		Detail: "connection refused (os error 61)",
	}}
	tt = ui.NewTester(func(c *ui.Context) { plain.View(c) }, 1360, 880)
	tt.Frame()
	mustFind(t, tt, "Retry", "Back to connections", "Offline", "Off duty")
	writeShellPNG(t, tt, "08-connect-failed")
}

// TestTheShellViewAnswersThePointer drives the view's own pointer
// handling: the sidebar divider follows a drag by its travel, a click on a
// tab activates it, and a click on the × asks to close it.
func TestTheShellViewAnswersThePointer(t *testing.T) {
	if err := uikit.RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	s := newViewTestShell(nil)
	s.newQuery()
	s.newQuery()
	tt := ui.NewTester(func(c *ui.Context) { s.View(c) }, 1360, 880)
	tt.Frame()

	// The divider straddles the sidebar's right border, half way down.
	x, y := sidebarDefault, float32(440)
	tt.Press(x, y)
	tt.Move(x+20, y)
	tt.Move(x+40, y)
	tt.Release(x+40, y)
	tt.Frame()
	if s.paneDrag != nil {
		t.Errorf("the drag is still out after the release")
	}
	if got, want := s.sidebarWidth, sidebarDefault+40; got != want {
		t.Errorf("sidebar width after a 40px drag = %v, want %v", got, want)
	}

	if s.active != 1 {
		t.Fatalf("active = %d, want the last tab opened", s.active)
	}
	if err := tt.Click("query 1"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.active != 0 {
		t.Errorf("a click on the first tab left active = %d", s.active)
	}

	// The run button, held down: pressed tone, then released away from
	// it, which is no click.
	box, ok := tt.Find("Run")
	if !ok {
		t.Fatal("no run button")
	}
	tt.Press(box.X+box.W/2, box.Y+box.H/2)
	tt.Frame()
	if !s.runPressed {
		t.Errorf("a press on the run button did not hold it down")
	}
	tt.Move(600, 600)
	tt.Release(600, 600)
	tt.Frame()
	if s.runPressed {
		t.Errorf("a release away from the run button left it held down")
	}
}
