package mcp

// 极简 TOML 子集解析器（零新依赖，仓库"不引入新依赖需先确认"约束下不引入
// 完整 TOML 库）。仅覆盖 Codex mcp_servers 配置所需构造：
//   - # 注释、空行（BOM 容忍）；
//   - 节头 [a.b.c]，段为裸键（A-Za-z0-9_-）或引号键（"…"/'…'），按未引号 '.'
//     分段，支持 [mcp_servers."name.with.dots"]；
//   - key = value，值支持：基本字符串（\t \n \r \b \f \" \\ \/ \uXXXX），
//     字面字符串 '…'，十进制整数，布尔，单行数组（标量元素，允许尾逗号），
//     单行内联表（标量值）；
//   - 值/节头之后允许 # 注释；数组与内联表必须单行收口；
//   - 不支持并在遇到时 fail loud（含行号）：多行字符串/多行数组、点号键、
//     表数组 [[…]]、浮点、日期时间、数字下划线分隔、嵌套数组/嵌套内联表、
//     键与节的重复定义。

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// parseTOMLSubset 按上述子集解析 TOML，返回节路径 → 值的树
// （map[string]any，叶值 string / int64 / bool / []any）。
func parseTOMLSubset(data []byte) (map[string]any, error) {
	root := map[string]any{}
	current := root
	declared := map[string]bool{}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	for i, rawLine := range strings.Split(text, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if strings.HasPrefix(line, "[[") {
				return nil, tomlErrf(lineNo, "不支持表数组 [[...]]")
			}
			end := indexUnquoted(line, ']')
			if end < 0 {
				return nil, tomlErrf(lineNo, "节头缺少右括号 ]")
			}
			if err := expectTOMLEOL(line[end+1:], lineNo); err != nil {
				return nil, err
			}
			path, err := parseTOMLKeyPath(line[1:end], lineNo)
			if err != nil {
				return nil, err
			}
			id := strings.Join(path, "\x00")
			if declared[id] {
				return nil, tomlErrf(lineNo, "节 [%s] 重复定义", strings.Join(path, "."))
			}
			declared[id] = true
			cur, err := ensureTOMLTable(root, path, lineNo)
			if err != nil {
				return nil, err
			}
			current = cur
			continue
		}
		// 键值行
		eq := indexUnquoted(line, '=')
		if eq < 0 {
			return nil, tomlErrf(lineNo, "缺少赋值号 =")
		}
		key, err := parseTOMLSingleKey(strings.TrimSpace(line[:eq]), lineNo)
		if err != nil {
			return nil, err
		}
		value, err := parseTOMLValue(strings.TrimSpace(line[eq+1:]), lineNo)
		if err != nil {
			return nil, err
		}
		if _, dup := current[key]; dup {
			return nil, tomlErrf(lineNo, "键 %q 重复定义", key)
		}
		current[key] = value
	}
	return root, nil
}

// ensureTOMLTable 沿路径逐段创建/复用子表；与已有标量值冲突时报错。
func ensureTOMLTable(root map[string]any, path []string, lineNo int) (map[string]any, error) {
	cur := root
	for _, seg := range path {
		next, ok := cur[seg]
		if !ok {
			m := map[string]any{}
			cur[seg] = m
			cur = m
			continue
		}
		m, ok := next.(map[string]any)
		if !ok {
			return nil, tomlErrf(lineNo, "节路径 [%s] 与已有标量值冲突", strings.Join(path, "."))
		}
		cur = m
	}
	return cur, nil
}

// parseTOMLSingleKey 解析单个键名（裸键或引号键）；点号键报错（子集外）。
func parseTOMLSingleKey(s string, lineNo int) (string, error) {
	if s == "" {
		return "", tomlErrf(lineNo, "空的键名")
	}
	seg, rest, err := scanTOMLKeySeg(s, lineNo)
	if err != nil {
		return "", err
	}
	if tail := strings.TrimSpace(rest); tail != "" {
		if tail[0] == '.' {
			return "", tomlErrf(lineNo, "不支持点号键（%q）", s)
		}
		return "", tomlErrf(lineNo, "键名后有多余内容 %q", tail)
	}
	return seg, nil
}

