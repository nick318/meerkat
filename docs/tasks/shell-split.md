# How `crates/meerkat/src/shell.rs` is split into Go files

> The Rust is gone from `main`; read it at tag `v0.1.8`:
> `git show v0.1.8:crates/meerkat/src/shell.rs`.

`shell.rs` is 12,169 lines; its `#[cfg(test)]` module starts at line
10,693. Every item belongs to exactly one Go file below, all in package
`main` under `cmd/meerkat/`. Line numbers are the item's first line in
`shell.rs` at commit `9f9382a`. An item not named here belongs to the file
whose range contains its line. Doc comments above an item travel with it.

Item map (generate a fresh one with):

```sh
rg -n '^(pub )?(async )?fn |^impl |^(pub(\(crate\))? )?(struct|enum|const|type|static) |^    (pub(\(crate\))? )?fn ' crates/meerkat/src/shell.rs
```

## K1 — the struct, the types, the pure policy, the tests (do first, alone)

`shell_types.go`
- 1–1234: imports become Go imports; every constant (111–413), the
  key-context names and the `*_key_bindings()` tables (133–228, as
  `uikit.KeyBinding` tables), `Divider` + impl, `PaneDrag`,
  `dragged_size`, **`Shell` (485)**, `Palette`, `ColumnFind`,
  `ResultFind`, `FoundCells`, `Peek`, `StopTask`, `Confirm`, `Close`,
  `close_intent`, `Target`, `ShellEvent`, `Status`, `Tab`, `TableTab`,
  **`QueryTab` (865)**, `Run`, `Live`, `Timing` + impl, `RunPhase`,
  `impl Run`, `HistoryTab`, `impl Tab`, `Marked`, `empty_grid`.
- 4724 `Outcome`; 4808–4965 `Changed`, `SchemaReport`, `Ran`,
  `RunOutcome` + impl, `Shape`, the `CALLED_OFF` … `RUN_HOLDS_THE_SESSION`
  constants; 5372 `Tally` (struct only; impl goes to K2).
- 8908–9076 `CatalogRow`, `Mark`, `TREE_*`, `chevron_at`, `content_at`,
  `Group`, `Section`, `Entry` + impl, `Leaf` + impl, `Parts`, `Folder`.
- 9918 `SessionState`; 9981 `TxTally` + impl; 10008 `TxState`; 10385
  `Sign`; 9839 `ConfirmCopy`.

`shell.go`
- 1235–1485: `impl Shell` opening, `Shell::new`, `restore_tabs`,
  `restore_table`, `remember_tabs`.
- 5479–5486: `Focusable`, `EventEmitter` (whatever the Go shape is).

`shell_policy.go` — every free function that paints nothing:
- 4694 `step_stop`, 4711 `step_wrapping`, 4738 `scope_of`, 4746
  `run_scope`, 4759 `vocabulary_of`.
- 5216 `statement_meta`, 5254 `ddl_meta`, 5302 `object_meta`, 5341
  `schema_note`, 5467 `not_connected_copy`, 5560 `endpoint`, 5585
  `connection_name`, 5492 `window_corner_radius`.
- 9077 `catalog_groups`, 9147 `catalog_stops`, 9157 `relation_key`, 9163
  `relation_parts`, 9239 `catalog_rows`.
- 9753 `clamp_to_boundary`, 9768 `word_at`, 9793 `line_at`.
- 9841 `confirm_copy`, 9932 `spare`, 9942 `idle_sessions`, 9954
  `evictable`, 9968 `needs_begin`.
- 10110 `diagnostics`, 10140 `mark_for`, 10190 `reach_back`, 10218
  `being_typed`, 10224 `report_verbs`, 10240 `report_subject`, 10266
  `DELTA_ROWS`, 10468 `column_count`, 10477 `column_note`, 10489
  `changed_copy`, 10508 `tx_copy`, 10670 `spoken_for`, 10678 `redact`.

