// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package portals is partner portals: ONE shared client credential over an
// admin-curated SET of pages, plus the membership the page switcher reads.
//
// Authority is the whole point of this package. A partner portal decides which
// client's numbers a given credential opens, so an agent must never be able to
// add a page to one, or move a page between two of them — that is the
// difference between "West sees his six Fabrikam dashboards" and "West sees
// NWM's". Every mutation therefore calls AssertPortalAdmin FIRST, before any
// validation or database work, and the MCP tool surface registers nothing from
// this package: portals add zero agent-reachable surface.
//
// The transaction and audit SHAPE is copied from internal/workspaces; its
// guards deliberately are NOT. workspaces.AssertOrganizer accepts agents, the
// exact inverse of what portals need, so portals carry their own guard and
// their own portal_admin_only error code — a grep for one can never turn up
// the other.
//
// This package does not participate in serving: the content host reads
// page_portals and page_portal_members itself (internal/contentview).
package portals

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/versions"
)

// MaxPortalsPerPage caps fan-out. A page in N portals means N candidate scrypt
// verifications on one password submission (each 30-80 ms), so fan-out is
// capped where it is created rather than where it would hurt. Four covers the
// real shape — a dashboard shared between a client portal, an agency portal
// and a per-person subset.
const MaxPortalsPerPage = 4

// MaxSortOrder is the ceiling on a membership's sort_order. The browser module
// (public/shell-assets/portals.js) keeps its own copy so the admin UI can
// append a member without proposing a value the write would reject; a test
// holds the two to each other.
const MaxSortOrder = 9999

// MinPasswordLength is the portal password floor. A portal password opens
// every dashboard in the portal, so it is worth more than a per-page one and
// is held to a floor versions.SetPassword has never had. The generated form is
// the intended path; the floor exists for the admin who pastes their own.
const MinPasswordLength = 16

const (
	maxNameLength     = 100
	maxLabelLength    = 200
	maxSlugLength     = 64
	maxPasswordLength = 512 // pagecookie.HashPassword truncates past this
	// 31 symbols with no look-alikes (no i/l/o/0/1): 16 characters is ~79
	// bits, well clear of the ~64-bit floor, and still readable down a phone
	// line.
	passwordAlphabet    = "abcdefghjkmnpqrstuvwxyz23456789"
	passwordGroups      = 4
	passwordGroupLength = 4
)

// Mutations names every mutation in this package (by its JavaScript name, as
// the audit-facing vocabulary). The authority test is table-driven over this
// list and over the package's exported functions, so a verb added without a
// matching case — or exported without being listed — fails it.
var Mutations = []string{
	"create",
	"rename",
	"setPassword",
	// Not a write to the portal, but it hands out its credential and writes
	// an audit row, so it is held to exactly the same human-only rule.
	"revealPassword",
	"addPage",
	"updatePage",
	"removePage",
	"setHome",
	"remove",
}

// AssertPortalAdmin admits a human admin, always (403 portal_admin_only
// otherwise). Not "an authenticated principal": the SSO audience is wider than
// staff and bearer tokens belong to agents, so this is the narrowest actor the
// codebase has (csrf.RequireAdminJSON is what puts ActorType "user" on the
// request in the first place).
func AssertPortalAdmin(a versions.Actor) error {
	if a.ActorType != audit.User {
		return apierror.Forbidden(
			"partner portals are admin-only: who may see which dashboards is a human decision",
			"portal_admin_only", nil)
	}
	return nil
}

// ── validation ──────────────────────────────────────────────────────────────

const maxSafeInteger = 1<<53 - 1

var decimalIDRE = regexp.MustCompile(`^[1-9][0-9]*$`)

