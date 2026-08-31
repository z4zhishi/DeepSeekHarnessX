extends RefCounted

## Composer 的结构文档（Phase 3 engine 化的渲染契约源）。
##
## spec() 返回一个描述 composer.tscn + composer.gd 的 Dictionary：
##   * "root"：DshUIDocument 形状的 AST（type/key/props/children，可附 meta），
##     与 engine/ui_document.gd 的声明式文档契约对齐；"type" 使用 engine builtin
##     已覆盖的类型（column/row/text/spacer）、engine widgets 已注册的交互类型
##     （text_input/chip/icon_button/dropdown/list，icon 为 TextureRect 回退；
##     见 "planned_types" 的 status=registered），或仍未注册的语义类型
##     （composer/menu/file_picker，status=planned）。"meta" 携带今天与 Godot
##     节点的绑定信息（class / scene_unique / origin / signals / i18n），
##     迁移 bridge 专用，engine 渲染时应忽略或按需转译。
##   * "signals" / "api" / "behaviors"：composer 对外契约与运行时行为清单，
##     是静态 AST 表达不了、Phase 3 由 engine 侧接管的部分。
##
## 当前状态：文档先行。composer.gd 仍是唯一行为与视觉宿主（场景渲染），
## Composer 并未 engine-mount。build_doc() 仅回读本文档；交互控件的 registry
## factory 已落地（DshUIWidgets），但本组件仍由 composer.tscn 场景渲染。
## 修改 composer.tscn / composer.gd 的结构时必须同步维护本文件（probe
## tests/composer_doc_probe 会断言关键节点与 key 稳定）。

const SOURCE_SCENE := "res://scenes/chrome/composer.tscn"
const SOURCE_SCRIPT := "res://scripts/ui/composer.gd"
const SOURCE_DOC := "res://documents/composer_doc.gd"

## access 三态/五档预设（与 composer.gd 的 ACCESS_PRESETS 常量保持一致）。
const ACCESS_PRESETS: Array = ["default", "accept-edits", "plan", "auto", "allow-all"]


## 组装单个 AST 节点。meta 缺省不落盘，保持叶子节点尽量贴近四键契约。
static func _n(type: String, key: String, props: Dictionary, meta: Dictionary = {}, children: Array = []) -> Dictionary:
	var out := {
		"type": type,
		"key": key,
		"props": props,
		"children": children,
	}
	if not meta.is_empty():
		out["meta"] = meta
	return out


## spec() 主体：整个 composer 的结构描述文档。
static func spec() -> Dictionary:
	return {
		"version": 1,
		"kind": "dsh_composer_doc",
		"component": "composer",
		"source": {
			"scene": SOURCE_SCENE,
			"script": SOURCE_SCRIPT,
			"doc": SOURCE_DOC,
		},
		"root": _root(),
		"signals": _signals(),
		"api": _api(),
		"behaviors": _behaviors(),
		"planned_types": _planned_types(),
	}


## 按 key 深度优先查找 AST 节点；未命中返回空 Dictionary。
static func find(node: Dictionary, key: String) -> Dictionary:
	if str(node.get("key", "")) == key:
		return node
	for c in node.get("children", []):
		if c is Dictionary:
			var hit := find(c as Dictionary, key)
			if not hit.is_empty():
				return hit
	return {}


## 先序展平整个 AST（含根），供遍历/查重工具使用。
static func nodes(node: Dictionary) -> Array:
	var out: Array = [node]
	for c in node.get("children", []):
		if c is Dictionary:
			out.append_array(nodes(c as Dictionary))
	return out


# --- 文档分节 ----------------------------------------------------------------


