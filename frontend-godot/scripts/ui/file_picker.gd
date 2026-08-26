extends Control
class_name DshFilePicker

## 应用内文件/目录选择器：常驻预实例化的内建 FileDialog（use_native_dialog=false）
## 加一条自绘工具条（上级 / 地址栏 / 常用位置 / 最近记录）。
##
## 为什么这样封装：
## - Windows 原生对话框冷启动慢且视觉与应用割裂；内建对话框随主窗口主题渲染。
## - 内建对话框缺地址栏粘贴、快速入口与最近记录，工具条补齐这些触达性缺口。
## - 工具条只走 FileDialog 公共 API（current_dir / filters / 选择信号），
##   绝不触碰内部节点：headless 与内部控件懒构建场景下同样成立。
## - 工具条 set_as_top_level 后按视口坐标逐帧贴靠在对话框正上方，
##   拖动对话框时同步跟随；关闭即隐藏。
##
## 最近记录持久化取舍：工程现有持久化面只有零散 FileAccess（theme/locale），
## SessionStore 是后端会话的镜像，均不适合。故用 ConfigFile 存 user://file_picker.cfg，
## 按 bucket（调用方语义命名空间）分节，key=路径 value=最后使用时间戳。

signal dir_selected(dir: String)
signal files_selected(paths: PackedStringArray)
signal file_selected(path: String)

const RECENTS_FILE := "user://file_picker.cfg"
const MAX_RECENTS := 8
const BAR_HEIGHT := 36.0

const MODES := {
	"dir": FileDialog.FILE_MODE_OPEN_DIR,
	"files": FileDialog.FILE_MODE_OPEN_FILES,
	"file": FileDialog.FILE_MODE_OPEN_FILE,
	"any": FileDialog.FILE_MODE_OPEN_ANY,
}

## 最近记录命名空间（如 "workspace"）；空串 = 不记录。
var bucket := ""
## 常用位置附加项 [{label: String, path: String}]，由调用方按语义提供。
var quick_dirs: Array = []

var _dialog: FileDialog = null
var _bar: PanelContainer = null
var _addr: LineEdit = null
var _quick_btn: MenuButton = null
var _recent_btn: MenuButton = null
var _active := false
var _last_seen_dir := ""
var _warn_tween: Tween = null


func _ready() -> void:
	mouse_filter = Control.MOUSE_FILTER_IGNORE
	_build_dialog()
	_build_bar()
	# 平时整棵隐藏（工具条随本节点），open() 时再亮出；内嵌的 FileDialog 是
	# Window 子节点，不受父 Control 可见性影响，随时可弹。
	visible = false


func _build_dialog() -> void:
	_dialog = FileDialog.new()
	_dialog.use_native_dialog = false
	_dialog.access = FileDialog.ACCESS_FILESYSTEM
	_dialog.min_size = Vector2i(640, 440)
	_dialog.dir_selected.connect(_on_dialog_dir_selected)
	_dialog.files_selected.connect(_on_dialog_files_selected)
	_dialog.file_selected.connect(_on_dialog_file_selected)
	_dialog.visibility_changed.connect(_on_dialog_visibility)
	add_child(_dialog)
	# 视觉一致性：embedded 子窗口继承宿主 Theme（Label/Button/LineEdit 等已验证
	# 生效），这里只补 builder 未覆盖的窗口外框与标题色。默认框是 StyleBoxFlat，
	# 复制后仅改配色与圆角，保留其内容边距，避免内容被裁。
	var sb := ThemeDB.get_default_theme().get_stylebox("embedded_border", "Window")
	if sb is StyleBoxFlat:
		var border: StyleBoxFlat = sb.duplicate()
		border.bg_color = DshTokens.bg_layer1()
		border.border_color = DshTokens.border_l2()
		border.set_corner_radius_all(DshTokens.RADIUS_LG)
		_dialog.add_theme_stylebox_override("embedded_border", border)
	_dialog.add_theme_color_override("title_color", DshTokens.text_primary())


