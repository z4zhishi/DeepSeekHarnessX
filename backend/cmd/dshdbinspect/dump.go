package main

import (
	"encoding/json"
	"fmt"
	"os"

	"dsh-go/pkg/storage"
)

// dump 子命令：把某会话全部事件写成 JSON 文件供前端复现与人工检查。
func dumpSession() {
	if len(os.Args) < 4 {
		fmt.Println("usage: dshdbinspect -dump <dataDir> <sessionID>")
		os.Exit(1)
	}
	st, err := storage.OpenSqliteStore(os.Args[2])
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer st.Close()
	evs, err := st.GetEvents(os.Args[3], 0)
	if err != nil {
		fmt.Println("getevents:", err)
		os.Exit(1)
	}
	type plain struct {
		Seq  int             `json:"seq"`
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	out := make([]plain, 0, len(evs))
	longest := 0
	for _, e := range evs {
		out = append(out, plain{Seq: e.Seq, Type: e.Type, Data: json.RawMessage(e.Data)})
		if len(e.Data) > longest {
			longest = len(e.Data)
		}
	}
	b, _ := json.MarshalIndent(out, "", " ")
	os.WriteFile("dumped_session.json", b, 0644)
	fmt.Printf("wrote dumped_session.json: %d events, longest payload %d bytes\n", len(out), longest)
}