## 根节点：面板壳 + 纵向栈（生成状态 / 两条动态 rail / 命令补全 / 输入区 / 动作行）。
## 运行期子节点同样如实收录：栈内插入的命令补全列表、根下挂载的文件选择器
## （_ready 即建）与 access 紧凑 popover（首次点击才建；常用 Ask/Edit 与其他分栏）。
## 结构镜像 composer.tscn + composer.gd 的真实树。
static func _root() -> Dictionary:
	var stack: Array = [
		_n("text", "gen_status",
			{"visible": false},
			{"class": "Label", "scene_unique": "GenStatus", "origin": "scene",
				"i18n": ["chat.generating", "chat.steerHint"],
				"behavior": "generating_pulse_dots", "pass_mouse": true}),
		_n("row", "queue_rail",
			{"gap": 6, "expand": true, "visible": false},
			{"class": "HBoxContainer", "scene_unique": "QueueRail", "origin": "scene",
				"clip_contents": true, "populates": "steer_queue_chip",
				"chips": "display-only（排队文案仅展示，无删除操作）"}),
		_n("row", "attach_rail",
			{"gap": 6, "visible": false},
			{"class": "HBoxContainer", "scene_unique": "AttachRail", "origin": "scene",
				"populates": "attachment_chip"}),
		_n("list", "cmd_palette",
			{"min_height": 108, "expand": true, "visible": false},
			{"class": "ItemList", "origin": "script_runtime", "build": "_build_cmd_list",
				"inserted_into": "composer_stack", "inserted_before": "prompt",
				"font_size": "FONT_CAPTION", "signals": ["item_clicked", "item_activated"]}),
		_n("text_input", "prompt",
			{"min_height": 44, "expand": true},
			{"class": "TextEdit", "scene_unique": "Prompt", "origin": "scene",
				"wrap": "boundary", "fit_content": true, "caret_blink": true,
				"placeholder_i18n": ["chat.placeholder", "chat.messageAgent"],
				"signals": ["text_changed", "resized"],
				"autosize": "44..140px，按可视折行行数 × FONT_BODY_LH"}),
		_n("row", "action_row",
			{"gap": 8},
			{"class": "HBoxContainer", "scene_unique": "Toolbar", "origin": "scene"},
			[
				_n("row", "left_chrome",
					{"gap": 6},
					{"class": "HBoxContainer", "scene_unique": "LeftChrome", "origin": "scene",
						"slot": "composer.left", "widgets": ["approval", "reject_all"]},
					[
						_n("chip", "access_chip",
							{"min_height": 28, "on_click": "composer.access_menu"},
							{"class": "Button", "scene_unique": "AccessBtn", "origin": "scene", "flat": true,
								"signal": "access_mode_requested", "options": ACCESS_PRESETS,
								"menu": "access_menu",
								"i18n": ["chat.approval", "chat.accessMode", "chat.accessAsk", "chat.accessEdit",
									"chat.accessGroupCommon", "chat.accessGroupMore"],
								"label_by": "_access_label(preset)",
								"habit_chain": "Ask (default) and Edit (accept-edits) first-class",
								"groups": {"common": ["default", "accept-edits"], "more": ["plan", "auto", "allow-all"]},
								"synced_by": "set_access_mode / session policy"}),
						_n("chip", "reject_all",
							{"min_height": 28, "toggle": true},
							{"class": "Button", "scene_unique": "RejectAllBtn", "origin": "scene", "flat": true,
								"signal": "reject_all_toggled", "persist": "user://approval_auto_reject.txt",
								"i18n": ["chat.rejectAll", "chat.rejectAllOn", "chat.rejectAllOff", "chat.rejectAllHint"]}),
					]),
				_n("spacer", "action_spacer",
					{"expand": true},
					{"class": "Control", "origin": "scene"}),
				_n("row", "right_chrome",
					{"gap": 6},
					{"class": "HBoxContainer", "scene_unique": "RightChrome", "origin": "scene",
						"slot": "composer.right", "widgets": ["model_effort", "attach", "send"]},
					[
						_n("chip", "model_effort",
							{"min_height": 28, "on_click": "composer.model_effort_pop"},
							{"class": "Button", "scene_unique": "ModelEffortBtn", "origin": "scene", "flat": true,
								"signals": ["model_selected", "effort_changed"],
								"i18n": ["chat.modelEffort"],
								"habit_chain": "model + effort stay open together"}),
						_n("icon_button", "overflow_button",
							{"min_width": 28, "min_height": 28, "text": "⋯"},
							{"class": "Button", "scene_unique": "OverflowBtn", "origin": "scene", "flat": true,
								"slot": "composer.overflow", "widgets": ["commands"],
								"customize_signal": "chrome_customize_requested",
								"i18n": ["chat.more", "chrome.customize"]}),
						_n("icon_button", "cmd_button",
							{"min_width": 28, "min_height": 28, "text": "/", "visible": false},
							{"class": "Button", "scene_unique": "CmdBtn", "origin": "scene", "flat": true,
								"style": "round", "i18n": ["chat.commands"],
								"behavior": "prefix '/' + 补全弹层聚焦"}),
						_n("dropdown", "model_picker",
							{"min_width": 160, "min_height": 28, "disabled": true, "visible": false},
							{"class": "OptionButton", "scene_unique": "ModelPicker", "origin": "scene",
								"signal": "model_selected", "fit_to_longest_item": false,
								"synced_by": "set_models(models, selected)",
								"i18n": ["common.model"]}),
						_n("icon_button", "attach_button",
							{"min_width": 28, "min_height": 28, "on_click": "composer.attach_picker"},
							{"class": "Button", "scene_unique": "AttachBtn", "origin": "scene", "flat": true,
								"i18n": ["chat.attach"]},
							[_n("icon", "attach_icon",
								{"glyph": "paperclip", "glyph_size": 16},
								{"class": "TextureRect", "scene_unique": "AttachIcon", "origin": "scene"})]),
						_n("icon_button", "send_button",
							{"min_width": 34, "min_height": 34, "on_click": "composer.send"},
							{"class": "Button", "scene_unique": "SendBtn", "origin": "scene", "flat": true,
								"signals": ["prompt_submitted", "stop_requested"],
								"i18n": ["chat.sendTooltip", "chat.stopTooltip"],
								"behavior": "generating 时切 stop 语义（Esc 同）；禁用规则见 behaviors.send_state"},
							[_n("icon", "send_icon",
								{"glyph": "send", "glyph_alt": "stop", "glyph_size": 16},
								{"class": "TextureRect", "scene_unique": "SendIcon", "origin": "scene",
									"stateful": "send <-> stop（随 _generating 切换）"})]),
					]),
			]),
	]
	var stack_node := _n("column", "composer_stack",
		{"gap": 8},
		{"class": "VBoxContainer", "origin": "scene"}, stack)
	return _n("composer", "composer",
		{"min_height": 72},
		{"class": "PanelContainer", "script_class": "ComposerBar",
			"scene": SOURCE_SCENE, "script": SOURCE_SCRIPT,
			"origin": "scene+script",
			"layout": "size_flags_horizontal = SHRINK_CENTER",
			"width_cap": "custom_minimum_size.x = min(DshTokens.COMPOSER_MAX, seat 宽)",
			"stylebox": "DshTokens.bg_input 圆角卡 + 单层短距投影（apply_tokens）"},
		[stack_node] + [
			_n("file_picker", "file_picker",
				{"visible": false},
				{"class": "DshFilePicker", "origin": "script_runtime", "created_in": "_ready",
					"bucket": "attachments",
					"signals": ["files_selected", "file_selected"]}),
			_n("menu", "access_menu",
				{"visible": false},
				{"class": "PanelContainer", "origin": "script_lazy", "created_in": "_on_access_pressed",
					"top_level": true, "motion": ["pop_in", "slide_in_y"],
					"groups": {"common": ["default", "accept-edits"], "more": ["plan", "auto", "allow-all"]},
					"presets": ACCESS_PRESETS, "signal": "access_mode_requested"}),
		])


