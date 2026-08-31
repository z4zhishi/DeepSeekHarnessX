extends CenterContainer
class_name HeroView

## Chat empty-state hero view (plan Phase 3: engine-driven rendering).
##
## The static business nodes historically declared in scenes/chrome/hero.tscn
## (VBox / Headline / Mark / Title / Subtitle / Grid + hand-built card
## buttons) are replaced by a DshUIDocument AST: documents/hero_doc.gd builds
## the hero document, DshUIReconciler materializes it into this
## CenterContainer at _ready, and apply_tokens() re-renders in place — the
## reconciler diffs the new document against the live tree, so a locale or
## theme flip re-patches only what changed and reuses every surviving node.
## hero.tscn itself stays untouched on disk as the legacy fallback; its
## static children are discarded at runtime here.
##
## Ownership split (per the engine doc comments — never share a code path):
##   Reconciler    node identity / keyed diffing
##   Registry      document type -> Godot class mapping (+ app-level "icon")
##   this script   _CARDS semantics, suggestion_clicked contract, and all
##                 DshTokens-based painting (document roles -> theme overrides)

signal suggestion_clicked(prompt: String)

## 建议卡语义与文案（Phase 3 保留在本文件）：documents/hero_doc.gd 读取该清单
## 渲染各卡；点击经 string action id（"hero.suggest.<i>"）回映射到 prompt。
const _CARDS := [
	{"icon": "terminal", "key": "chat.suggestExplore", "title": "Explore workspace", "desc_key": "chat.suggestExploreDesc", "desc": "Index the tree and explain entry points", "prompt": "Please explore and explain the architecture of this workspace."},
	{"icon": "plan", "key": "chat.suggestPlan", "title": "Draft a plan", "desc_key": "chat.suggestPlanDesc", "desc": "Outline steps before making edits", "prompt": "/plan on"},
	{"icon": "diff", "key": "chat.suggestDiff", "title": "Review git diff", "desc_key": "chat.suggestDiffDesc", "desc": "Inspect local changes and regressions", "prompt": "Check current git diff and review recent changes."},
	{"icon": "check", "key": "chat.suggestTest", "title": "Run tests", "desc_key": "chat.suggestTestDesc", "desc": "Execute the suite and explain failures", "prompt": "Run the project test suite and report results."},
]

# 跨文件类型走 preload 常量而非全局 class_name（仓库惯例，见
# reconciler.gd / probe_ui_engine.gd 的注释），解析不依赖全局类缓存新鲜度。
const DshHeroDocT := preload("res://documents/hero_doc.gd")
const DshReconT := preload("res://engine/reconciler.gd")
const DshIconsT := preload("res://scripts/ui/icons.gd")

# 主标题字号（task #21 的视觉决策，保持不变）。
const TITLE_FONT_SIZE := 28

var _recon: DshReconT = null
var _root: Control = null


func _ready() -> void:
	size_flags_horizontal = Control.SIZE_EXPAND_FILL
	size_flags_vertical = Control.SIZE_EXPAND_FILL
	if DshI18n.has_signal("locale_changed"):
		DshI18n.locale_changed.connect(func(_loc: String): apply_tokens())
	_discard_legacy_scene_nodes()
	_recon = DshReconT.new()  # _init 内自动注册 builtin 类型
	DshHeroDocT.register_components(_recon.registry)
	_recon.action.connect(_on_hero_action)
	apply_tokens()


## 保持 Phase 3 之前的外部契约：app.gd 在主题切换时调用，本文件在
## _ready / 语言切换（DshI18n.locale_changed）时调用。
## 重建 hero 文档并交给 reconciler diff：首次挂载整棵树，之后按 key 复用，
## 仅对真正变化的 props（如文案）打补丁；随后重刷 token 上色。
func apply_tokens() -> void:
	if _recon == null:
		return
	_recon.update(self, DshHeroDocT.build())
	_root = null
	if has_meta(DshReconT.META_ROOT):
		_root = get_meta(DshReconT.META_ROOT) as Control
	if _root != null and is_instance_valid(_root):
		_paint_tree(_root)


## 丢弃 hero.tscn 里遗留的静态子节点（VBox/Headline/Grid 等）：文档树现在是
## 唯一渲染内容；磁盘上的 hero.tscn 保持原样，作为 legacy fallback。
func _discard_legacy_scene_nodes() -> void:
	for child in get_children():
		remove_child(child)
		child.free()


