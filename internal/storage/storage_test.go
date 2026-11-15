package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"
)

func strPtr(s string) *string { return &s }
func u16Ptr(n uint16) *uint16 { return &n }
func u64Ptr(n uint64) *uint64 { return &n }
func i64Ptr(n int64) *int64   { return &n }

// testDir is Rust's `std::env::temp_dir().join("meerkat-store-test")`,
// one per test so parallel packages never share a file.
func testDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "meerkat-store-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustOpen(t *testing.T, path string) *Store {
	t.Helper()
	_ = os.Remove(path)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestProfileRoundTrip(t *testing.T) {
	path := filepath.Join(testDir(t), "test.sqlite")

	store := mustOpen(t, path)
	must(t, store.SaveProfile(&dbclient.Profile{
		Id:       "p1",
		Name:     "Local PG",
		Engine:   dbclient.EnginePostgres,
		Host:     strPtr("localhost"),
		Port:     u16Ptr(5432),
		Database: "app",
		User:     strPtr("nick"),
		ReadOnly: true,
	}))

	profiles, err := store.ListProfiles()
	must(t, err)
	if len(profiles) != 1 {
		t.Fatalf("len = %d, want 1", len(profiles))
	}
	if profiles[0].Engine != dbclient.EnginePostgres {
		t.Fatalf("engine = %v", profiles[0].Engine)
	}
	if profiles[0].Port == nil || *profiles[0].Port != 5432 {
		t.Fatalf("port = %v", profiles[0].Port)
	}
}

func TestAUiValueRoundTripsAndAMissingKeyIsNone(t *testing.T) {
	path := filepath.Join(testDir(t), "ui-value.sqlite")

	store := mustOpen(t, path)
	if v, ok := store.UiValue("sidebar_width"); ok {
		t.Fatalf("got %q, want none", v)
	}
	must(t, store.SetUiValue("sidebar_width", "312"))
	if v, ok := store.UiValue("sidebar_width"); !ok || v != "312" {
		t.Fatalf("got %q, %v", v, ok)
	}
	// A second write replaces, never duplicates: the bag is one value
	// per key.
	must(t, store.SetUiValue("sidebar_width", "204"))
	if v, ok := store.UiValue("sidebar_width"); !ok || v != "204" {
		t.Fatalf("got %q, %v", v, ok)
	}
}

func TestAProbeSurvivesTheNextSave(t *testing.T) {
	path := filepath.Join(testDir(t), "probe.sqlite")

	store := mustOpen(t, path)
	profile := dbclient.Profile{
		Id:       "p1",
		Name:     "Local PG",
		Engine:   dbclient.EnginePostgres,
		Host:     strPtr("localhost"),
		Port:     u16Ptr(5432),
		Database: "app",
		User:     strPtr("nick"),
		ReadOnly: true,
	}
	must(t, store.SaveProfile(&profile))
	must(t, store.RecordProbe("p1", "PG 16.2"))
	must(t, store.MarkOpened("p1", 1_700_000_000))

	// Renaming the profile must not lose what the probe learned.
	profile.Name = "Prod"
	must(t, store.SaveProfile(&profile))

	saved, err := store.ListConnections()
	must(t, err)
	if saved[0].Profile.Name != "Prod" {
		t.Fatalf("name = %q", saved[0].Profile.Name)
	}
	if saved[0].Server == nil || *saved[0].Server != "PG 16.2" {
		t.Fatalf("server = %v", saved[0].Server)
	}
	if saved[0].LastOpened == nil || *saved[0].LastOpened != 1_700_000_000 {
		t.Fatalf("last_opened = %v", saved[0].LastOpened)
	}
}

func TestAnEnvTagSurvivesTheNextSaveAndCanBeTakenAway(t *testing.T) {
	store := storeAt(t, "env.sqlite")
	profile := dbclient.Profile{
		Id:       "p1",
		Name:     "Local PG",
		Engine:   dbclient.EnginePostgres,
		Host:     strPtr("localhost"),
		Port:     u16Ptr(5432),
		Database: "app",
		User:     strPtr("nick"),
		ReadOnly: true,
	}
	must(t, store.SaveProfile(&profile))
	must(t, store.SetEnv("p1", strPtr("prod")))

	// An edit writes the profile again; the tag must stay.
	must(t, store.SaveProfile(&profile))
	saved, err := store.ListConnections()
	must(t, err)
	if saved[0].Env == nil || *saved[0].Env != "prod" {
		t.Fatalf("env = %v", saved[0].Env)
	}

	must(t, store.SetEnv("p1", nil))
	saved, err = store.ListConnections()
	must(t, err)
	if saved[0].Env != nil {
		t.Fatalf("env = %q, want none", *saved[0].Env)
	}
}