## 对外信号契约（composer.gd 顶部声明，一一对应）。
static func _signals() -> Dictionary:
	return {
		"prompt_submitted": ["text: String", "attachments: Array"],
		"stop_requested": [],
		"command_submitted": ["line: String"],
		"model_selected": ["id: String"],
		"access_mode_requested": ["preset: String"],
		"effort_changed": ["effort: String"],
		"reject_all_toggled": ["enabled: bool"],
		"chrome_customize_requested": [],
	}


## 外部调用契约（宿主 app.gd 等对 composer 的操作面）。
static func _api() -> Array:
	return [
		{"method": "set_generating", "args": ["generating: bool"],
			"purpose": "生成态切换：GenStatus 显隐、三点动效启停、Send 图标语义切换、清空 queue"},
		{"method": "set_commands", "args": ["commands: Array"],
			"purpose": "命令补全候选集（Dictionary{name,description} 或纯字符串）"},
		{"method": "set_enabled", "args": ["enabled: bool"],
			"purpose": "整体可用性：prompt 编辑、附件/命令/access 钮、model picker、Send 联动"},
		{"method": "set_models", "args": ["models: Array", "selected: String"],
			"purpose": "重建 ModelPicker 项并选中指定 id；空列表时禁用"},
		{"method": "selected_model", "args": [], "returns": "String",
			"purpose": "当前选中模型 id（无选中返回 \"\"）"},
		{"method": "set_access_mode", "args": ["preset: String"],
			"purpose": "同步 access chip 文案与菜单勾选（不发信号）"},
		{"method": "current_access_mode", "args": [], "returns": "String",
			"purpose": "当前审批预设 id（default / accept-edits / plan / auto / allow-all）"},
		{"method": "reload_chrome", "args": [],
			"purpose": "重读 chrome_layout.json 并按 catalog 槽位 remount"},
		{"method": "grab_input_focus", "args": [],
			"purpose": "聚焦 prompt 输入区"},
		{"method": "get_draft", "args": [], "returns": "String",
			"purpose": "读取草稿（prompt 原文）"},
		{"method": "set_draft", "args": ["text: String"],
			"purpose": "写入草稿并联动 grow/补全/Send 态"},
		{"method": "apply_tokens", "args": [],
			"purpose": "主题令牌重刷（样式、图标、文案、chip/圆钮着色）"},
		{"method": "set_effort", "args": ["effort: String"],
			"purpose": "同步思考等级（high/low/max/off），刷新模型·effort 按钮文案"},
		{"method": "current_effort", "args": [], "returns": "String",
			"purpose": "当前思考等级"},
		{"method": "set_reject_all", "args": ["enabled: bool"],
			"purpose": "同步自动拒绝审批开关（不发信号）"},
		{"method": "is_reject_all", "args": [], "returns": "bool"},
		{"method": "is_generating", "args": [], "returns": "bool"},
	]