// parseTOMLKeyPath 解析节头路径（按未引号 '.' 分段，支持引号段）。
func parseTOMLKeyPath(s string, lineNo int) ([]string, error) {
	var parts []string
	rest := strings.TrimSpace(s)
	if rest == "" {
		return nil, tomlErrf(lineNo, "空的节路径")
	}
	for {
		seg, tail, err := scanTOMLKeySeg(rest, lineNo)
		if err != nil {
			return nil, err
		}
		parts = append(parts, seg)
		rest = strings.TrimSpace(tail)
		if rest == "" {
			return parts, nil
		}
		if rest[0] != '.' {
			return nil, tomlErrf(lineNo, "节路径段 %q 后缺少 '.'", seg)
		}
		rest = strings.TrimSpace(rest[1:])
		if rest == "" {
			return nil, tomlErrf(lineNo, "节路径以 '.' 结尾")
		}
	}
}

// scanTOMLKeySeg 返回（段值, 余串）：引号键或裸键。
func scanTOMLKeySeg(s string, lineNo int) (string, string, error) {
	switch s[0] {
	case '"':
		return scanBasicString(s, lineNo)
	case '\'':
		return scanLiteralString(s, lineNo)
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '-' || c == '_' ||
			c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return "", "", tomlErrf(lineNo, "非法键起始字符 %q", string(s[0]))
	}
	return s[:i], s[i:], nil
}

// parseTOMLValue 解析一个 TOML 值（含行尾校验：值后只允许空白或注释）。
func parseTOMLValue(s string, lineNo int) (any, error) {
	if s == "" {
		return nil, tomlErrf(lineNo, "缺少值")
	}
	switch s[0] {
	case '"':
		v, rest, err := scanBasicString(s, lineNo)
		if err != nil {
			return nil, err
		}
		if err := expectTOMLEOL(rest, lineNo); err != nil {
			return nil, err
		}
		return v, nil
	case '\'':
		v, rest, err := scanLiteralString(s, lineNo)
		if err != nil {
			return nil, err
		}
		if err := expectTOMLEOL(rest, lineNo); err != nil {
			return nil, err
		}
		return v, nil
	case '[':
		return parseTOMLArray(s, lineNo)
	case '{':
		return parseTOMLInlineTable(s, lineNo)
	}
	token := s
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		token = s[:i]
	}
	if err := expectTOMLEOL(s[len(token):], lineNo); err != nil {
		return nil, err
	}
	switch token {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if !isDecimalInt(token) {
		return nil, tomlErrf(lineNo, "不支持的值 %q（本子集仅支持字符串/整数/布尔/单行数组/单行内联表）", token)
	}
	n, err := strconv.ParseInt(token, 10, 64)
	if err != nil {
		return nil, tomlErrf(lineNo, "整数 %q 超出范围", token)
	}
	return n, nil
}

func isDecimalInt(token string) bool {
	body := token
	if len(body) > 0 && (body[0] == '+' || body[0] == '-') {
		body = body[1:]
	}
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return false
		}
	}
	return true
}

// parseTOMLArray 解析单行数组（标量元素，允许尾逗号；嵌套结构报错）。
func parseTOMLArray(s string, lineNo int) (any, error) {
	end := indexUnquoted(s, ']')
	if end < 0 {
		return nil, tomlErrf(lineNo, "数组未在本行收口（不支持多行数组）")
	}
	if err := expectTOMLEOL(s[end+1:], lineNo); err != nil {
		return nil, err
	}
	parts := splitTOMLLine(s[1:end])
	items := []any{}
	for idx, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			if idx == len(parts)-1 {
				continue // 空数组或尾逗号
			}
			return nil, tomlErrf(lineNo, "数组存在空元素")
		}
		v, err := parseTOMLValue(part, lineNo)
		if err != nil {
			return nil, err
		}
		if _, nested := v.(map[string]any); nested {
			return nil, tomlErrf(lineNo, "数组元素不支持内联表")
		}
		if _, nested := v.([]any); nested {
			return nil, tomlErrf(lineNo, "不支持嵌套数组")
		}
		items = append(items, v)
	}
	return items, nil
}