func TestTheReadOnlyFlagRoundTripsAndAnOlderRowReadsAsReadOnly(t *testing.T) {
	store := storeAt(t, "read_only.sqlite")
	profile := dbclient.Profile{
		Id:       "p1",
		Name:     "Local PG",
		Engine:   dbclient.EnginePostgres,
		Host:     strPtr("localhost"),
		Port:     u16Ptr(5432),
		Database: "app",
		User:     strPtr("nick"),
		ReadOnly: false,
	}
	must(t, store.SaveProfile(&profile))
	readOnly := func() bool {
		t.Helper()
		saved, err := store.ListConnections()
		must(t, err)
		return saved[0].Profile.ReadOnly
	}
	if readOnly() {
		t.Fatal("saved read-write, read back read-only")
	}

	// An edit writes the flag as well, both ways round.
	profile.ReadOnly = true
	must(t, store.SaveProfile(&profile))
	if !readOnly() {
		t.Fatal("saved read-only, read back read-write")
	}

	// A row an older build wrote has no answer in the column. The
	// careful reading is the one that wins.
	_, err := store.conn.Exec("UPDATE profiles SET read_only = NULL")
	must(t, err)
	if !readOnly() {
		t.Fatal("a NULL flag read back read-write")
	}
}

func storeAt(t *testing.T, name string) *Store {
	t.Helper()
	return mustOpen(t, filepath.Join(testDir(t), name))
}

func run(scope, statement string, ranAt int64, source RunSource) NewRun {
	return NewRun{
		Scope:     scope,
		Statement: statement,
		RanAt:     ranAt,
		Source:    source,
		ElapsedMs: u64Ptr(12),
		RowCount:  u64Ptr(3),
		Affected:  nil,
		Error:     nil,
	}
}

func TestHistoryReadsBackNewestFirstAndPerConnection(t *testing.T) {
	store := storeAt(t, "history.sqlite")
	must(t, store.RecordQuery(run("prod", "select 1", 100, RunSourceUser)))
	must(t, store.RecordQuery(run("prod", "select 2", 200, RunSourceUser)))
	must(t, store.RecordQuery(run("staging", "select 3", 300, RunSourceUser)))

	runs, err := store.ListHistory("prod", DefaultHistoryFilter())
	must(t, err)
	var seen []string
	for _, r := range runs {
		seen = append(seen, r.Statement)
	}
	// A staging query never shows up under production.
	if !reflect.DeepEqual(seen, []string{"select 2", "select 1"}) {
		t.Fatalf("seen = %q", seen)
	}
}

// Rows changed ride their own column, so a run that returned nothing
// and changed three rows reads back as exactly that — and a run that
// returned rows reads back with no count at all, rather than a zero
// that would say it changed nothing when it was never asked to.
func TestTheHistoryKeepsRowsChangedApartFromRowsReturned(t *testing.T) {
	store := storeAt(t, "history-affected.sqlite")
	update := run("prod", "update t set a = 1", 100, RunSourceUser)
	update.RowCount = u64Ptr(0)
	update.Affected = u64Ptr(3)
	must(t, store.RecordQuery(update))
	must(t, store.RecordQuery(run("prod", "select 1", 90, RunSourceUser)))

	seen, err := store.ListHistory("prod", DefaultHistoryFilter())
	must(t, err)
	if seen[0].Affected == nil || *seen[0].Affected != 3 {
		t.Fatalf("affected = %v", seen[0].Affected)
	}
	if seen[0].RowCount == nil || *seen[0].RowCount != 0 {
		t.Fatalf("row_count = %v", seen[0].RowCount)
	}
	if seen[1].Affected != nil {
		t.Fatalf("affected = %d, want none", *seen[1].Affected)
	}
}

func TestTheFiltersNarrowTheHistory(t *testing.T) {
	store := storeAt(t, "history-filters.sqlite")
	must(t, store.RecordQuery(run("prod", "select 1", 100, RunSourceUser)))
	must(t, store.RecordQuery(run("prod", "select * from users", 150, RunSourceApp)))
	failed := run("prod", "select * from user_setings", 200, RunSourceUser)
	failed.ElapsedMs = nil
	failed.RowCount = nil
	failed.Error = strPtr("relation \"user_setings\" does not exist")
	must(t, store.RecordQuery(failed))

	userOnly := DefaultHistoryFilter()
	userOnly.UserOnly = true
	got, err := store.ListHistory("prod", userOnly)
	must(t, err)
	if len(got) != 2 {
		t.Fatalf("user only: %d, want 2", len(got))
	}

	errorsOnly := DefaultHistoryFilter()
	errorsOnly.ErrorsOnly = true
	failedRuns, err := store.ListHistory("prod", errorsOnly)
	must(t, err)
	if len(failedRuns) != 1 {
		t.Fatalf("errors only: %d, want 1", len(failedRuns))
	}
	if failedRuns[0].Error == nil {
		t.Fatal("the failed run has no error")
	}
	if failedRuns[0].ElapsedMs != nil {
		t.Fatalf("elapsed = %d, want none", *failedRuns[0].ElapsedMs)
	}

	recent := DefaultHistoryFilter()
	recent.Since = i64Ptr(150)
	got, err = store.ListHistory("prod", recent)
	must(t, err)
	if len(got) != 2 {
		t.Fatalf("recent: %d, want 2", len(got))
	}
}