## 静态 AST 表达不了的运行时行为，Phase 3 engine 渲染必须逐项接管。
static func _behaviors() -> Array:
	return [
		{"id": "keyboard_input", "source": "_input",
			"summary": "Esc：补全弹层关闭 > 生成中 stop（均置 handled）；Enter/KP-Enter 提交（Shift 换行放行、IME 输入中放行）；Tab/Up/Down 仅在补全弹层可见时生效；审批弹窗（dsh_approval 组可见）时全量放行不拦截"},
		{"id": "submit_routing", "source": "_submit",
			"summary": "空文本仅在有附件时放行；生成中提交转为 queue chip（/ 开头走 command_submitted，其余 prompt_submitted）；附件 paths 以 \"@path\" 追加进文本尾部"},
		{"id": "send_state", "source": "_refresh_send_state/_refresh_send_icon",
			"summary": "generating 时始终可点（=stop）；未启用禁用；否则 empty-text+empty-attachments 禁用"},
		{"id": "generating_pulse_dots", "source": "_start_gen_pulse/_on_gen_tick",
			"summary": "0.38s Timer 循环重写省略号（0..3），DshTokens.motion_enabled 关闭时静态"},
		{"id": "steer_queue", "source": "_add_queue_chip/_clear_queue",
			"summary": "生成中提交的后续指令以 chip 展示（截断 36 字符，tooltip 全文，display-only），generating 结束清空"},
		{"id": "cmd_palette", "source": "_refresh_cmd_popup/_build_cmd_list",
			"summary": "仅当文本以 '/' 开头且无空白时按前缀/子串过滤候选；选中后回填 '/name ' 并聚焦行尾"},
		{"id": "autosize", "source": "_grow/_cap_width",
			"summary": "prompt 按 wrap 后可视行数自增高（44..140px 钳制）；宽度钳制到 COMPOSER_MAX 并随 seat resized 重算"},
		{"id": "attachments", "source": "_add_attachment/_on_files_dropped/_open_picker",
			"summary": "选择/拖放追加附件 chip（去重）；rail 空时隐藏；提交时并入文本"},
		{"id": "access_menu_sync", "source": "_on_access_pressed/_set_access/set_access_mode",
			"summary": "紧凑 popover：常用 Ask(default)/Edit(accept-edits) 一等入口，其他预设在「其他」；选中即应用并关闭"},
		{"id": "chrome_slots", "source": "_setup_chrome/reload_chrome",
			"summary": "catalog ids 按 layout JSON 槽位 composer.left/right/overflow 挂载；reload_chrome 重读并 remount"},
		{"id": "model_tooltip", "source": "_refresh_model_tooltip",
			"summary": "tooltip 反映当前选中项；空列表回落到 i18n 占位"},
		{"id": "i18n_refresh", "source": "_apply_strings",
			"summary": "监听 DshI18n.locale_changed，重刷 placeholder/tooltip/chip 文案/Send 图标语义"},
	]


