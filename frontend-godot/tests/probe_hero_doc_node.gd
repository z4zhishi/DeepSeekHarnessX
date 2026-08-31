extends Node

## Headless probe for the engine-driven hero (plan Phase 3 acceptance).
##
## Drives the REAL runtime path — scenes/chrome/hero.tscn instantiated fresh,
## its script (hero.gd) mounting documents/hero_doc.gd through the
## ComponentRegistry/Reconciler:
##   1. document — hero_doc.build() is a valid DshUIDocument AST
##   2. mount    — _ready discards the legacy static scene children and mounts
##                 the engine tree (root column, gap 12, cards grid)
##   3. structure— brand mark + title + subtitle + exactly 4 suggestion cards
##                 with the legacy geometry (240x72 buttons, icon+title+desc)
##   4. events   — each card's pressed reaches suggestion_clicked with the
##                 _CARDS prompt (String action id roundtrip, reused nodes)
##   5. re-render— a locale flip re-renders texts IN PLACE (instance ids kept)
##   6. tokens   — a theme flip repaints colors/fonts via apply_tokens()
##   7. fallback — hero.tscn on disk still carries the legacy scene nodes
##
## Verdict line (grep this in CI):
##     HERO_DOC_RESULT passed=<p> failed=<f>

const DshReconT := preload("res://engine/reconciler.gd")
const DshHeroDocT := preload("res://documents/hero_doc.gd")

const SUBTITLE_EN := "High-performance agent workbench — code, inspect, refactor, orchestrate."
const SUBTITLE_ZH := "高性能 Agent 工作台 — 编码、检查、重构、编排工具。"
const CARD_KEYS := ["hero-card-0", "hero-card-1", "hero-card-2", "hero-card-3"]

var _passed := 0
var _failed := 0
var _clicked: Array = []
var _original_locale := "en"
var _original_mode: int = DshTokens.Mode.DARK


func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		print("  PASS: ", msg)
	else:
		_failed += 1
		print("  FAIL: ", msg)


func _frames(n: int) -> void:
	for _i in n:
		await get_tree().process_frame


## First engine node under [param node] carrying the document key [param key]
## (reconciler stores keys in the META_KEY metadata of materialized nodes).
func _find_keyed(node: Node, key: String) -> Control:
	if node is Control and str((node as Control).get_meta(DshReconT.META_KEY, "")) == key:
		return node as Control
	for child in node.get_children():
		var hit := _find_keyed(child, key)
		if hit != null:
			return hit
	return null


func _ids_of_keyed(hero: Control, keys: Array) -> Array:
	var ids: Array = []
	for key in keys:
		var found := _find_keyed(hero, str(key))
		ids.append(found.get_instance_id() if found != null else -1)
	return ids


func _ready() -> void:
	await _run()


