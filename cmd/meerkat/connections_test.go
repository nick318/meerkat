package main

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/storage"
	"github.com/nick318/meerkat/internal/uikit"
)

func TestARowUrlCarriesNoCredentials(t *testing.T) {
	profile := dbclient.Profile{
		Id:       "p1",
		Name:     "prod",
		Engine:   dbclient.EnginePostgres,
		Host:     connectionsSome("db.internal"),
		Port:     connectionsSome(uint16(5432)),
		Database: "meerkat",
		User:     connectionsSome("ada"),
		ReadOnly: true,
	}
	if got := profileUrl(&profile); got != "postgres://db.internal:5432/meerkat" {
		t.Errorf("profileUrl = %q", got)
	}
}

func TestTheSearchLineMatchesTheNameTheUrlAndTheUser(t *testing.T) {
	saved := savedConnection("prod", connectionsSome("db.internal"), connectionsSome(uint16(5432)), "meerkat", "ada")

	// The name, the host, the port, the database and the user.
	for _, query := range []string{
		"prod",
		"db.internal",
		"5432",
		"meerkat",
		"ada",
		"postgres://",
	} {
		if !matchesQuery(&saved, query) {
			t.Errorf("%s should match", query)
		}
	}
	// An empty line hides nothing.
	if !matchesQuery(&saved, "") {
		t.Error("an empty line should match")
	}
	if matchesQuery(&saved, "staging") {
		t.Error("staging should not match")
	}
}

func TestEveryWordOfTheSearchMustHit(t *testing.T) {
	prod := savedConnection("prod", connectionsSome("db.internal"), connectionsSome(uint16(5432)), "meerkat", "ada")
	staging :=
		savedConnection("staging", connectionsSome("db.internal"), connectionsSome(uint16(5433)), "meerkat", "ada")

	// A second word narrows: both rows are on db.internal, only one
	// answers to the port as well.
	if !matchesQuery(&prod, "db.internal 5432") {
		t.Error("prod should match")
	}
	if matchesQuery(&staging, "db.internal 5432") {
		t.Error("staging should not match")
	}
}

func savedConnection(
	name string,
	host *string,
	port *uint16,
	database string,
	user string,
) storage.SavedConnection {
	return storage.SavedConnection{
		Profile: dbclient.Profile{
			Id:       name,
			Name:     name,
			Engine:   dbclient.EnginePostgres,
			Host:     host,
			Port:     port,
			Database: database,
			User:     connectionsSome(user),
			ReadOnly: true,
		},
		LastOpened: nil,
		Server:     nil,
		Env:        nil,
	}
}

func TestTheSearchLineMatchesTheEnvironmentTag(t *testing.T) {
	saved := savedConnection("api", connectionsSome("db.internal"), connectionsSome(uint16(5432)), "meerkat", "ada")
	saved.Env = connectionsSome("prod")
	if !matchesQuery(&saved, "prod") {
		t.Error("prod should match")
	}
	if matchesQuery(&saved, "staging") {
		t.Error("staging should not match")
	}
}

func TestGroupsFollowTheFormsEnvOrderAndUntaggedCloseTheList(t *testing.T) {
	type entry struct {
		name string
		env  *string
	}
	var rows []connectionsRow
	for _, e := range []entry{
		{"scratch", nil},
		{"local", connectionsSome("dev")},
		{"api", connectionsSome("Prod")},
		{"replica", connectionsSome("staging")},
		{"billing", connectionsSome("prod")},
	} {
		saved := savedConnection(e.name, nil, nil, "db", "ada")
		saved.Env = e.env
		rows = append(rows, connectionsRow{
			saved: saved,
			state: probeIdle{},
		})
	}

	var refs []*connectionsRow
	for i := range rows {
		refs = append(refs, &rows[i])
	}
	groups := groupByEnv(refs)
	var keys []*Env
	for _, group := range groups {
		keys = append(keys, group.env)
	}
	// The groups stand in the form's order whatever the store order
	// was, "Prod" and "prod" are one group, and untagged is last
	// even though the store listed it first.
	want := []*Env{connectionsSome(EnvProd), connectionsSome(EnvStaging), connectionsSome(EnvDev), nil}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", envKeys(keys), envKeys(want))
	}
	for i := range want {
		if !connectionsSameEnv(keys[i], want[i]) {
			t.Fatalf("keys = %v, want %v", envKeys(keys), envKeys(want))
		}
	}
	var prod []string
	for _, row := range groups[0].rows {
		prod = append(prod, row.saved.Profile.Name)
	}
	if len(prod) != 2 || prod[0] != "api" || prod[1] != "billing" {
		t.Errorf("prod = %q, want [api billing]", prod)
	}
}