## Phase 3 类型盘点。builtin column/row/text/button/panel/scroll/spacer 已覆盖，未列出。
## status=registered：DshUIWidgets 已提供 skip-if-has factory，registry.create 可实例化。
## status=planned：尚未注册。Composer 本身仍由 composer.tscn 场景渲染，未 engine-mount。
static func _planned_types() -> Dictionary:
	return {
		"text_input": {"class": "TextEdit", "status": "registered",
			"note": "engine widget factory（TextEdit, wrap + min height 36）；Composer 仍场景渲染"},
		"chip": {"class": "Button", "status": "registered",
			"note": "flat clip_text Button；pill 着色仍在场景侧"},
		"icon_button": {"class": "Button", "status": "registered",
			"note": "28x28 flat Button"},
		"dropdown": {"class": "OptionButton", "status": "registered"},
		"list": {"class": "ItemList", "status": "registered"},
		"icon": {"class": "TextureRect", "status": "registered",
			"note": "通用 TextureRect 回退；Hero 的 DshIcons factory 若先注册则跳过不覆盖。DshIcons glyph_alt 状态切换仍待行为层"},
		"composer": {"class": "PanelContainer", "status": "planned",
			"note": "组件型根类型，或拆回 builtin panel + 行为层。Composer 仍由 composer.tscn 场景渲染，未 engine-mount"},
		"menu": {"class": "PanelContainer", "status": "planned",
			"note": "access 紧凑 popover（Ask/Edit 分组）；top_level PanelContainer，非 PopupMenu"},
		"file_picker": {"class": "DshFilePicker", "status": "planned",
			"note": "应用内文件选择器，bucket 语义保留"},
	}


# --- MOUNT 文档（engine 挂载用，区别于上方 spec() 的结构契约文档）--------------
#
# 本节为 W10-c Composer engine mount 追加：build_mount_doc() 产出可由
# DshUIReconciler.mount()/update() 直接消费的 DshUIDocument AST 裸节点
# （不带 spec() 的 version/kind/… 外壳）。分工：engine 渲染用
# build_mount_doc()，spec() 仍是静态结构契约（含 meta，迁移 bridge 专用），
# 两者永不共享代码路径。挂载文档沿用 spec() 的 key 命名（含根
# "composer_stack"），使 reconciler 在刷新/换源时按 key 复用已有节点。
#
# 确定性约束（硬）：build_mount_doc() 是 (options) 的纯函数——不读
# composer.gd、不调 DshI18n/DshTokens、不取时间/随机数。相同 options 的
# 两次调用产出深度相等的文档，且节点/prop 插入顺序稳定，reconciler 的
# keyed diff 在 options 不变时全部命中复用（patched == 0）。
#
# 行为层所有权约束（为什么不落某些 prop）：
#   * prompt 永不落 "text"：reconciler 的 text 通路会直接写 TextEdit.text，
#     locale/主题刷新重建文档时会把用户草稿覆盖成文档值；草稿（同 tooltip、
#     chip 文案、Send 图标语义）一律由行为层挂载后回填。
#   * rail/palette 不落 "visible"：显隐是运行态（生成中/排队/补全可见），
#     由行为层驱动；仅"落槽外 widget"例外，以 visible:false 兜底常驻，
#     保住 key 身份供后续槽位重排复用。
#
# 槽位顺序规则（options.left/right 为 chrome widget id 序列）：
#   * 去重：同一 id 在左右合计只挂一次——首次出现生效，重复出现一律丢弃；
#     不在 WIDGET_KEYS 里的 id 丢弃。
#   * right_chrome 固定尾部：右槽 ids -> overflow_button（永远在场，永不进
#     槽位列表）-> cmd_button（仅当 "commands" 不被任何槽位收编时，紧贴
#     model_picker 之前补挂）-> 落槽外 widget（WIDGET_KEYS 顺序，
#     visible:false 兜底常驻）-> model_picker（永远在场、永远最后）。

