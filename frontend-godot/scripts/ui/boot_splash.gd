extends CanvasLayer

## scripts/ui/boot_splash.gd — 开屏 BootSplash（supremacy-plan §4 感知启动）。
##
## 定位：app 同一 viewport 内的首帧引导层（app.tscn 末位 CanvasLayer，
## layer=100 高于全部 overlay ≤50），非独立窗口进程。场景自带可见 →
## Godot 主场景首帧即呈现；app.gd _ready 第一行调 present()，先于任何
## backend await（真实客户端 WS/RPC 本就异步：dsh_client._connect_*）。
##
## 预算纪律：splash 帧零 Theme 构建——只用 DshTokens 现成颜色 + 两个
## StyleBox；logo 复用既有品牌资产 assets/brand/dshx_mark.svg（窗口图标
## 同源，引擎启动时已进导入缓存）。
##
## 收场编排（reveal，由 app._finish_boot 在主区域可用时调用）：
##   logo fling（1.0→1.04）+ 整层 180ms 淡出；主区域按 侧栏→主栏→composer
##   席位 3 步 stagger 显现（每步 DshTokens.MOTION_QUICK=100ms，间隔 70ms，
##   总长 ~240ms ≤ 400ms）。全部动效经 DshTokens.motion_enabled 门控：
##   off 时静态直出/直收，任何路径不留半透明残态。

const POP_DELAY_SEC := 0.12   # 首帧后 ~120ms 品牌标 pop_in（plan §4）
const FLING_SEC := 0.18       # logo fling + 整层淡出（plan 指定 180ms）
const REVEAL_STEP_SEC := 0.07 # 区域 stagger 间隔
const TRACK_W := 140.0
const PILL_W := 36.0
const SHIMMER_SPAN_SEC := 0.85
const SHIMMER_ALPHA := 0.55
const SHIMMER_STATIC_ALPHA := 0.45

@onready var _backdrop: ColorRect = $Fader/Backdrop
@onready var _fader: Control = $Fader
@onready var _logo: TextureRect = $Fader/Mid/Stack/Logo
@onready var _wordmark: Label = $Fader/Mid/Stack/Wordmark
@onready var _track: Panel = $Fader/Mid/Stack/Track
@onready var _pill: Panel = $Fader/Mid/Stack/Track/Pill

var _presented := false
var _revealed := false
var _shimmer_tw: Tween = null


func _ready() -> void:
	# frame-0 观感：不构建 Theme，只用 DshTokens 现值。底色与主界面
	# bg_base 一致，收场淡出时与主界面底无缝交接。
	_backdrop.color = DshTokens.bg_base()
	_logo.texture = DshIcons.brand()
	DshIcons.paint(_logo, true)
	_wordmark.add_theme_color_override("font_color", DshTokens.text_primary())
	_wordmark.add_theme_font_size_override("font_size", 20)
	_track.add_theme_stylebox_override("panel",
			DshTokens.box(DshTokens.bg_layer3(), DshTokens.RADIUS_SM, Color.TRANSPARENT, 0, Vector4.ZERO))
	_pill.add_theme_stylebox_override("panel",
			DshTokens.box(DshTokens.accent(), DshTokens.RADIUS_SM, Color.TRANSPARENT, 0, Vector4.ZERO))


func present() -> void:
	if _presented:
		return
	_presented = true
	visible = true
	if not DshTokens.motion_enabled:
		# 静态直出：logo/进度条直接就位，无任何插值。
		_logo.modulate.a = 1.0
		_pill.modulate.a = SHIMMER_STATIC_ALPHA
		return
	# 首帧先呈现底色/字标/进度轨道；~120ms 后品牌标 pop_in 落位。
	_logo.modulate.a = 0.0
	get_tree().create_timer(POP_DELAY_SEC).timeout.connect(_pop_logo)
	_shimmer_loop()


func _pop_logo() -> void:
	if not is_instance_valid(_logo) or not visible:
		return
	DshTokens.pop_in(_logo, DshTokens.MOTION_QUICK)


## 主区域可用点收场：regions = [侧栏, 主栏, composer 席位]（次序即显现序）。
## done 在整层隐藏后回调（app 用它落 boot_stage=3）。
func reveal(regions: Array, done: Callable = Callable()) -> void:
	if _revealed:
		if done.is_valid():
			done.call_deferred()
		return
	_revealed = true
	if not DshTokens.motion_enabled:
		# 静态：区域内全部直回不透明，层即时隐藏（无半透明残态）。
		for region in regions:
			if region is Control:
				(region as Control).modulate.a = 1.0
		if done.is_valid():
			done.call_deferred()
		_conceal()
		return
	if _shimmer_tw != null and _shimmer_tw.is_valid():
		_shimmer_tw.kill()
	# logo fling（1.0→1.04）+ 整层 180ms 淡出：淡出开始即 stage≥2，
	# 主区域自此才允许外露（此前整层不透明覆盖，transcript 无双绘）。
	var fade := DshTokens.motion_tween(_fader, false)
	fade.tween_property(_fader, "modulate:a", 0.0, FLING_SEC)
	fade.finished.connect(_conceal)
	var fling := DshTokens.motion_tween(_logo, true)
	_logo.pivot_offset = _logo.size * 0.5
	fling.tween_property(_logo, "scale", Vector2(1.04, 1.04), FLING_SEC)
	# 主区域 stagger：step i*70ms 间隔 + 每步 MOTION_QUICK(100ms)，
	# 总长 2*70+100≈240ms ≤ 400ms。
	for i in regions.size():
		var node: Variant = regions[i]
		if not (node is Control):
			continue
		var region := node as Control
		region.modulate.a = 0.0
		var tw := DshTokens.motion_tween(region, false)
		tw.tween_interval(i * REVEAL_STEP_SEC)
		tw.tween_property(region, "modulate:a", 1.0, DshTokens.MOTION_QUICK)


func _shimmer_loop() -> void:
	_pill.position = Vector2.ZERO
	_pill.modulate.a = SHIMMER_ALPHA
	var tw := DshTokens.motion_tween(_pill, false)
	tw.set_loops()
	var span := TRACK_W - PILL_W
	var to_end: PropertyTweener = tw.tween_property(_pill, "position:x", span, SHIMMER_SPAN_SEC)
	to_end.set_trans(Tween.TRANS_SINE)
	to_end.set_ease(Tween.EASE_IN_OUT)
	var back: PropertyTweener = tw.tween_property(_pill, "position:x", 0.0, SHIMMER_SPAN_SEC)
	back.set_trans(Tween.TRANS_SINE)
	back.set_ease(Tween.EASE_IN_OUT)
	_shimmer_tw = tw


func _conceal() -> void:
	if _shimmer_tw != null and _shimmer_tw.is_valid():
		_shimmer_tw.kill()
		_shimmer_tw = null
	visible = false