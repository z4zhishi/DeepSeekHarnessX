package core

// ImmediateScheduler is a real synchronous scheduler implementation used by
// the core bootstrap until the production session scheduler is wired in.
type ImmediateScheduler struct{}