func _run() -> void:
	_original_locale = DshI18n.get_locale()
	if _original_locale != "en" and _original_locale != "zh":
		_original_locale = "en"
	DshI18n.set_locale("en")

	# --- 1. document ----------------------------------------------------------
	print("== document ==")
	var doc: Dictionary = DshHeroDocT.build()
	_assert(not doc.is_empty(), "hero_doc.build() returned an AST")
	_assert(DshReconT.validate(doc).is_empty(), "AST passes reconciler validate()")
	_assert(str(doc.get("type", "")) == "column", "root is a column")
	_assert(int(DshHeroDocT._cards_block([]).get("children", []).size()) == 0,
			"empty card list yields an empty grid block")
	var buttons: Array = []
	var icons: Array = []
	for child in doc.get("children", []):
		_collect_types(child as Dictionary, buttons, icons)
	_assert(buttons.size() == 4, "document declares exactly 4 suggestion buttons")
	_assert(icons.size() == 5, "brand mark + 4 card icons are icon nodes")
	_assert(str(doc.get("key", "")) == "hero-root", "root key is the explicit hero-root, not a type fallback")
	var seen_keys := {}
	var dup := _collect_keys(doc, seen_keys)
	_assert(not dup and seen_keys.has("hero-title") and seen_keys.has("hero-card-0"),
			"document keys are unique and include hero-title / hero-card-0")

	# --- 2. live mount through the real view ---------------------------------
	print("== mount ==")
	var packed: PackedScene = load("res://scenes/chrome/hero.tscn") as PackedScene
	if packed == null:
		_assert(false, "hero.tscn loadable")
		_finish()
		return
	var hero: Control = packed.instantiate()
	add_child(hero)
	await _frames(4)
	_assert(hero.get_child_count() == 1,
			"legacy static scene children discarded, only the engine root remains")
	var engine_root: Control = hero.get_child(0) as Control
	_assert(engine_root is VBoxContainer, "engine root materializes as the hero column (VBoxContainer)")
	_assert(int(engine_root.get_theme_constant("separation")) == 12, "root column carries the legacy 12px separation")
	_assert(_find_keyed(hero, "hero-cards") is VBoxContainer, "cards block materializes as a VBoxContainer")
	_assert(_find_keyed(hero, "hero-card-row-0") is HBoxContainer, "card rows materialize as HBoxContainers")

	# --- 3. structure ----------------------------------------------------------
	print("== structure ==")
	var title := _find_keyed(hero, "hero-title") as Label
	_assert(title != null and title.text == "DSHX", "brand title rendered from the document")
	var mark := _find_keyed(hero, "hero-mark") as TextureRect
	_assert(mark != null, "brand mark materializes through the app-level icon type")
	_assert(mark != null and mark.texture != null, "brand mark carries the product texture")
	var subtitle := _find_keyed(hero, "hero-subtitle") as Label
	_assert(subtitle != null and subtitle.text == SUBTITLE_EN, "subtitle renders the EN copy")
	var cards: Array = []
	for i in 4:
		cards.append(_find_keyed(hero, "hero-card-%d" % i))
	var all_cards: bool = not cards.has(null)
	_assert(all_cards, "all 4 suggestion cards are reachable by key")
	if all_cards:
		var geo_ok := true
		for card in cards:
			geo_ok = geo_ok and (card as Button).custom_minimum_size == Vector2(240, 72)
		_assert(geo_ok, "cards keep the legacy 240x72 minimum size")
		var icon := _find_keyed(hero, "hero-card-0-icon") as TextureRect
		_assert(icon != null and icon.texture != null, "card icon materializes as a painted TextureRect")
		var desc := _find_keyed(hero, "hero-card-0-desc") as Label
		_assert(desc != null and desc.autowrap_mode == TextServer.AUTOWRAP_WORD_SMART,
				"card description renders with legacy word-wrap")
		var body: Control = _find_keyed(hero, "hero-card-0-body")
		_assert(body != null and body.mouse_filter == Control.MOUSE_FILTER_IGNORE,
				"card body container is mouse-transparent (legacy input contract)")
		var head: Control = _find_keyed(hero, "hero-card-0-head")
		_assert(head != null and head.mouse_filter == Control.MOUSE_FILTER_IGNORE,
				"card head container is mouse-transparent")
		var btn := cards[0] as Button
		_assert(btn.mouse_default_cursor_shape == Control.CURSOR_POINTING_HAND,
				"cards keep the pointing-hand cursor")
		var card_title := _find_keyed(hero, "hero-card-0-title") as Label
		var want_title := str(DshI18n.t("chat.suggestExplore"))
		if want_title == "chat.suggestExplore" or want_title.strip_edges() == "":
			want_title = "Explore workspace"
		_assert(card_title != null and card_title.text == want_title,
				"card title text renders from _CARDS + i18n")

	# --- 4. events --------------------------------------------------------------
	print("== events ==")
	hero.suggestion_clicked.connect(func(prompt: String): _clicked.append(prompt))
	var expected: Array = []
	var specs: Array = (load("res://scripts/ui/hero.gd") as GDScript)._CARDS
	for spec in specs:
		expected.append(str((spec as Dictionary)["prompt"]))
	for i in 4:
		(cards[i] as Button).pressed.emit()
	_assert(_clicked == expected, "card presses round-trip the _CARDS prompts through the String action ids")

	# --- 5. re-render in place (locale flip) -------------------------------------
	print("== locale re-render ==")
	var keys := ["hero-card-0", "hero-card-1", "hero-card-2", "hero-card-3", "hero-title", "hero-subtitle", "hero-mark"]
	var ids_before := _ids_of_keyed(hero, keys)
	DshI18n.set_locale("zh")
	await _frames(2)
	_assert(subtitle.text == SUBTITLE_ZH, "locale flip re-renders the subtitle text")
	var zh_title := _find_keyed(hero, "hero-card-0-title") as Label
	_assert(zh_title != null and zh_title.text == "探索工作区", "locale flip re-renders card titles")
	_assert(_ids_of_keyed(hero, keys) == ids_before, "apply_tokens() reuses the live nodes (no rebuild)")

	# --- 6. token repaint (theme flip) -------------------------------------------
	print("== token repaint ==")
	_original_mode = DshTokens.Mode.DARK
	DshTokens.mode = DshTokens.Mode.LIGHT
	hero.apply_tokens()
	var light_subtitle: Color = subtitle.get_theme_color("font_color")
	_assert(light_subtitle == DshTokens.text_secondary(), "LIGHT repaint applies the themed subtitle color")
	_assert(light_subtitle != Color("cfd3d6"), "LIGHT repaint is distinct from the DARK palette")
	var title_size: int = title.get_theme_font_size("font_size")
	_assert(title_size == 28, "title keeps the 28px task#21 display scale")
	DshTokens.mode = DshTokens.Mode.DARK
	hero.apply_tokens()
	_assert(subtitle.get_theme_color("font_color") == DshTokens.text_secondary(),
			"DARK repaint restores the themed subtitle color")

	# --- 7. legacy fallback on disk -----------------------------------------------
	print("== legacy fallback ==")
	var scene_file := FileAccess.open("res://scenes/chrome/hero.tscn", FileAccess.READ)
	if scene_file == null:
		_assert(false, "hero.tscn readable on disk")
	else:
		var content := scene_file.get_as_text()
		scene_file.close()
		_assert(content.contains("GridContainer") and content.contains("unique_name_in_owner"),
				"hero.tscn still declares the legacy structure (fallback intact, unmodified)")

	_restore_locale()
	_print_verdict()
	get_tree().quit(1 if _failed > 0 else 0)


func _finish() -> void:
	_restore_locale()
	_print_verdict()
	get_tree().quit(1 if _failed > 0 else 0)


func _restore_locale() -> void:
	DshI18n.set_locale(_original_locale)
	DshTokens.mode = _original_mode as int


func _print_verdict() -> void:
	print("")
	print("HERO_DOC_RESULT passed=%d failed=%d" % [_passed, _failed])


func _collect_keys(node: Dictionary, seen: Dictionary) -> bool:
	if node.is_empty():
		return false
	var key := str(node.get("key", ""))
	if key != "":
		if seen.has(key):
			return true
		seen[key] = true
	for child in node.get("children", []):
		if child is Dictionary and _collect_keys(child as Dictionary, seen):
			return true
	return false


func _collect_types(node: Dictionary, buttons: Array, icons: Array) -> void:
	if node.is_empty():
		return
	match str(node.get("type", "")):
		"button":
			buttons.append(node)
		"icon":
			icons.append(node)
	for child in node.get("children", []):
		if child is Dictionary:
			_collect_types(child as Dictionary, buttons, icons)