// parseTOMLInlineTable 解析单行内联表（标量值；嵌套表报错，值后只允许注释）。
func parseTOMLInlineTable(s string, lineNo int) (any, error) {
	end := indexUnquoted(s, '}')
	if end < 0 {
		return nil, tomlErrf(lineNo, "内联表未在本行收口（不支持多行）")
	}
	if err := expectTOMLEOL(s[end+1:], lineNo); err != nil {
		return nil, err
	}
	body := strings.TrimSpace(s[1:end])
	m := map[string]any{}
	if body == "" {
		return m, nil
	}
	for _, part := range splitTOMLLine(body) {
		part = strings.TrimSpace(part)
		eq := indexUnquoted(part, '=')
		if eq < 0 {
			return nil, tomlErrf(lineNo, "内联表项 %q 缺少 =", part)
		}
		key, err := parseTOMLSingleKey(strings.TrimSpace(part[:eq]), lineNo)
		if err != nil {
			return nil, err
		}
		v, err := parseTOMLValue(strings.TrimSpace(part[eq+1:]), lineNo)
		if err != nil {
			return nil, err
		}
		if _, nested := v.(map[string]any); nested {
			return nil, tomlErrf(lineNo, "内联表不支持嵌套表")
		}
		if _, dup := m[key]; dup {
			return nil, tomlErrf(lineNo, "内联表键 %q 重复", key)
		}
		m[key] = v
	}
	return m, nil
}

// scanBasicString 解析以 s[0]=='"' 开始的基本字符串，返回（值, 余串）。
func scanBasicString(s string, lineNo int) (string, string, error) {
	if strings.HasPrefix(s, `"""`) {
		return "", "", tomlErrf(lineNo, `不支持多行字符串 """`)
	}
	var sb strings.Builder
	i := 1
	for i < len(s) {
		switch c := s[i]; c {
		case '"':
			return sb.String(), s[i+1:], nil
		case '\\':
			if i+1 >= len(s) {
				return "", "", tomlErrf(lineNo, "字符串转义不完整")
			}
			switch e := s[i+1]; e {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case '"', '\\', '/':
				sb.WriteByte(e)
			case 'u', 'U':
				width := 4
				if e == 'U' {
					width = 8
				}
				if i+2+width > len(s) {
					return "", "", tomlErrf(lineNo, "\\%c 转义不完整", e)
				}
				code, err := strconv.ParseUint(s[i+2:i+2+width], 16, 32)
				if err != nil {
					return "", "", tomlErrf(lineNo, "\\%c 转义无效", e)
				}
				sb.WriteRune(rune(code))
			default:
				return "", "", tomlErrf(lineNo, "不支持的转义 \\%c", e)
			}
			i += 2
		default:
			sb.WriteByte(c)
			i++
		}
	}
	return "", "", tomlErrf(lineNo, "字符串未闭合")
}

// scanLiteralString 解析以 s[0]=='\'' 开始的字面字符串（无转义），返回（值, 余串）。
func scanLiteralString(s string, lineNo int) (string, string, error) {
	if strings.HasPrefix(s, "'''") {
		return "", "", tomlErrf(lineNo, "不支持多行字符串 '''")
	}
	end := strings.IndexByte(s[1:], '\'')
	if end < 0 {
		return "", "", tomlErrf(lineNo, "字符串未闭合")
	}
	return s[1 : 1+end], s[2+end:], nil
}

// splitTOMLLine 在未引号 ',' 处分段（引号内逗号不拆；基本字符串内的 \ 转义
// 跳过下一字符）。
func splitTOMLLine(s string) []string {
	parts := []string{}
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if quote == '"' && c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case ',':
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// indexUnquoted 返回 s 中首个未被引号包裹的 sep 字节下标；找不到返回 -1。
func indexUnquoted(s string, sep byte) int {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if quote == '"' && c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case sep:
			return i
		}
	}
	return -1
}

// expectTOMLEOL 校验值/节头之后只剩空白或注释。
func expectTOMLEOL(rest string, lineNo int) error {
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.HasPrefix(rest, "#") {
		return nil
	}
	return tomlErrf(lineNo, "之后有多余内容 %q", rest)
}

func tomlErrf(lineNo int, format string, args ...any) error {
	return fmt.Errorf("第 %d 行: %s", lineNo, fmt.Sprintf(format, args...))
}