## 文档 key -> composer.tscn unique_name_in_owner 场景名。两处例外按注册名
## 约定补齐（行为层以 mode.scene_unique 统一寻址）："cmd_palette"（运行期
## _build_cmd_list 新建的 ItemList，无场景唯一名）、"action_spacer"（tscn
## 未开启 unique_name_in_owner）；其余均与 composer.tscn 真实 unique 名一致。
const SCENE_UNIQUE := {
	"gen_status": "GenStatus",
	"queue_rail": "QueueRail",
	"attach_rail": "AttachRail",
	"cmd_palette": "CmdPalette",
	"prompt": "Prompt",
	"action_row": "Toolbar",
	"left_chrome": "LeftChrome",
	"reject_all": "RejectAllBtn",
	"access_chip": "AccessBtn",
	"action_spacer": "ActionSpacer",
	"right_chrome": "RightChrome",
	"model_effort": "ModelEffortBtn",
	"overflow_button": "OverflowBtn",
	"cmd_button": "CmdBtn",
	"model_picker": "ModelPicker",
	"attach_button": "AttachBtn",
	"attach_icon": "AttachIcon",
	"send_button": "SendBtn",
	"send_icon": "SendIcon",
}

## chrome 槽位 widget id -> 文档 key（key 与 spec() 一致，保持两套文档间
## key 身份可迁移）。插入顺序即 known id 典范顺序（用于落槽外兜底排序）。
const WIDGET_KEYS := {
	"approval": "access_chip",
	"reject_all": "reject_all",
	"model_effort": "model_effort",
	"attach": "attach_button",
	"send": "send_button",
	"commands": "cmd_button",
}

## options 未提供槽位时的出厂布局（镜像 composer.tscn 的默认 chrome 形态）。
const DEFAULT_LEFT: Array = ["approval", "reject_all"]
const DEFAULT_RIGHT: Array = ["model_effort", "attach", "send"]

## DshUIDocument 契约的挂载侧构建入口。文档层保持零依赖（不 load 脚本，
## 不触 autoload），与 spec() 不同的是产出可直接交给 DshUIReconciler 的
## 根节点本身。
const DshDocT := preload("res://engine/ui_document.gd")


## 构建 engine 挂载文档：返回且仅返回可挂载的根 AST 节点（无 spec 外壳）。
## 根为 column、key="composer_stack"，与 spec() 的栈 key 一致，保证
## reconciler 在两次刷新间身份稳定（options 不变时 patched == 0）。
## options 约定（全部可选）：
##   "left"/"right" : Array 槽位 widget id 序列（未知 id 丢弃；去重规则与
##     尾部固定形态见节首注释）。缺省取 DEFAULT_LEFT/DEFAULT_RIGHT。
##   "placeholder"  : String prompt 占位文案；缺省不落 placeholder prop
##     （占位文案由行为层按 i18n 驱动）。
static func build_mount_doc(options: Dictionary = {}) -> Dictionary:
	var placed := {}  # widget id -> 已挂载（去重：首次出现生效）
	var left_children := _slot_children(options.get("left", DEFAULT_LEFT), placed)
	var right_children := _slot_children(options.get("right", DEFAULT_RIGHT), placed)
	# 右列固定尾部（见节首槽位顺序规则）：overflow 永远在右槽之后、永不进
	# 槽位列表。
	right_children.append(_overflow_node())
	# "commands" 未被任何槽位收编时，紧贴 model_picker 之前补挂（无 visible
	# prop：槽位归属决定运行态可见性）。
	if not placed.has("commands"):
		right_children.append(_widget_node("commands", false))
	# 落槽外 widget（known 但左右都不在）：按 WIDGET_KEYS 顺序以
	# visible:false 兜底常驻（"commands" 走上面的补挂规则，不在此列）。
	for wid in WIDGET_KEYS:
		var wid_id := str(wid)
		if wid_id != "commands" and not placed.has(wid_id):
			right_children.append(_widget_node(wid_id, true))
	# model_picker 永远在场、永远右列最后。
	right_children.append(_model_picker_node())
	return DshDocT.node("column", {"gap": 8}, [
		DshDocT.node("text", {"mode": {"scene_unique": String(SCENE_UNIQUE["gen_status"])}},
				[], "gen_status"),
		DshDocT.node("row", {"gap": 6, "expand": true,
				"mode": {"scene_unique": String(SCENE_UNIQUE["queue_rail"])}},
				[], "queue_rail"),
		DshDocT.node("row", {"gap": 6,
				"mode": {"scene_unique": String(SCENE_UNIQUE["attach_rail"])}},
				[], "attach_rail"),
		DshDocT.node("list", {"min_height": 108.0, "expand": true,
				"mode": {"scene_unique": String(SCENE_UNIQUE["cmd_palette"])}},
				[], "cmd_palette"),
		_prompt_node(options),
		DshDocT.node("row", {"gap": 8.0,
				"mode": {"scene_unique": String(SCENE_UNIQUE["action_row"])}}, [
			DshDocT.node("row", {"gap": 6.0, "mode": {
					"scene_unique": String(SCENE_UNIQUE["left_chrome"]),
					"slot": "composer.left"}}, left_children, "left_chrome"),
			DshDocT.node("spacer", {"expand": true,
					"mode": {"scene_unique": String(SCENE_UNIQUE["action_spacer"])}},
					[], "action_spacer"),
			DshDocT.node("row", {"gap": 6.0, "mode": {
					"scene_unique": String(SCENE_UNIQUE["right_chrome"]),
					"slot": "composer.right"}}, right_children, "right_chrome"),
		], "action_row"),
	], "composer_stack")


