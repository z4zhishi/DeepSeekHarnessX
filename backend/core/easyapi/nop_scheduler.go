package easyapi

import "dsh-go/core"

type nopScheduler struct{}

func (nopScheduler) CancelOwner(string)                {}
func (nopScheduler) CancelSession(core.SessionContext) {}
func (nopScheduler) Active() int                       { return 0 }
func (nopScheduler) Tick()                             {}
