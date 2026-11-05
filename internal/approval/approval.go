package approval

import (
	"context"
	"encoding/json"
)

type Decision int

const (
	Deny Decision = iota
	AllowOnce
	AllowAlways
)

func (d Decision) String() string {
	switch d {
	case AllowOnce:
		return "allow"
	case AllowAlways:
		return "always"
	default:
		return "deny"
	}
}

type Request struct {
	ID       string
	Tool     string
	Args     json.RawMessage
	ReadOnly bool
	Depth    int
}

type Approver interface {
	Approve(ctx context.Context, req Request) (Decision, error)
}

type Func func(ctx context.Context, req Request) (Decision, error)

func (f Func) Approve(ctx context.Context, req Request) (Decision, error) {
	return f(ctx, req)
}

var Auto = Func(func(context.Context, Request) (Decision, error) { return AllowAlways, nil })

var DenyAll = Func(func(context.Context, Request) (Decision, error) { return Deny, nil })