// NormalizeID validates a database id: a positive safe-integer number or a
// decimal string within BIGINT range. field names the input in the error
// ("<field> must be a positive database id", 400 bad_<field>); "" means
// "portal_id".
func NormalizeID(v any, field string) (int64, error) {
	if field == "" {
		field = "portal_id"
	}
	switch x := v.(type) {
	case float64:
		if x > 0 && x == math.Trunc(x) && x <= maxSafeInteger {
			return int64(x), nil
		}
	case int:
		if x > 0 && x <= maxSafeInteger {
			return int64(x), nil
		}
	case int64:
		if x > 0 && x <= maxSafeInteger {
			return x, nil
		}
	case jsx.ID:
		if x > 0 {
			return int64(x), nil
		}
	case string:
		if decimalIDRE.MatchString(x) {
			if n, err := strconv.ParseInt(x, 10, 64); err == nil {
				return n, nil
			}
		}
	}
	return 0, apierror.BadRequest(field+" must be a positive database id", "bad_"+field, nil)
}

var portalSlugRE = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*$`)

// NormalizeSlug validates a portal slug: ONE url-safe segment, because the
// portal URL is <content-host>/portal/<slug>. Trimmed and lowercased.
func NormalizeSlug(v any) (string, error) {
	s, ok := v.(string)
	if !ok || jsstr.Trim(s) == "" {
		return "", apierror.BadRequest("portal slug is required", "portal_slug_required", nil)
	}
	s = jsstr.Trim(s)
	bad := apierror.BadRequest("portal slug must be one url-safe segment (a-z 0-9 - _)", "bad_portal_slug", nil)
	// JavaScript lowercases U+0130 to "i̇" (two code points), which the pattern
	// then rejects; Go's simple case mapping would make it a plain "i".
	if strings.ContainsRune(s, 'İ') {
		return "", bad
	}
	s = strings.Map(unicode.ToLower, s)
	if !portalSlugRE.MatchString(s) {
		return "", bad
	}
	if len(s) > maxSlugLength {
		return "", apierror.BadRequest(fmt.Sprintf("portal slug must be %d characters or fewer", maxSlugLength), "bad_portal_slug", nil)
	}
	return s, nil
}

var controlRE = regexp.MustCompile("[\u0000-\u001f\u007f]")

// collapse is JavaScript's value.trim().replace(/\s+/g, " ").
func collapse(s string) string { return strings.Join(strings.FieldsFunc(s, jsstr.IsSpace), " ") }

// NormalizeName trims a portal name and collapses internal whitespace. 400
// portal_name_required / bad_portal_name / portal_name_too_long.
func NormalizeName(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", apierror.BadRequest("portal name is required", "portal_name_required", nil)
	}
	if controlRE.MatchString(s) {
		return "", apierror.BadRequest("portal name contains unsupported control characters", "bad_portal_name", nil)
	}
	name := collapse(s)
	if name == "" {
		return "", apierror.BadRequest("portal name is required", "portal_name_required", nil)
	}
	if jsstr.UTF16Len(name) > maxNameLength {
		return "", apierror.BadRequest(fmt.Sprintf("portal name must be %d characters or fewer", maxNameLength), "portal_name_too_long", nil)
	}
	return name, nil
}

func isNullish(v any) bool { return v == nil || v == versions.Null }

// NormalizeLabel validates the partner-facing title for one membership.
// nil/Null/"" (and a blank string) clear it — nil result — which means "fall
// back to pages.title". 400 bad_portal_label otherwise.
func NormalizeLabel(v any) (*string, error) {
	if isNullish(v) || v == "" {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, apierror.BadRequest("portal label must be a string or null", "bad_portal_label", nil)
	}
	if controlRE.MatchString(s) {
		return nil, apierror.BadRequest("portal label contains unsupported control characters", "bad_portal_label", nil)
	}
	label := collapse(s)
	if label == "" {
		return nil, nil
	}
	if jsstr.UTF16Len(label) > maxLabelLength {
		return nil, apierror.BadRequest(fmt.Sprintf("portal label must be %d characters or fewer", maxLabelLength), "bad_portal_label", nil)
	}
	return &label, nil
}

var digitsRE = regexp.MustCompile(`^[0-9]+$`)

// NormalizeSortOrder validates a membership's order: nil/Null/"" ⇒ 0, else an
// integer (or a decimal string) between 0 and MaxSortOrder (400
// bad_portal_sort_order).
func NormalizeSortOrder(v any) (int, error) {
	if isNullish(v) || v == "" {
		return 0, nil
	}
	var n float64
	ok := false
	switch x := v.(type) {
	case string:
		if digitsRE.MatchString(x) {
			n, ok = jsstr.Number(x), true
		}
	case float64:
		n, ok = x, true
	case int:
		n, ok = float64(x), true
	case int64:
		n, ok = float64(x), true
	}
	if !ok || n != math.Trunc(n) || math.IsInf(n, 0) || n < 0 || n > MaxSortOrder {
		return 0, apierror.BadRequest(fmt.Sprintf("sort_order must be an integer between 0 and %d", MaxSortOrder), "bad_portal_sort_order", nil)
	}
	return int(n), nil
}

// GeneratePassword is the default credential, because this one secret opens
// every dashboard in the portal and a human-chosen one would be the weakest
// part of the feature. Grouped ("abcd-efgh-jkmn-pqrs") so it can be read aloud
// and typed once.
func GeneratePassword() (string, error) {
	groups := make([]string, 0, passwordGroups)
	limit := big.NewInt(int64(len(passwordAlphabet)))
	for g := 0; g < passwordGroups; g++ {
		var b strings.Builder
		for i := 0; i < passwordGroupLength; i++ {
			n, err := rand.Int(rand.Reader, limit)
			if err != nil {
				return "", err
			}
			b.WriteByte(passwordAlphabet[n.Int64()])
		}
		groups = append(groups, b.String())
	}
	return strings.Join(groups, "-"), nil
}

// AssertPasswordAcceptable checks a supplied password; it is never silently
// repaired: trimming one would change the secret behind the admin's back, and
// the copy-paste-a-newline case is far better as an error at set time than as
// "the password doesn't work". 400 bad_portal_password /
// portal_password_too_weak / portal_password_too_long.
func AssertPasswordAcceptable(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", apierror.BadRequest("portal password must be a string", "bad_portal_password", nil)
	}
	if controlRE.MatchString(s) {
		return "", apierror.BadRequest("portal password contains unsupported control characters", "bad_portal_password", nil)
	}
	if s != jsstr.Trim(s) {
		return "", apierror.BadRequest("portal password must not begin or end with whitespace", "bad_portal_password", nil)
	}
	n := jsstr.UTF16Len(s)
	if n < MinPasswordLength {
		return "", apierror.BadRequest(
			fmt.Sprintf("portal password must be at least %d characters — omit it and Pages generates a strong one", MinPasswordLength),
			"portal_password_too_weak", nil)
	}
	if n > maxPasswordLength {
		return "", apierror.BadRequest(fmt.Sprintf("portal password must be %d characters or fewer", maxPasswordLength), "portal_password_too_long", nil)
	}
	return s, nil
}

// secretFor is the shared create/setPassword step: generate when omitted,
// validate otherwise.
func secretFor(password any) (secret string, generated bool, err error) {
	if isNullish(password) || password == "" {
		s, err := GeneratePassword()
		return s, true, err
	}
	s, err := AssertPasswordAcceptable(password)
	return s, false, err
}

// memberTarget addresses one membership either by the page's slug (the normal
// path) or by its database id. The id path exists for a membership row whose
// page has since been soft-deleted: its slug no longer resolves — and may even
// belong to a different, newer page — so without it that row could never be
// removed without hand-written SQL.
type memberTarget struct {
	slug   string
	pageID int64
}

func normalizeMemberTarget(slug, pageID any) (memberTarget, error) {
	hasSlug := !isNullish(slug) && slug != ""
	hasID := !isNullish(pageID) && pageID != ""
	switch {
	case hasSlug && hasID:
		return memberTarget{}, apierror.BadRequest("pass either slug or page_id, not both", "bad_portal_member_target", nil)
	case hasSlug:
		s, err := versions.NormalizeSlug(slug)
		return memberTarget{slug: s}, err
	case hasID:
		id, err := NormalizeID(pageID, "page_id")
		return memberTarget{pageID: id}, err
	}
	return memberTarget{}, apierror.BadRequest("a page slug or page_id is required", "portal_member_target_required", nil)
}