## String action id（建议卡点击）→ suggestion_clicked(prompt)：
## 文档保持纯数据（无 Callable），卡序号即 _CARDS 下标。
func _on_hero_action(action_name: String) -> void:
	if not action_name.begins_with(DshHeroDocT.ACTION_PREFIX):
		return
	var index := action_name.trim_prefix(DshHeroDocT.ACTION_PREFIX).to_int()
	if index < 0 or index >= _CARDS.size():
		return
	suggestion_clicked.emit(str((_CARDS[index] as Dictionary)["prompt"]))


## Token 上色（视觉策略全部收敛在本文件）：递归遍历 engine 挂载树，按文档
## "mode" 角色（reconciler 保存为节点元数据）套用 DshTokens 主题覆盖。
## 此 pass 不增删、不移动节点——身份仍由 reconciler 持有；每次 apply_tokens()
## 幂等重跑（主题/语言切换后重生效）。
func _paint_tree(node: Control) -> void:
	_paint_node(node)
	for child in node.get_children():
		if child is Control:
			_paint_tree(child as Control)


func _paint_node(node: Control) -> void:
	if node == null or not is_instance_valid(node):
		return
	if not node.has_meta(DshReconT.META_MODE):
		return
	var hint: Variant = node.get_meta(DshReconT.META_MODE)
	if not (hint is Dictionary):
		return
	var role := str((hint as Dictionary).get("role", ""))
	if role == DshHeroDocT.ROLE_TITLE:
		# 视觉重设计（task #21）：主标题字号保持，色阶降为 text_primary。
		node.add_theme_color_override("font_color", DshTokens.text_primary())
		node.add_theme_font_size_override("font_size", TITLE_FONT_SIZE)
	elif role == DshHeroDocT.ROLE_SUBTITLE:
		# 副标题从 tertiary 提到 secondary——空态是产品开场，不应显得灰暗。
		node.add_theme_color_override("font_color", DshTokens.text_secondary())
		node.add_theme_font_size_override("font_size", DshTokens.FONT_UI)
	elif role == DshHeroDocT.ROLE_MARK:
		DshIconsT.paint(node as TextureRect, true)
	elif role == DshHeroDocT.ROLE_CARD:
		_paint_card(node as Button)
	elif role == DshHeroDocT.ROLE_CARD_BODY or role == DshHeroDocT.ROLE_CARD_HEAD:
		# 旧 _build_cards 的显式 IGNORE：卡片内层容器不得吞掉按钮的点击/悬停。
		node.mouse_filter = Control.MOUSE_FILTER_IGNORE
	elif role == DshHeroDocT.ROLE_CARD_TITLE:
		node.add_theme_color_override("font_color", DshTokens.text_primary())
		node.add_theme_font_size_override("font_size", DshTokens.FONT_UI)
	elif role == DshHeroDocT.ROLE_CARD_DESC:
		var desc := node as Label
		if desc != null:
			desc.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
			desc.add_theme_color_override("font_color", DshTokens.text_tertiary())
			desc.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	elif role == DshHeroDocT.ROLE_CARD_ICON:
		DshIconsT.paint(node as TextureRect, false)


func _paint_card(card: Button) -> void:
	if card == null:
		return
	# Apple 化（截图审出缺陷 4 / task #23）：无悬浮——hover 只抬亮不加投影；
	# 标题与描述的行距放宽（由文档 gap 承担），层次用色阶区分。
	var pad := Vector4(16, 12, 16, 12)
	var rest := DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_LG, DshTokens.border_l1(), 1, pad)
	var hov := DshTokens.box(DshTokens.bg_layer3(), DshTokens.RADIUS_LG, DshTokens.border_l2(), 1, pad)
	var prs := DshTokens.box(DshTokens.pressed_layer(), DshTokens.RADIUS_LG, Color(0, 0, 0, 0), 0, pad)
	card.add_theme_stylebox_override("normal", rest)
	card.add_theme_stylebox_override("hover", hov)
	card.add_theme_stylebox_override("pressed", prs)
	card.add_theme_stylebox_override("focus", card.get_theme_stylebox("normal"))
	card.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND
	# Button is not a Container; pin the engine-built body so title/desc/icon
	# fill the 240x72 hit target instead of sitting at (0,0) with zero layout.
	for child in card.get_children():
		if child is Control:
			var body := child as Control
			body.set_anchors_preset(Control.PRESET_FULL_RECT)
			body.offset_left = 0.0
			body.offset_top = 0.0
			body.offset_right = 0.0
			body.offset_bottom = 0.0


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v