func _build_bar() -> void:
	_bar = PanelContainer.new()
	_bar.name = "PickerBar"
	_bar.set_as_top_level(true)
	_bar.mouse_filter = Control.MOUSE_FILTER_STOP
	_bar.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(), DshTokens.RADIUS_MD, DshTokens.border_l2(), 1,
		Vector4(8, 4, 8, 4)
	))
	_bar.visible = false
	add_child(_bar)

	var row := HBoxContainer.new()
	row.add_theme_constant_override("separation", 6)
	_bar.add_child(row)

	var up := Button.new()
	up.text = "↑"
	up.tooltip_text = _t("picker.upTip", "Parent folder (Backspace)")
	up.focus_mode = Control.FOCUS_NONE
	up.pressed.connect(_go_up)
	row.add_child(up)

	_addr = LineEdit.new()
	_addr.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_addr.placeholder_text = _t("picker.addrHint", "Type or paste a path, press Enter (Ctrl+L to focus)")
	_addr.text_changed.connect(_on_addr_edited)
	_addr.text_submitted.connect(_on_addr_submitted)
	_addr.focus_exited.connect(func():
		# 弃输回弹：地址栏失焦后立即对齐真实当前目录。
		_warn_clear()
	)
	row.add_child(_addr)

	_quick_btn = MenuButton.new()
	_quick_btn.text = _t("picker.places", "Places")
	_quick_btn.tooltip_text = _t("picker.placesTip", "Common locations")
	_quick_btn.focus_mode = Control.FOCUS_NONE
	_quick_btn.about_to_popup.connect(_fill_quick_menu)
	row.add_child(_quick_btn)

	_recent_btn = MenuButton.new()
	_recent_btn.text = _t("picker.recent", "Recent")
	_recent_btn.tooltip_text = _t("picker.recentTip", "Recently used")
	_recent_btn.focus_mode = Control.FOCUS_NONE
	_recent_btn.about_to_popup.connect(_fill_recent_menu)
	row.add_child(_recent_btn)


## 打开选择器。cfg 键（均可省略）：
##   mode: "dir" | "files" | "file"（目录多选单选文件）
##   title: String  start_dir: String  filters: PackedStringArray
##   ratio: float（占屏比，默认 0.7）
func open(cfg: Dictionary = {}) -> void:
	if _dialog == null:
		return
	var mode := str(cfg.get("mode", "dir"))
	_dialog.file_mode = MODES.get(mode, FileDialog.FILE_MODE_OPEN_DIR)
	_dialog.filters = cfg.get("filters", PackedStringArray())
	_dialog.title = str(cfg.get("title", ""))
	_dialog.ok_button_text = _t("picker." + mode + "_ok", {
		"dir": "Use This Folder",
		"files": "Select",
		"file": "Select",
		"any": "Select",
	}.get(mode, "Select"))
	var start := normalize_dir(str(cfg.get("start_dir", "")))
	if start != "" and not DirAccess.dir_exists_absolute(start):
		start = ""
	if start == "":
		# 未指定起点时落到最近一次使用过的目录，接续上次的操作位置
		# （原生对话框此前由 OS 记忆；内建化后由我们自己的最近记录承担）。
		var recents := load_recents(bucket)
		if recents.size() > 0 and DirAccess.dir_exists_absolute(recents[0]):
			start = recents[0]
	if start != "":
		_dialog.current_dir = start
	_active = true
	visible = true
	_last_seen_dir = ""
	_bar.visible = true
	_sync_bar(true)
	_dialog.popup_centered_ratio(float(cfg.get("ratio", 0.7)))
	_sync_bar(true)


func close() -> void:
	if _dialog != null and _dialog.visible:
		_dialog.hide()


func _process(_delta: float) -> void:
	if not _active or _dialog == null:
		return
	if not _dialog.visible:
		_deactivate()
		return
	_sync_bar(false)


func _unhandled_key_input(event: InputEvent) -> void:
	if not _active or _dialog == null or not _dialog.visible:
		return
	var k := event as InputEventKey
	if k == null or not k.pressed or k.echo:
		return
	if k.keycode == KEY_BACKSPACE and not (k.ctrl_pressed or k.alt_pressed or k.meta_pressed):
		var focus := get_viewport().gui_get_focus_owner()
		if focus is LineEdit or focus is TextEdit:
			return
		_go_up()
		get_viewport().set_input_as_handled()
	elif k.keycode == KEY_L and (k.ctrl_pressed or k.meta_pressed):
		_addr.grab_focus()
		_addr.select_all()
		get_viewport().set_input_as_handled()