func TestTheHistoryStopsGrowing(t *testing.T) {
	store := storeAt(t, "history-prune.sqlite")
	for ix := int64(0); ix < int64(historyLimit)+20; ix++ {
		must(t, store.RecordQuery(run("prod", fmt.Sprintf("select %d", ix), ix, RunSourceUser)))
	}
	kept, err := store.ListHistory("prod", DefaultHistoryFilter())
	must(t, err)
	if len(kept) != historyLimit {
		t.Fatalf("kept %d, want %d", len(kept), historyLimit)
	}
	// The oldest runs are the ones that go.
	if want := fmt.Sprintf("select %d", historyLimit+19); kept[0].Statement != want {
		t.Fatalf("first = %q, want %q", kept[0].Statement, want)
	}
	if want := fmt.Sprintf("select %d", 20); kept[historyLimit-1].Statement != want {
		t.Fatalf("last = %q, want %q", kept[historyLimit-1].Statement, want)
	}
}

func TestTheCatalogCacheIsPerConnectionAndOverwritten(t *testing.T) {
	store := storeAt(t, "catalog.sqlite")
	catalog := func(table string) *introspect.Catalog {
		return &introspect.Catalog{
			Schemas: []introspect.Schema{{
				Name: "public",
				Tables: []introspect.Table{{
					Name:       table,
					Kind:       introspect.TableKindTable,
					Columns:    []introspect.Column{},
					PrimaryKey: []string{"id"},
					ApproxRows: u64Ptr(12),
				}},
			}},
		}
	}

	got, err := store.CachedCatalog("prod")
	must(t, err)
	if got != nil {
		t.Fatal("an empty cache answered with a catalog")
	}
	must(t, store.CacheCatalog("prod", catalog("users"), 100))
	must(t, store.CacheCatalog("staging", catalog("orders"), 100))
	// The second read of the same connection replaces the first.
	must(t, store.CacheCatalog("prod", catalog("people"), 200))

	prod, err := store.CachedCatalog("prod")
	must(t, err)
	if prod == nil {
		t.Fatal("prod is missing")
	}
	if prod.Schemas[0].Tables[0].Name != "people" {
		t.Fatalf("name = %q", prod.Schemas[0].Tables[0].Name)
	}
	if !reflect.DeepEqual(prod.Schemas[0].Tables[0].PrimaryKey, []string{"id"}) {
		t.Fatalf("primary key = %q", prod.Schemas[0].Tables[0].PrimaryKey)
	}
	staging, err := store.CachedCatalog("staging")
	must(t, err)
	if staging == nil {
		t.Fatal("staging is missing")
	}
	if staging.Schemas[0].Tables[0].Name != "orders" {
		t.Fatalf("name = %q", staging.Schemas[0].Tables[0].Name)
	}
}

func TestACatalogTheModelNoLongerParsesIsAMiss(t *testing.T) {
	store := storeAt(t, "catalog-stale.sqlite")
	_, err := store.conn.Exec(
		"INSERT INTO catalog_cache (scope, catalog, cached_at) VALUES (?1, ?2, ?3)",
		"prod", "{\"schemas\":\"whatever\"}", 100,
	)
	must(t, err)

	got, err := store.CachedCatalog("prod")
	must(t, err)
	if got != nil {
		t.Fatal("a stale row answered with a catalog")
	}
	// The row that cannot be read is dropped rather than re-read.
	var left int64
	must(t, store.conn.QueryRow("SELECT count(*) FROM catalog_cache").Scan(&left))
	if left != 0 {
		t.Fatalf("left = %d, want 0", left)
	}
}