// envKeys prints a list of `Option<Env>` the way Rust's Debug did.
func envKeys(keys []*Env) []string {
	var out []string
	for _, key := range keys {
		if key == nil {
			out = append(out, "None")
		} else {
			out = append(out, "Some("+key.AsStr()+")")
		}
	}
	return out
}

func TestLastSeenReadsInTheLargestUnitThatFits(t *testing.T) {
	for _, tc := range []struct {
		seconds int64
		want    string
	}{
		{12, "just now"},
		{300, "5m ago"},
		{7_200, "2h ago"},
		{3*86_400 + 60, "3d ago"},
	} {
		if got := ago(tc.seconds); got != tc.want {
			t.Errorf("ago(%d) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

func TestTheCredentialFieldsWinOverTheUrl(t *testing.T) {
	base := dbclient.Profile{
		Id:       "p1",
		Name:     "prod",
		Engine:   dbclient.EnginePostgres,
		Host:     connectionsSome("db.internal"),
		Port:     connectionsSome(uint16(5432)),
		Database: "meerkat",
		User:     connectionsSome("from_url"),
		ReadOnly: true,
	}
	deref := func(s *string) string {
		if s == nil {
			return "<None>"
		}
		return *s
	}

	// Both fields filled: the URL's credentials are replaced.
	profile := base
	password :=
		mergeCredentials(&profile, "ada", "typed", connectionsSome("from_url"))
	if deref(profile.User) != "ada" {
		t.Errorf("user = %s, want ada", deref(profile.User))
	}
	if deref(password) != "typed" {
		t.Errorf("password = %s, want typed", deref(password))
	}

	// Both fields empty: a URL that carries its own credentials still
	// works on its own.
	profile = base
	password = mergeCredentials(&profile, "", "", connectionsSome("from_url"))
	if deref(profile.User) != "from_url" {
		t.Errorf("user = %s, want from_url", deref(profile.User))
	}
	if deref(password) != "from_url" {
		t.Errorf("password = %s, want from_url", deref(password))
	}

	// A password field left empty means no password at all, which is
	// what trust and peer authentication need.
	profile = base
	password = mergeCredentials(&profile, "ada", "", nil)
	if deref(profile.User) != "ada" {
		t.Errorf("user = %s, want ada", deref(profile.User))
	}
	if password != nil {
		t.Errorf("password = %s, want None", deref(password))
	}
}

func TestAMultiLineDriverErrorFitsOneRow(t *testing.T) {
	if got := connectionsFirstLine("connection refused\n  caused by: ..."); got != "connection refused" {
		t.Errorf("first line = %q", got)
	}
}

// TestConnectionsScreenRendersToPNG is not a port of a Rust test: it
// renders the screen headless, over a store of its own with three saved
// connections, then with the form open, and writes both frames to
// MEERKAT_CONNECTIONS_PNG_DIR (else a temp dir) to be looked at. It also
// walks the list with the keys the screen binds.
func TestConnectionsScreenRendersToPNG(t *testing.T) {
	if err := uikit.RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "meerkat.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i, saved := range []struct {
		name, host, env string
		readOnly        bool
	}{
		{"billing", "db.internal", "prod", true},
		{"replica", "replica.internal", "staging", true},
		{"scratch", "localhost", "", false},
	} {
		profile := dbclient.Profile{
			Id:       saved.name,
			Name:     saved.name,
			Engine:   dbclient.EnginePostgres,
			Host:     connectionsSome(saved.host),
			Port:     connectionsSome(uint16(5432 + i)),
			Database: "meerkat",
			User:     connectionsSome("ada"),
			ReadOnly: saved.readOnly,
		}
		if err := store.SaveProfile(&profile); err != nil {
			t.Fatal(err)
		}
		if saved.env != "" {
			if err := store.SetEnv(saved.name, connectionsSome(saved.env)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.RecordProbe("billing", "PG 17.2"); err != nil {
		t.Fatal(err)
	}

	screen := newConnections(nil, store, nil)
	var opened []string
	screen.Subscribe(func(_ *Connections, e ConnectionsEvent) {
		if open, ok := e.(ConnectionsEventOpen); ok {
			opened = append(opened, open.Profile.Id)
		}
	})
	tt := ui.NewTester(func(c *ui.Context) {
		screen.View(c)
	}, 1100, 900)
	screen.FocusHandle().Focus()
	tt.Frame()

	dir := os.Getenv("MEERKAT_CONNECTIONS_PNG_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	write := func(name string) {
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, tt.Image()); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
	}
	write("connections.png")

	for _, text := range []string{"Connections", "billing", "postgres://db.internal:5432/meerkat", "read-only · PG 17.2", "PROD", "UNTAGGED", "+ new connection"} {
		if !tt.HasText(text) {
			t.Errorf("%q missing; texts %q", text, tt.Texts())
		}
	}

	// The list paints prod, staging, then untagged; ↓ walks it in that
	// order and clamps at the end.
	if screen.selected == nil || *screen.selected != "billing" {
		t.Fatalf("selected = %v, want billing", screen.selected)
	}
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	if *screen.selected != "scratch" {
		t.Errorf("after three ↓ selected = %s, want scratch", *screen.selected)
	}
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if len(opened) != 1 || opened[0] != "scratch" {
		t.Errorf("⏎ opened %q, want [scratch]", opened)
	}

	// ⌘I opens the form on the selection, prefilled.
	tt.Key(ui.Cmd, ui.KeyI)
	tt.Frame()
	tt.Frame()
	if screen.form == nil || screen.form.editing == nil || *screen.form.editing != "scratch" {
		t.Fatalf("⌘I did not open the edit form")
	}
	if got := screen.form.url.Text(); got != "postgres://localhost:5434/meerkat" {
		t.Errorf("url = %q", got)
	}
	write("connections-form.png")
	if !tt.HasText("EDIT CONNECTION") {
		t.Errorf("form title missing; texts %q", tt.Texts())
	}
	// ⎋ in a field closes the form.
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if screen.form != nil {
		t.Errorf("⎋ left the form open")
	}
}

// TestConnectionsRowsSelectOnAClickAndOpenOnADoubleClick is not a port of
// a Rust test: a click selects a row, a double click opens it, and the
// form's scrim does not dismiss on a click.
func TestConnectionsRowsSelectOnAClickAndOpenOnADoubleClick(t *testing.T) {
	if err := uikit.RegisterFonts(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "meerkat.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, name := range []string{"alpha", "beta"} {
		profile := dbclient.Profile{
			Id:       name,
			Name:     name,
			Engine:   dbclient.EnginePostgres,
			Host:     connectionsSome("localhost"),
			Database: "meerkat",
			ReadOnly: true,
		}
		if err := store.SaveProfile(&profile); err != nil {
			t.Fatal(err)
		}
	}
	screen := newConnections(nil, store, nil)
	var opened []string
	screen.Subscribe(func(_ *Connections, e ConnectionsEvent) {
		if open, ok := e.(ConnectionsEventOpen); ok {
			opened = append(opened, open.Profile.Id)
		}
	})
	tt := ui.NewTester(func(c *ui.Context) { screen.View(c) }, 1100, 900)
	screen.FocusHandle().Focus()
	tt.Frame()

	if *screen.selected != "alpha" {
		t.Fatalf("selected = %s, want alpha", *screen.selected)
	}
	at, ok := tt.Find("beta")
	if !ok {
		t.Fatalf("no beta row; texts %q", tt.Texts())
	}
	tt.ClickAt(at.X+at.W/2, at.Y+at.H/2)
	tt.Frame()
	if *screen.selected != "beta" || len(opened) != 0 {
		t.Errorf("a click: selected %s, opened %q", *screen.selected, opened)
	}
	// The second click of a double click: its press counts two.
	tt.ClickAt(at.X+at.W/2, at.Y+at.H/2)
	tt.Frame()
	if len(opened) != 1 || opened[0] != "beta" {
		t.Errorf("a double click opened %q, want [beta]", opened)
	}

	if err := tt.Click("+ new connection"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if screen.form == nil || screen.form.editing != nil {
		t.Fatal("+ new connection did not open an empty form")
	}
	tt.ClickAt(20, 880)
	tt.Frame()
	if screen.form == nil {
		t.Error("a click on the scrim closed the form")
	}
	if err := tt.Click("cancel"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if screen.form != nil {
		t.Error("cancel left the form open")
	}
}