## 槽位 id 序列 -> widget 子节点数列。守卫：非 Array 视为空槽；不在
## WIDGET_KEYS 里的 id 丢弃；同一 id 首次出现生效（placed 记账），左右
## 重复或槽内重复一律跳过。
static func _slot_children(ids: Variant, placed: Dictionary) -> Array:
	var out: Array = []
	if not (ids is Array):
		return out
	for entry in ids as Array:
		var wid := str(entry)
		if not WIDGET_KEYS.has(wid) or placed.has(wid):
			continue
		placed[wid] = true
		out.append(_widget_node(wid, false))
	return out


## chrome widget id -> AST 节点（key 固定；type 取 registry 已注册的
## factory 类型；mode 携带 scene_unique 与 widget_id）。文档只落几何与
## mode：文案、信号绑定、语义可见性全部归行为层。out_of_slot=true 时追加
## visible:false 兜底（"commands" 永不走该通路——其显隐由槽位归属决定）。
static func _widget_node(id: String, out_of_slot: bool) -> Dictionary:
	var widget_key := String(WIDGET_KEYS.get(id, ""))
	if widget_key.is_empty():
		return {}
	var props: Dictionary = {}
	var children: Array = []
	var wtype := "chip"
	match id:
		"attach":
			wtype = "icon_button"
			props["min_width"] = 28.0
			props["min_height"] = 28.0
			children = [_icon_node("attach_icon", "paperclip")]
		"send":
			wtype = "icon_button"
			props["min_width"] = 34.0
			props["min_height"] = 34.0
			children = [_icon_node("send_icon", "send")]
		"commands":
			wtype = "icon_button"
			props["min_width"] = 28.0
			props["min_height"] = 28.0
			props["text"] = "/"
	if out_of_slot:
		props["visible"] = false
	props["mode"] = {"scene_unique": String(SCENE_UNIQUE[widget_key]), "widget_id": id}
	return DshDocT.node(wtype, props, children, widget_key)


## 图标子节点（TextureRect 回退 factory 约定的构造期 props：glyph +
## glyph_size）。
static func _icon_node(icon_key: String, glyph: String) -> Dictionary:
	return DshDocT.node("icon",
			{"glyph": glyph, "glyph_size": 16.0,
				"mode": {"scene_unique": String(SCENE_UNIQUE[icon_key])}},
			[], icon_key)


## 右列溢出按钮：永远在场，永不进槽位列表（chrome 自定义入口）。
static func _overflow_node() -> Dictionary:
	return DshDocT.node("icon_button",
			{"min_width": 28.0, "min_height": 28.0, "text": "⋯",
				"mode": {"scene_unique": String(SCENE_UNIQUE["overflow_button"])}},
			[], "overflow_button")


## 模型选择器：初始禁用且隐藏，行为层经 set_models 接管。
static func _model_picker_node() -> Dictionary:
	return DshDocT.node("dropdown",
			{"min_width": 160.0, "min_height": 28.0, "disabled": true,
				"visible": false,
				"mode": {"scene_unique": String(SCENE_UNIQUE["model_picker"])}},
			[], "model_picker")


## prompt 输入区：几何 + 可选 placeholder（options 显式给出 String 才落）。
## 永不落 "text"——reconciler 的 text 通路会直接写 TextEdit.text，刷新时
## 会把草稿覆盖为文档值；草稿由行为层单独回填。
static func _prompt_node(options: Dictionary) -> Dictionary:
	var props := {"min_height": 44.0, "expand": true}
	var placeholder: Variant = options.get("placeholder")
	if placeholder is String:
		props["placeholder"] = placeholder
	props["mode"] = {"scene_unique": String(SCENE_UNIQUE["prompt"])}
	return DshDocT.node("text_input", props, [], "prompt")