## 工具条贴靠：与对话框同宽，紧贴其上沿；拖动时逐帧跟随。
func _sync_bar(force: bool) -> void:
	if _bar == null or _dialog == null:
		return
	var pos := Vector2(_dialog.position.x, _dialog.position.y - BAR_HEIGHT)
	pos.y = maxf(pos.y, 0.0)
	_bar.position = pos
	_bar.size = Vector2(_dialog.size.x, BAR_HEIGHT)
	if force or not _addr.has_focus():
		var cur := normalize_dir(_dialog.current_dir)
		if force or cur != _last_seen_dir:
			_last_seen_dir = cur
			if not _addr.has_focus():
				_addr.text = cur


func _deactivate() -> void:
	_active = false
	if _bar != null:
		_bar.visible = false
	visible = false


func _go_up() -> void:
	var cur := normalize_dir(_dialog.current_dir)
	var up := normalize_dir(cur.get_base_dir())
	if up == "" or up == cur:
		return
	if not DirAccess.dir_exists_absolute(up):
		return
	_dialog.current_dir = up


func _focus_address() -> void:
	_addr.grab_focus()
	_addr.select_all()


static func normalize_dir(p: String) -> String:
	var s := p.strip_edges().replace("\\", "/")
	if s == "":
		return ""
	s = s.simplify_path()
	while s.length() > 3 and s.ends_with("/"):
		s = s.substr(0, s.length() - 1)
	return s


# ---------- 地址栏 ----------

func _on_addr_edited(_text: String) -> void:
	# 用户输入中：清掉可能存在的错误提示色。
	if _warn_tween != null and _warn_tween.is_valid():
		_warn_tween.kill()
		_addr.remove_theme_color_override("font_color")


func _on_addr_submitted(text: String) -> void:
	var p := normalize_dir(text)
	if p != "" and DirAccess.dir_exists_absolute(p):
		_dialog.current_dir = p
		_last_seen_dir = normalize_dir(p)
		_addr.text = _last_seen_dir
		_warn_clear()
	else:
		# 自解释反馈：字面变警示色并回弹为当前目录，悬停可见原因。
		_addr.tooltip_text = _t("picker.badPath", "Path does not exist")
		if _warn_tween != null and _warn_tween.is_valid():
			_warn_tween.kill()
		_addr.add_theme_color_override("font_color", DshTokens.danger())
		_warn_tween = create_tween()
		_warn_tween.tween_interval(0.9)
		_warn_tween.tween_callback(_warn_clear)


func _warn_clear() -> void:
	_addr.remove_theme_color_override("font_color")
	_addr.text = normalize_dir(_dialog.current_dir)


# ---------- 快捷入口 / 最近记录 ----------

func _fill_quick_menu() -> void:
	var menu := _quick_btn.get_popup()
	menu.clear()
	menu.max_size = Vector2(480, 0)
	var home := home_dir()
	if home != "" and DirAccess.dir_exists_absolute(home):
		menu.add_item(home)
		menu.set_item_metadata(menu.item_count - 1, home)
	for entry in quick_dirs:
		if not (entry is Dictionary):
			continue
		var path := normalize_dir(str(entry.get("path", "")))
		if path == "" or not DirAccess.dir_exists_absolute(path):
			continue
		menu.add_item(str(entry.get("label", path)) + "  —  " + path)
		menu.set_item_metadata(menu.item_count - 1, path)
	if menu.item_count == 0:
		menu.add_item(_t("picker.noPlaces", "No common locations"))
		menu.set_item_disabled(menu.item_count - 1, true)
	if not menu.index_pressed.is_connected(_on_quick_picked):
		menu.index_pressed.connect(_on_quick_picked)


func _on_quick_picked(index: int) -> void:
	var menu := _quick_btn.get_popup()
	if menu.is_item_disabled(index):
		return
	var path := str(menu.get_item_metadata(index))
	if path != "" and DirAccess.dir_exists_absolute(path):
		_dialog.current_dir = path


