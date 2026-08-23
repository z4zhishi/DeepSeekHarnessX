extends PanelContainer
class_name ReasoningBox

## 可折叠推理卡：消费 usage.reasoningTokens + 时间戳显示真实 meta。
## begin 记录起始时间，finish 传真实 tokens 并计算耗时，MetaLabel 显示
## "X.XXs  N tokens"。

@onready var toggle_btn: Button = $%ToggleBtn
@onready var content_label: RichTextLabel = $%ReasoningContent
@onready var meta_label: Label = $%MetaLabel

var _expanded: bool = false
var _start_ms: int = 0

func _ready() -> void:
	toggle_btn.pressed.connect(_toggle)

func begin() -> void:
	content_label.text = ""
	meta_label.text = ""
	_expanded = true
	content_label.visible = true
	_start_ms = Time.get_ticks_msec()
	toggle_btn.text = "Reasoning…"

func append_delta(text: String) -> void:
	content_label.text += text
	content_label.fit_content = true

## finish: 用真实 usage.reasoningTokens（tokens）与真实耗时（由 begin 时间戳计算）。
## 兼容 main.gd 现有 finish(0, tokens) 调用：elapsed<=0 时按 begin 到现在计算。
func finish(elapsed_ms: int = -1, tokens: int = 0) -> void:
	var elapsed := elapsed_ms
	if elapsed <= 0:
		elapsed = Time.get_ticks_msec() - _start_ms
	meta_label.text = "%.2fs  %d tokens" % [elapsed / 1000.0, tokens]
	toggle_btn.text = "Reasoning (" + str(tokens) + " tokens)"

func _toggle() -> void:
	_expanded = not _expanded
	content_label.visible = _expanded
	toggle_btn.text = "Hide" if _expanded else "Show"