`shell_test.go` — the whole test module, 10693–end: 59 tests, one Go
test each, same names in CamelCase. Tests that touch a method owned by
K2/K3 still live here; K1 writes the test and a minimal stub of the
method signature in the owning file with `// TODO(K2)` / `// TODO(K3)`
only if the test cannot compile otherwise — prefer porting the method
for real when it is pure.

## K2 — sessions, runs, transactions, checks, schema report, close guard

`shell_run.go`
- 1673 `apply_catalog`, 1706 `connect`, 1789 `retry_connect`, 1810
  `introspect`, 1843 `resume_pending_pages`, 1930 `load_page`.
- 2033 `mark_statement`, 2055 `skip_statements`.
- 2360 `run_active_query`, 2673 `stop_active_query`.
- 2733 `session_states`, 2749 `sweep_idle_sessions`, 2761
  `make_room_for_session`, 2777 `end_session`, 2793
  `start_session_timer`.
- 3009 `refresh_server_timing`, 3049 `start_timer`, 3073
  `any_run_in_flight`, 3084 `record_run`.
- 4790 `run_sql`, 4966 `run_statements`, 5388 `impl Tally`, 5436
  `flatten`, 5447 `connect_outcome`.
- 3106 `open_history`, 3134 `load_history`, 3182 `reload_open_history`,
  3194 `toggle_history_filter`.

`shell_tx.go` — 2829 `refresh_transaction`, 2876 `set_tx_mode`, 2905
`end_transaction`.

`shell_check.go` — 2194 `schedule_check`, 2296 `check_active`, 10076
`missing_relation`.

`shell_schema.go` — 2083 `report_schema`, 2153 `reveal_relation`.

`shell_close.go` — 4125 `guard_close`, 4167 `work_ended_by`, 4194
`cancel_runs`, 4225 `forget_quit`, 4230 `proceed_close`, 4287
`close_confirm`, 4309 `close_tab`.

## K3a — interaction (state changes the views call)

`shell_interact.go`
- 1486 `rebuild_catalog_rows` … 1636 `on_filter_event` (sidebar filter,
  ring, completion, toggles).
- 1865 `browse_table`, 1894 `browse_table_in`, 1919 `table_model`.
- 2304 `new_query`, 2311 `new_query_with`.
- 3208–3454 palette methods; 3455–3609 column find; 3610–3886 result
  find (incl. `result_find_strip` 3823 — it paints, but keep it with its
  state); 3887–4103 peek; 4104 `step_tab`.
- 4337 `activate`, 4360 `focus_active_tab`, 4368 `step_page`, 4391
  `refresh_active`, 4408 `tab_mut`, 4412 `take_id`, 4425 `step_cursor`.
- 4439–4690 every `on_*` action handler.
- 7808 `release_run_button`, 7822 `toggle_run`; 6073–6162 pane-size
  state (`pane_size`, `set_pane_size`, `on_divider_down`,
  `drag_pane_to`, `end_pane_drag`, `remember_pane_size`).

## K3b — rendering

`shell_view.go` — 5492–5559 frame overlay (`RING_WIDTH`,
`frame_overlay`), 5594 `impl Render` (`render`), 5736–6072 title row,
tabs, mode mark, `session_name`; 6164–6302 pane handle and drag surfaces.

`shell_view_sidebar.go` — 6303–6552 sidebar, filter row, catalog list;
9348–9746 `header_row`, `chevron`, `count_label`, `leaf_mark`,
`catalog_row`, `column_row`.

`shell_view_pane.go` — 6553–7832 pane, result body, `hit_handler`,
toolbars, column find control/popover/list, run timer, tx switch, schema
strip, connect strip, error strip, tx bar, tx button, run button;
7830–8192 query pane, history pane/list, placeholder; 8193 status strip;
10031 `no_result_note`, 10271 `delta_rows`, 10391 `delta_row`, 10583
`session_mark`, 10605 `cap_note`, 10633 `syntax_strip`.

`shell_view_overlays.go` — 8365 confirm overlay, 8509 peek overlay, 8679
palette overlay/header/chips/list/footer, 8877 `page_link`; 9747
`offset_at`, 9805 `keycap`, 9822 `key_badge`.