func _fill_recent_menu() -> void:
	var menu := _recent_btn.get_popup()
	menu.clear()
	menu.max_size = Vector2(520, 0)
	var hint := _t("picker.recentNav", "Click to jump there")
	if bucket != "" and _dialog.file_mode == FileDialog.FILE_MODE_OPEN_DIR:
		hint = _t("picker.recentOpen", "Click to open directly")
	menu.add_item(hint)
	menu.set_item_disabled(0, true)
	var entries := load_recents(bucket)
	for path in entries:
		var label := path if path.length() <= 52 else "…" + path.right(50)
		menu.add_item(label)
		menu.set_item_metadata(menu.item_count - 1, path)
	if not menu.index_pressed.is_connected(_on_recent_picked):
		menu.index_pressed.connect(_on_recent_picked)
	if bucket != "" and not entries.is_empty():
		menu.add_separator()
		menu.add_item(_t("picker.recentClear", "Clear recent list"))
		menu.set_item_metadata(menu.item_count - 1, "__clear__")


func _on_recent_picked(index: int) -> void:
	var menu := _recent_btn.get_popup()
	if menu.is_item_disabled(index):
		return
	var meta := str(menu.get_item_metadata(index))
	if meta == "__clear__":
		clear_recents(bucket)
		_fill_recent_menu()
		return
	var path := meta
	if not DirAccess.dir_exists_absolute(path):
		return
	# 目录模式下最近项一步直达（自解释：菜单首行已注明）；其余模式跳转浏览。
	if bucket != "" and _dialog.file_mode == FileDialog.FILE_MODE_OPEN_DIR:
		_record_recent(path)
		dir_selected.emit(path)
		close()
	else:
		_dialog.current_dir = path


# ---------- 选择信号中继（信号名与载荷同 FileDialog，调用方语义不变） ----------

func _on_dialog_dir_selected(dir: String) -> void:
	_record_recent(dir)
	dir_selected.emit(dir)


func _on_dialog_files_selected(paths: PackedStringArray) -> void:
	_record_recent(normalize_dir(_dialog.current_dir))
	files_selected.emit(paths)


func _on_dialog_file_selected(path: String) -> void:
	_record_recent(normalize_dir(_dialog.current_dir))
	file_selected.emit(path)


func _on_dialog_visibility() -> void:
	if _dialog != null and not _dialog.visible:
		_deactivate()


# ---------- 最近记录持久化 ----------

func _record_recent(path: String) -> void:
	if bucket == "" or path.strip_edges() == "":
		return
	if not DirAccess.dir_exists_absolute(path):
		return
	var cf := ConfigFile.new()
	cf.load(RECENTS_FILE)
	var sec := "recents." + bucket
	cf.set_value(sec, path, Time.get_unix_time_from_system())
	var ranked := _rank_section(cf, sec)
	for i in range(MAX_RECENTS, ranked.size()):
		cf.set_value(sec, ranked[i][1], null)
	cf.save(RECENTS_FILE)


func load_recents(section_bucket: String) -> PackedStringArray:
	if section_bucket == "":
		return PackedStringArray()
	var cf := ConfigFile.new()
	cf.load(RECENTS_FILE)
	var out := PackedStringArray()
	for entry in _rank_section(cf, "recents." + section_bucket):
		out.append(str(entry[1]))
	return out


func clear_recents(section_bucket: String) -> void:
	if section_bucket == "":
		return
	var cf := ConfigFile.new()
	cf.load(RECENTS_FILE)
	cf.erase_section("recents." + section_bucket)
	cf.save(RECENTS_FILE)


## 返回 [(timestamp, path)] 按时间倒序。
static func _rank_section(cf: ConfigFile, sec: String) -> Array:
	var ranked: Array = []
	if cf.has_section(sec):
		for key in cf.get_section_keys(sec):
			ranked.append([float(cf.get_value(sec, key, 0.0)), key])
	ranked.sort_custom(func(a, b): return float(a[0]) > float(b[0]))
	return ranked


# ---------- 环境 ----------

## 用户主目录：Windows 取 USERPROFILE，类 Unix 取 HOME。
static func home_dir() -> String:
	var h := OS.get_environment("USERPROFILE")
	if h.strip_edges() == "":
		h = OS.get_environment("HOME")
	return normalize_dir(h)


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
