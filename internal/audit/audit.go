// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package audit appends rows to audit_log. It is always called with the
// transaction of the mutation it records, so an action and its audit row
// commit or roll back together and the log can never disagree with the data.
package audit

import (
	"context"
	"encoding/json"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsx"
)

// Actor types.
const (
	User   = "user"
	Agent  = "agent"
	System = "system"
)

// Entry is one audit_log row. Zero ids and an empty IP are stored as NULL.
type Entry struct {
	Actor     string
	ActorType string // User, Agent or System
	Action    string // deploy | publish | approve | reject | rollback | create_page | …
	PageID    jsx.ID
	VersionID jsx.ID
	TokenID   jsx.ID
	IP        string
	Metadata  any // marshaled to JSON; nil ⇒ NULL
}

// Write inserts e using q (normally the mutation's transaction).
func Write(ctx context.Context, q db.Querier, e Entry) error {
	var meta []byte
	if e.Metadata != nil {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return err
		}
		meta = b
	}
	var ip any
	if e.IP != "" {
		ip = e.IP
	}
	var metaArg any
	if meta != nil {
		metaArg = string(meta)
	}
	_, err := q.Exec(ctx,
		`INSERT INTO audit_log (actor, actor_type, action, page_id, version_id, token_id, ip, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.Actor, e.ActorType, e.Action, e.PageID, e.VersionID, e.TokenID, ip, metaArg)
	return err
}
