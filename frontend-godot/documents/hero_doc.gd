extends RefCounted

## Declarative document for the chat empty-state hero (plan Phase 3).
##
## The hero is expressed as a plain Dictionary AST built through
## DshUIDocument.node(): engine builtins render the structure
## (column/row/text/button/spacer) and ONE app-level type ("icon", a
## TextureRect slot for the brand mark and the card icons) is registered at
## runtime by register_components() onto the view's own registry instance.
## Nothing under engine/ changes and the reconcile semantics stay untouched.
##
## Ownership split (as documented in the engine files — never share a code
## path): this file owns the AST, the Reconciler owns node identity /
## prop diffing, HeroView owns token painting and the interaction contract.
##
## Card semantics (icon / key / title / desc / prompt) remain owned by
## HeroView (_CARDS): build() reads that const and maps every spec onto a
## card subtree
##     button > column[ row[icon, text(title)], text(desc) ]
## — nested engine builtins only, no custom card type. The click stays plain
## serializable data: a String action id "hero.suggest.<i>" dispatched by
## DshUIReconciler.action (no Callables travel inside the document).
## All copy resolves through DshI18n.t() at build time, so a locale flip only
## re-patches the changed texts during the next rebuild.

const DshDocT := preload("res://engine/ui_document.gd")
const DshRegistryT := preload("res://engine/component_registry.gd")
const DshIconsT := preload("res://scripts/ui/icons.gd")

## Visual role vocabulary — private contract with HeroView._paint_tree only.
## The document attaches these through the engine "mode" prop, which the
## reconciler stores as node metadata, so the document itself stays plain
## data (JSON round-trippable) and the app keeps its visual policy per role.
const ROLE_TITLE := "hero_title"
const ROLE_SUBTITLE := "hero_subtitle"
const ROLE_MARK := "hero_mark"
const ROLE_CARD := "hero_card"
const ROLE_CARD_BODY := "hero_card_body"
const ROLE_CARD_HEAD := "hero_card_head"
const ROLE_CARD_TITLE := "hero_card_title"
const ROLE_CARD_DESC := "hero_card_desc"
const ROLE_CARD_ICON := "hero_card_icon"

## Geometry constants ported 1:1 from the legacy hero.tscn / _build_cards so
## the engine render keeps the previous geometry (all values in px).
const ROOT_GAP := 12.0
const BRAND_GAP := 10.0
const CARD_GAP := 12.0
const CARD_BODY_GAP := 5.0
const CARD_HEAD_GAP := 8.0
const CARD_MIN_WIDTH := 240.0
const CARD_MIN_HEIGHT := 72.0
const BRAND_MARK_SIZE := 34.0
const CARD_ICON_SIZE := 16.0
const TITLE_FONT_SIZE := 28

## String action id prefix for the suggestion cards (see _card).
const ACTION_PREFIX := "hero.suggest."


## Registers the app-level component vocabulary onto [param registry]. Purely
## additive and scoped to the caller's reconciler instance: this uses the
## registry's designed override point (builtins are registered first, app
## types layer on top) so the icon slots can stay genuine TextureRects.
static func register_components(registry: DshRegistryT) -> void:
	if registry == null or registry.has("icon"):
		return
	registry.register("icon", _icon_factory)


## TextureRect factory for icon payloads. Construction-time props carry the
## icon name ("brand" selects the product mark) and the square size; the
## token tint (modulate) is re-applied by HeroView._paint_tree on theme
## flips. DshIcons.apply* also makes the rect mouse-transparent, exactly like
## the legacy hand-built card icons.
static func _icon_factory(props: Dictionary) -> Control:
	var rect := TextureRect.new()
	var icon := str(props.get("name", ""))
	var size_px := maxf(1.0, _to_f(props.get("size", CARD_ICON_SIZE)))
	if icon == "brand":
		DshIconsT.apply_brand(rect, size_px)
	else:
		DshIconsT.apply(rect, icon, size_px)
	return rect


## Builds the hero document: brand line + title, subtitle, and the suggestion
## grid read from HeroView._CARDS. Mounted by HeroView into its own
## CenterContainer, which centers the column at its minimum size exactly like
## the legacy static VBox did.
static func build() -> Dictionary:
	return DshDocT.node("column", {"gap": ROOT_GAP}, [
		_brand_line(),
		_subtitle_line(),
		_cards_block(_card_specs()),
	], "hero-root")


## Brand line: [expand-spacer, mark icon, fixed 10px, title, expand-spacer].
## The engine has no alignment prop, so the two expanding side spacers split
## the leftover width and center the group — the equivalent of the legacy
## Headline HBox with alignment=1; the fixed mid spacer reproduces the legacy
## headline separation (10).
static func _brand_line() -> Dictionary:
	return DshDocT.node("row", {"gap": 0.0}, [
		_expand_spacer("hero-brand-lead"),
		DshDocT.node("icon", {
			"name": "brand",
			"size": BRAND_MARK_SIZE,
			"mode": {"role": ROLE_MARK},
		}, [], "hero-mark"),
		DshDocT.node("spacer", {"min_width": BRAND_GAP}, [], "hero-brand-gap"),
		DshDocT.node("text", {
			"text": _t("chat.heroTitle", "DSHX"),
			"mode": {"role": ROLE_TITLE},
		}, [], "hero-title"),
		_expand_spacer("hero-brand-trail"),
	], "hero-brand")


