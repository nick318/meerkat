// The schema report: what a shape-changing run did, read back off the
// catalog (shell.rs, agent K2's part — see docs/tasks/shell-split.md).

package main

import (
	"context"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/introspect"
)

// reportSchema reads the catalog again after a run changed the shape of
// the database, and puts what differs into the tab's report.
//
// **Down the tab's own session**, because that is the only connection
// the change is visible to while a transaction holds it; the pool
// answers only for a tab with no session, which cannot have run a
// statement, so in practice never. The catalog the shell had is what
// the new one is compared with, and it is taken for the sidebar only
// when nothing is uncommitted — a table one tab has not committed
// must not appear in the tree every other tab reads.
//
// Rust: `fn report_schema(&mut self, tab_id, generation, cx)`.
func (s *Shell) reportSchema(tabID, generation uint64) {
	connection := s.connection
	if connection == nil {
		return
	}
	before := s.catalog
	tab, ok := s.tabMut(tabID).(*QueryTab)
	if !ok {
		return
	}
	session := tab.session
	go func() {
		after, err := flatten(joinTask(func() (introspect.Catalog, error) {
			ctx := context.Background()
			if session != nil {
				catalog, err := session.Introspect(ctx)
				if err != nil {
					return introspect.Catalog{}, err
				}
				if catalog != nil {
					return *catalog, nil
				}
			}
			return connection.Introspect(ctx)
		}))
		s.update(func() {
			tab, ok := s.tabMut(tabID).(*QueryTab)
			if !ok {
				return
			}
			if tab.generation != generation {
				return
			}
			report := tab.schemaReport
			if report == nil {
				return
			}
			report.pending = false
			if err != nil {
				message := err.Error()
				report.error = &message
				return
			}
			if before != nil {
				report.delta = introspect.Diff(before, &after)
			} else {
				report.delta = introspect.Delta{}
			}
			report.held = tab.inTransaction
			held := report.held
			// One shape-changing statement in the run, so the whole
			// report is its doing, and its own line can say so.
			if len(report.statements) == 1 {
				only := report.statements[0]
				if meta, ok := ddlMeta(&report.delta, only.verb, report.ms); ok {
					tab.editor.SetStatementMeta(only.ix, meta)
				}
			}
			if !held {
				if s.store != nil {
					_ = s.store.CacheCatalog(s.scope, &after, unixNow())
				}
				s.applyCatalog(after)
			}
		})
	}()
}

// revealRelation opens the sidebar on a relation the schema report names,
// so "what did that do" and "where is it now" are one click apart. The
// filter is emptied — a filtered tree may not hold the row — the schema
// and its section are opened, because a row under a closed header is not
// revealed, and the relation is unfolded to show what changed in it.
//
// Rust: `fn reveal_relation(&mut self, schema: SharedString, name:
// SharedString, cx)`.
func (s *Shell) revealRelation(schema, name string) {
	s.catalogFilter.Clear()
	s.openSchemas[schema] = struct{}{}
	// Only the schema's own sections: a folder the user closed inside
	// one of its relations has a third part, and stays as they left it.
	prefix := schema + "\t"
	for key := range s.closedSections {
		if strings.HasPrefix(key, prefix) && !strings.Contains(key[len(prefix):], "\t") {
			delete(s.closedSections, key)
		}
	}
	// Unfolded, because what changed may be an index or a column, and
	// a relation revealed shut would hide exactly that.
	s.openRelations[relationKey(schema, name)] = struct{}{}
	s.rebuildCatalogRows()
	for ix, row := range s.catalogRows {
		if row, ok := row.(CatalogRowRelation); ok && row.schema == schema && row.name == name {
			found := ix
			s.catalogSelected = &found
			s.catalogScroll.List.ScrollTo(ix, ui.Center)
			break
		}
	}
}