func TestTheOpenTabsComeBackInStripOrder(t *testing.T) {
	store := storeAt(t, "tabs.sqlite")
	tabs := []SavedTab{
		SavedTabQuery{
			Title:     "query 1",
			Statement: "select 1",
			Relation:  nil,
			TxMode:    dbclient.TxModeAuto,
		},
		SavedTabQuery{
			Title:     "public.users",
			Statement: "select * from \"public\".\"users\" limit 500;",
			Relation:  &[2]string{"public", "users"},
			// A tab that holds its own transactions comes back holding
			// them, so the mode rides in the row beside the statement.
			TxMode: dbclient.TxModeManual,
		},
		SavedTabTable{
			Schema: "public",
			Table:  "orders",
			Page:   3,
		},
		SavedTabHistory{},
	}
	must(t, store.SaveTabs("prod", tabs, 2))
	must(t, store.SaveTabs("staging", []SavedTab{SavedTabHistory{}}, 0))

	read, err := store.SavedTabs("prod")
	must(t, err)
	if !reflect.DeepEqual(read.Tabs, tabs) {
		t.Fatalf("tabs = %#v", read.Tabs)
	}
	if read.Active != 2 {
		t.Fatalf("active = %d, want 2", read.Active)
	}
	// One connection's tabs never show up under another.
	staging, err := store.SavedTabs("staging")
	must(t, err)
	if !reflect.DeepEqual(staging.Tabs, []SavedTab{SavedTabHistory{}}) {
		t.Fatalf("staging = %#v", staging.Tabs)
	}
}

func TestSavingTheTabsReplacesTheLastSet(t *testing.T) {
	store := storeAt(t, "tabs-replace.sqlite")
	query := func(sql string) SavedTab {
		return SavedTabQuery{
			Title:     "query",
			Statement: sql,
			Relation:  nil,
			TxMode:    dbclient.TxModeAuto,
		}
	}
	must(t, store.SaveTabs("prod", []SavedTab{query("select 1"), query("select 2")}, 1))
	must(t, store.SaveTabs("prod", []SavedTab{query("select 3")}, 0))

	read, err := store.SavedTabs("prod")
	must(t, err)
	if !reflect.DeepEqual(read.Tabs, []SavedTab{query("select 3")}) {
		t.Fatalf("tabs = %#v", read.Tabs)
	}
	if read.Active != 0 {
		t.Fatalf("active = %d, want 0", read.Active)
	}
}

func TestOnlyTheNewestTabsAreKept(t *testing.T) {
	store := storeAt(t, "tabs-limit.sqlite")
	var tabs []SavedTab
	for ix := 0; ix < TabLimit+10; ix++ {
		tabs = append(tabs, SavedTabQuery{
			Title:     fmt.Sprintf("query %d", ix),
			Statement: fmt.Sprintf("select %d", ix),
			Relation:  nil,
			TxMode:    dbclient.TxModeAuto,
		})
	}
	must(t, store.SaveTabs("prod", tabs, len(tabs)-1))

	read, err := store.SavedTabs("prod")
	must(t, err)
	if len(read.Tabs) != TabLimit {
		t.Fatalf("len = %d, want %d", len(read.Tabs), TabLimit)
	}
	// The front of the strip goes, and the selection moves with what
	// is left.
	if !reflect.DeepEqual(read.Tabs[0], tabs[10]) {
		t.Fatalf("first = %#v, want %#v", read.Tabs[0], tabs[10])
	}
	if read.Active != TabLimit-1 {
		t.Fatalf("active = %d, want %d", read.Active, TabLimit-1)
	}
}

func TestATabTheModelNoLongerUnderstandsIsDropped(t *testing.T) {
	store := storeAt(t, "tabs-stale.sqlite")
	_, err := store.conn.Exec(
		"INSERT INTO open_tabs (scope, position, kind, active) VALUES (?1, 0, 'diagram', 1)",
		"prod",
	)
	must(t, err)
	_, err = store.conn.Exec(
		"INSERT INTO open_tabs (scope, position, kind, active) VALUES (?1, 1, 'history', 0)",
		"prod",
	)
	must(t, err)

	read, err := store.SavedTabs("prod")
	must(t, err)
	if !reflect.DeepEqual(read.Tabs, []SavedTab{SavedTabHistory{}}) {
		t.Fatalf("tabs = %#v", read.Tabs)
	}
}

func TestTheMostRecentlyOpenedConnectionComesFirst(t *testing.T) {
	path := filepath.Join(testDir(t), "order.sqlite")

	store := mustOpen(t, path)
	for _, p := range [][2]string{{"a", "alpha"}, {"b", "beta"}, {"c", "gamma"}} {
		must(t, store.SaveProfile(&dbclient.Profile{
			Id:       p[0],
			Name:     p[1],
			Engine:   dbclient.EnginePostgres,
			Host:     strPtr("localhost"),
			Port:     u16Ptr(5432),
			Database: "app",
			User:     nil,
			ReadOnly: true,
		}))
	}
	must(t, store.MarkOpened("b", 100))
	must(t, store.MarkOpened("c", 200))

	connections, err := store.ListConnections()
	must(t, err)
	var order []string
	for _, c := range connections {
		order = append(order, c.Profile.Id)
	}
	// Opened ones first, newest first; never opened last, by name.
	if !reflect.DeepEqual(order, []string{"c", "b", "a"}) {
		t.Fatalf("order = %q", order)
	}
}
