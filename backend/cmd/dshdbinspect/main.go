package main

// dshdbinspect 只读检查一个 DSH SQLite 数据目录：会话清单、事件数、
// 单事件大小分布与类型直方图。用于排查前端加载历史崩溃。
import (
	"strings"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"dsh-go/pkg/session"
	"dsh-go/pkg/storage"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-dump" {
		dumpSession()
		return
	}
	_ = strings.TrimSpace
	dir := os.Args[1]
	st, err := storage.OpenSqliteStore(dir)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer st.Close()
	headers, err := st.ListSessions()
	if err != nil {
		fmt.Println("list:", err)
		return
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].CreatedAt > headers[j].CreatedAt })
	for _, h := range headers {
		evs, err := st.GetEvents(h.ID, 0)
		if err != nil {
			fmt.Printf("session %s: GetEvents ERROR: %v\n", h.ID, err)
			continue
		}
		maxSz, total := 0, 0
		biggestType, biggestSeq := "", 0
		hist := map[string]int{}
		for _, e := range evs {
			sz := len(e.Data)
			total += sz
			if sz > maxSz {
				maxSz, biggestType, biggestSeq = sz, e.Type, e.Seq
			}
			hist[e.Type]++
		}
		fmt.Printf("session %s origin=%s events=%d totalBytes=%d maxEvent=%d(%s seq=%d)\n",
			h.ID, h.Origin, len(evs), total, maxSz, biggestType, biggestSeq)
		types := make([]string, 0, len(hist))
		for t := range hist {
			types = append(types, t)
		}
		sort.Strings(types)
		for _, t := range types {
			fmt.Printf("    %-28s x%d\n", t, hist[t])
		}
	}
	_ = json.Marshal // keep import honest if unused later
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = session.SessionEnvelope{}