## Subtitle line: [expand-spacer, text, expand-spacer] — same centering
## pattern as the brand line (the legacy subtitle used a centered label).
static func _subtitle_line() -> Dictionary:
	return DshDocT.node("row", {"gap": 0.0}, [
		_expand_spacer("hero-sub-lead"),
		DshDocT.node("text", {
			"text": _t("chat.heroSubtitle", "高性能 Agent 工作台"),
			"mode": {"role": ROLE_SUBTITLE},
		}, [], "hero-subtitle"),
		_expand_spacer("hero-sub-trail"),
	], "hero-subline")


## Suggestion grid expressed with nested rows (engine builtins have no
## GridContainer): cards pair up two per row inside a column, reproducing the
## legacy 2-column GridContainer (h/v separation 12). Each row centers the
## card pair with expanding side spacers and a fixed CARD_GAP mid spacer, so
## the pair keeps its legacy minimum (2 x 240 + 12) no matter how wide the
## subtitle line makes the column.
static func _cards_block(cards: Array) -> Dictionary:
	var rows: Array = []
	var index := 0
	var row_index := 0
	while index < cards.size():
		var row_children: Array = [_expand_spacer(_row_key(row_index, "lead"))]
		var pair := 0
		while pair < 2 and index < cards.size():
			if pair > 0:
				row_children.append(DshDocT.node(
						"spacer", {"min_width": CARD_GAP},
						[], _row_key(row_index, "gap")))
			row_children.append(_card(index, cards[index]))
			pair += 1
			index += 1
		row_children.append(_expand_spacer(_row_key(row_index, "trail")))
		rows.append(DshDocT.node("row", {"gap": 0.0}, row_children,
				_row_key(row_index, "")))
		row_index += 1
	return DshDocT.node("column", {"gap": CARD_GAP}, rows, "hero-cards")


## One suggestion card, engine builtins only: a Button carrying a nested
## column (the legacy body/head separation values live in the gap props).
## Click stays serializable data — "hero.suggest.<i>" — the view re-emits
## suggestion_clicked(_CARDS[i].prompt) when the reconciler dispatches it.
static func _card(index: int, spec: Dictionary) -> Dictionary:
	var key_hint := "hero-card-%d" % index
	return DshDocT.node("button", {
		"min_width": CARD_MIN_WIDTH,
		"min_height": CARD_MIN_HEIGHT,
		"on_click": ACTION_PREFIX + str(index),
		"mode": {"role": ROLE_CARD},
	}, [
		DshDocT.node("column", {"gap": CARD_BODY_GAP, "mode": {"role": ROLE_CARD_BODY}}, [
			DshDocT.node("row", {"gap": CARD_HEAD_GAP, "mode": {"role": ROLE_CARD_HEAD}}, [
				DshDocT.node("icon", {
					"name": str(spec.get("icon", "")),
					"size": CARD_ICON_SIZE,
					"mode": {"role": ROLE_CARD_ICON},
				}, [], key_hint + "-icon"),
				DshDocT.node("text", {
					"text": _t(str(spec.get("key", "")), str(spec.get("title", ""))),
					"mode": {"role": ROLE_CARD_TITLE},
				}, [], key_hint + "-title"),
			], key_hint + "-head"),
			DshDocT.node("text", {
				"text": _t(str(spec.get("desc_key", "")), str(spec.get("desc", ""))),
				"mode": {"role": ROLE_CARD_DESC},
			}, [], key_hint + "-desc"),
		], key_hint + "-body"),
	], key_hint)


## Zero-width spacer that expands to absorb leftover space (centers whatever
## it brackets, without inflating the row's minimum size).
static func _expand_spacer(key_hint: String) -> Dictionary:
	return DshDocT.node("spacer", {"expand": true}, [], key_hint)


static func _row_key(row_index: int, slot: String) -> String:
	if slot.strip_edges() == "":
		return "hero-card-row-%d" % row_index
	return "hero-card-row-%d-%s" % [row_index, slot]


## Card semantics stay owned by HeroView._CARDS (Phase 3 contract). Reached
## through a function-body load() — a top-level preload here would close a
## preload cycle, because hero.gd preloads this document builder.
static func _card_specs() -> Array:
	var hero_script: GDScript = load("res://scripts/ui/hero.gd")
	if hero_script == null:
		push_warning("hero_doc: cannot read HeroView script for _CARDS")
		return []
	return hero_script._CARDS


## Same fallback contract as HeroView._t(): DshI18n.t() hands back the key
## itself when a translation is missing, which swaps in the fallback. Kept as
## a tiny local copy so the documents layer never depends on the view beyond
## the _CARDS data.
static func _t(key: String, fallback: String) -> String:
	var value := str(DshI18n.t(key))
	return fallback if value == key or value.strip_edges() == "" else value


static func _to_f(value: Variant) -> float:
	if value is float or value is int:
		return float(value)
	return 0.0